package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file pins the published batch-registration contract: when the storage
// layer fails in the middle of a batch — earlier entries already written,
// later ones not yet reached — the whole batch rolls back, both batch
// entries answer HTTP 503 with only the top-level storage_unavailable error
// object, and the identical request succeeds once the fault clears. Every
// check runs against a real SQLite file and is repeated after closing and
// reopening the database.

// injectBatchWriteFault installs SQLite triggers that abort any insert or
// update of the named instance id, so a batch write fails exactly when it
// reaches that entry: entries before it are already written inside the batch
// transaction, entries after it never run. Both trigger kinds are installed
// because an upsert of an existing row and an insert of a new row may take
// different trigger paths. The returned function drops the triggers,
// modelling the storage fault being cleared.
func injectBatchWriteFault(t *testing.T, path, instanceID string) (clear func()) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fault injector: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	quoted := "'" + strings.ReplaceAll(instanceID, "'", "''") + "'"
	for _, kind := range []string{"INSERT", "UPDATE"} {
		name := "batch_fail_" + strings.ToLower(kind)
		if _, err := db.Exec(fmt.Sprintf(
			`CREATE TRIGGER %s BEFORE %s ON service_instances
			WHEN NEW.instance_id = %s
			BEGIN SELECT RAISE(ABORT, 'injected batch write failure'); END`,
			name, kind, quoted)); err != nil {
			t.Fatalf("inject %s fault: %v", kind, err)
		}
	}
	return func() {
		for _, kind := range []string{"INSERT", "UPDATE"} {
			name := "batch_fail_" + strings.ToLower(kind)
			if _, err := db.Exec("DROP TRIGGER " + name); err != nil {
				t.Fatalf("clear %s fault: %v", kind, err)
			}
		}
	}
}

// expectInstanceIDSet compares the instance ids of a decoded list response
// with the wanted set, ignoring order.
func expectInstanceIDSet(t *testing.T, entry string, out map[string]any, want ...string) {
	t.Helper()
	got := instanceIDs(t, out)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if strings.Join(got, ",") != strings.Join(sortedWant, ",") {
		t.Fatalf("%s: instance ids = %v, want %v", entry, got, sortedWant)
	}
}

func TestBatchRegisterMidBatchFailureRollsBack(t *testing.T) {
	styles := []struct {
		name      string
		pathEntry bool
	}{
		{"path entry POST /api/v1/services/{serviceName}/instances/batch", true},
		{"register entry POST /api/v1/register/batch", false},
	}
	variants := []struct {
		name            string
		failOnOverwrite bool
	}{
		{"failing entry adds a new instance", false},
		{"failing entry overwrites an existing instance", true},
	}
	for _, style := range styles {
		for _, variant := range variants {
			t.Run(style.name+"/"+variant.name, func(t *testing.T) {
				runBatchRegisterMidBatchFailure(t, style.pathEntry, variant.failOnOverwrite)
			})
		}
	}
}

func runBatchRegisterMidBatchFailure(t *testing.T, pathEntry, failOnOverwrite bool) {
	path := filepath.Join(t.TempDir(), "batch-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })

	// Seed records: one svc instance the batch overwrites, one svc instance
	// the batch never names, one stale svc record that registration must not
	// clean up, and both batch instance ids under a second service.
	seeds := []map[string]any{
		{"service_name": "svc", "instance_id": "keep", "address": "10.0.1.1:9001",
			"port": 9001, "healthy": true, "weight": 11, "heartbeat_at": at(0)},
		{"service_name": "svc", "instance_id": "untouched", "address": "10.0.1.2:9002",
			"port": 9002, "healthy": false, "weight": 7, "heartbeat_at": at(1)},
		{"service_name": "svc", "instance_id": "stale", "address": "10.0.1.3:9003",
			"port": 9003, "healthy": true, "weight": 3, "heartbeat_at": at(0)},
		{"service_name": "peer", "instance_id": "keep", "address": "10.0.2.1:9101",
			"port": 9101, "healthy": false, "weight": 5, "heartbeat_at": at(2)},
		{"service_name": "peer", "instance_id": "fresh", "address": "10.0.2.2:9102",
			"port": 9102, "healthy": true, "weight": 6, "heartbeat_at": at(3)},
	}
	for _, body := range seeds {
		registerInstance(t, router, body)
	}
	wantSeedKeep := map[string]any{"service_name": "svc", "instance_id": "keep",
		"address": "10.0.1.1:9001", "port": float64(9001), "healthy": true,
		"weight": float64(11), "heartbeat_at": at(0)}
	wantSeedUntouched := map[string]any{"service_name": "svc", "instance_id": "untouched",
		"address": "10.0.1.2:9002", "port": float64(9002), "healthy": false,
		"weight": float64(7), "heartbeat_at": at(1)}
	wantSeedStale := map[string]any{"service_name": "svc", "instance_id": "stale",
		"address": "10.0.1.3:9003", "port": float64(9003), "healthy": true,
		"weight": float64(3), "heartbeat_at": at(0)}
	wantSeedPeerKeep := map[string]any{"service_name": "peer", "instance_id": "keep",
		"address": "10.0.2.1:9101", "port": float64(9101), "healthy": false,
		"weight": float64(5), "heartbeat_at": at(2)}
	wantSeedPeerFresh := map[string]any{"service_name": "peer", "instance_id": "fresh",
		"address": "10.0.2.2:9102", "port": float64(9102), "healthy": true,
		"weight": float64(6), "heartbeat_at": at(3)}

	// The batch mixes one overwrite (keep) and two new instances (fresh,
	// tail). The failing entry sits in the middle: one entry is already
	// written when storage fails and one valid entry follows the failure.
	batchKeep := map[string]any{"instance_id": "keep", "address": "10.0.9.1:7001",
		"port": 7001, "healthy": false, "weight": 21, "heartbeat_at": at(10)}
	batchFresh := map[string]any{"instance_id": "fresh", "address": "10.0.9.2:7002",
		"port": 7002, "healthy": true, "weight": 22, "heartbeat_at": at(11)}
	batchTail := map[string]any{"instance_id": "tail", "address": "10.0.9.3:7003",
		"port": 7003, "healthy": true, "weight": 23, "heartbeat_at": at(12)}
	wantBatchKeep := map[string]any{"service_name": "svc", "instance_id": "keep",
		"address": "10.0.9.1:7001", "port": float64(7001), "healthy": false,
		"weight": float64(21), "heartbeat_at": at(10)}
	wantBatchFresh := map[string]any{"service_name": "svc", "instance_id": "fresh",
		"address": "10.0.9.2:7002", "port": float64(7002), "healthy": true,
		"weight": float64(22), "heartbeat_at": at(11)}
	wantBatchTail := map[string]any{"service_name": "svc", "instance_id": "tail",
		"address": "10.0.9.3:7003", "port": float64(7003), "healthy": true,
		"weight": float64(23), "heartbeat_at": at(12)}

	orderedEntries := []map[string]any{batchKeep, batchFresh, batchTail}
	failID := "fresh"
	if failOnOverwrite {
		orderedEntries = []map[string]any{batchFresh, batchKeep, batchTail}
		failID = "keep"
	}
	instances := make([]any, 0, len(orderedEntries))
	for _, entry := range orderedEntries {
		instances = append(instances, entry)
	}
	body := map[string]any{"instances": instances}
	target := "/api/v1/register/batch"
	entry := "POST /api/v1/register/batch"
	if pathEntry {
		target = "/api/v1/services/svc/instances/batch"
		entry = "POST /api/v1/services/svc/instances/batch"
		// A conflicting body service name must be ignored: only the path
		// service is ever operated on.
		body["service_name"] = "rogue"
	} else {
		body["service_name"] = "svc"
	}

	// assertRolledBack pins the state every failed attempt must leave: the
	// overwrite target keeps its seed values field by field, the batch's new
	// instances do not exist, and no other record moved.
	assertRolledBack := func(phase string) {
		t.Helper()
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/keep", "svc", "keep",
			getFullRecord(t, router, "svc", "keep"), wantSeedKeep)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/untouched", "svc", "untouched",
			getFullRecord(t, router, "svc", "untouched"), wantSeedUntouched)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/stale", "svc", "stale",
			getFullRecord(t, router, "svc", "stale"), wantSeedStale)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/keep", "peer", "keep",
			getFullRecord(t, router, "peer", "keep"), wantSeedPeerKeep)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/fresh", "peer", "fresh",
			getFullRecord(t, router, "peer", "fresh"), wantSeedPeerFresh)
		for _, id := range []string{"fresh", "tail"} {
			rec := doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances/"+id, nil)
			expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")
		}
		list := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances", nil))
		expectInstanceIDSet(t, phase+" GET /api/v1/services/svc/instances", list, "keep", "stale", "untouched")
		if pathEntry {
			rogue := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/rogue/instances", nil))
			expectInstanceIDSet(t, phase+" GET /api/v1/services/rogue/instances", rogue)
		}
	}

	// assertFinal pins the state the successful retry must leave: the batch
	// applied in full, every out-of-batch record unchanged.
	assertFinal := func(phase string) {
		t.Helper()
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/keep", "svc", "keep",
			getFullRecord(t, router, "svc", "keep"), wantBatchKeep)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/fresh", "svc", "fresh",
			getFullRecord(t, router, "svc", "fresh"), wantBatchFresh)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/tail", "svc", "tail",
			getFullRecord(t, router, "svc", "tail"), wantBatchTail)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/untouched", "svc", "untouched",
			getFullRecord(t, router, "svc", "untouched"), wantSeedUntouched)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/stale", "svc", "stale",
			getFullRecord(t, router, "svc", "stale"), wantSeedStale)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/keep", "peer", "keep",
			getFullRecord(t, router, "peer", "keep"), wantSeedPeerKeep)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/fresh", "peer", "fresh",
			getFullRecord(t, router, "peer", "fresh"), wantSeedPeerFresh)
		list := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances", nil))
		expectInstanceIDSet(t, phase+" GET /api/v1/services/svc/instances", list,
			"fresh", "keep", "stale", "tail", "untouched")
		if pathEntry {
			rogue := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/rogue/instances", nil))
			expectInstanceIDSet(t, phase+" GET /api/v1/services/rogue/instances", rogue)
		}
	}

	clearFault := injectBatchWriteFault(t, path, failID)

	// The failed request is repeatable: each attempt returns the same 503
	// error shape and leaves the same fully rolled-back state.
	for attempt := 1; attempt <= 2; attempt++ {
		rec := doRequest(t, router, http.MethodPost, target, body)
		expectOnlyTopLevelError(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
		assertRolledBack(fmt.Sprintf("%s attempt-%d", entry, attempt))
	}

	// The rollback survives closing and reopening the same database file.
	router, st = reopenRouterAtPath(t, path, st)
	assertRolledBack(entry + " after-reopen")

	// With the fault cleared, the identical request succeeds.
	clearFault()
	rec := doRequest(t, router, http.MethodPost, target, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s retry: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "svc" {
		t.Fatalf("%s retry: service_name = %v, want svc", entry, out["service_name"])
	}
	if out["registered"].(float64) != float64(len(orderedEntries)) {
		t.Fatalf("%s retry: registered = %v, want %d", entry, out["registered"], len(orderedEntries))
	}
	results := out["instances"].([]any)
	if len(results) != len(orderedEntries) {
		t.Fatalf("%s retry: instances len = %d, want %d", entry, len(results), len(orderedEntries))
	}
	wantByID := map[string]map[string]any{"keep": wantBatchKeep, "fresh": wantBatchFresh, "tail": wantBatchTail}
	for i, raw := range results {
		result := raw.(map[string]any)
		wantID := orderedEntries[i]["instance_id"].(string)
		if result["instance_id"] != wantID {
			t.Fatalf("%s retry: instances[%d] instance_id = %v, want %v (request order)",
				entry, i, result["instance_id"], wantID)
		}
		expectRecordFields(t, fmt.Sprintf("%s retry instances[%d]", entry, i), "svc", wantID, result, wantByID[wantID])
	}

	assertFinal(entry + " after-retry")

	// The committed batch survives closing and reopening the database again.
	router, st = reopenRouterAtPath(t, path, st)
	assertFinal(entry + " after-retry-reopen")
}
