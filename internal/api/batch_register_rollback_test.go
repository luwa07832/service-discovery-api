package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setBatchAbortTrigger installs (or removes) a real SQLite trigger that
// aborts a write to an instance whose id is "boom". The trigger lives in
// the database file, so reopening the store keeps the injected failure in
// place until the test drops it. The trigger fires only after earlier
// statements of the same transaction have already run, reproducing a
// mid-batch storage failure without touching the product code.
func setBatchAbortTrigger(t *testing.T, path, event string, installed bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer db.Close()

	name := "batch_abort_" + strings.ToLower(event)
	if !installed {
		if _, err := db.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s", name)); err != nil {
			t.Fatalf("drop trigger %s: %v", name, err)
		}
		return
	}
	if _, err := db.Exec(fmt.Sprintf(`
CREATE TRIGGER %s
BEFORE %s ON service_instances
WHEN NEW.instance_id = 'boom'
BEGIN
	SELECT RAISE(ABORT, 'injected mid-batch storage failure');
END;`, name, event)); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}

// expectInstanceIDSet lists one service and compares the stored id set.
func expectInstanceIDSet(t *testing.T, router http.Handler, service string, want []string) {
	t.Helper()
	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+service+"/instances", nil))
	got := instanceIDs(t, out)
	counts := make(map[string]int, len(want))
	for _, id := range want {
		counts[id]++
	}
	if len(got) != len(want) {
		t.Fatalf("service=%s instance ids = %v, want %v", service, got, want)
	}
	for _, id := range got {
		counts[id]--
	}
	for _, remaining := range counts {
		if remaining != 0 {
			t.Fatalf("service=%s instance ids = %v, want %v", service, got, want)
		}
	}
}

// expectMissingInstance confirms a full-record query reports the published
// instance_not_found shape after an unsuccessful batch.
func expectMissingInstance(t *testing.T, router http.Handler, service, id string) {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+service+"/instances/"+id, nil)
	expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")
}

// TestBatchRegisterAtomicRollbackSurvivesReopen drives both batch entries
// with a real SQLite failure between an already-written item and a later
// valid item. Nothing written by the failed request may survive, before or
// after reopening the database; once the failure is cleared the same
// request retries successfully and the committed state survives another
// reopen.
func TestBatchRegisterAtomicRollbackSurvivesReopen(t *testing.T) {
	cases := []struct {
		name                string
		event               string // trigger event: INSERT or UPDATE
		seedBoom            bool
		missingAfterFailure []string
		failureList         []string
	}{
		{
			name:                "later new entry fails",
			event:               "INSERT",
			seedBoom:            false,
			missingAfterFailure: []string{"boom", "i-3"},
			failureList:         []string{"i-1", "lost"},
		},
		{
			name:                "later overwrite fails",
			event:               "UPDATE",
			seedBoom:            true,
			missingAfterFailure: []string{"i-3"},
			failureList:         []string{"boom", "i-1", "lost"},
		},
	}
	entryPoints := []struct {
		name             string
		target           string
		bodyServiceName  string
		unrelatedService string
	}{
		// A conflicting body service name must be ignored by the path entry.
		{"path entry", "/api/v1/services/svc/instances/batch", "other-svc", "other-svc"},
		{"body entry", "/api/v1/register/batch", "svc", "other-svc"},
	}
	for _, tc := range cases {
		for _, ep := range entryPoints {
			t.Run(tc.name+"/"+ep.name, func(t *testing.T) {
				runBatchRollbackScenario(t, tc.event, tc.seedBoom,
					tc.missingAfterFailure, tc.failureList, ep.target,
					ep.bodyServiceName, ep.unrelatedService)
			})
		}
	}
}

func runBatchRollbackScenario(t *testing.T, event string, seedBoom bool,
	missingAfterFailure, failureList []string, target, bodyServiceName, unrelatedService string) {

	path := filepath.Join(t.TempDir(), "rollback.db")
	router, st := openRouterAtPath(t, path)
	// st is rebound after every reopen; the cleanup closes the final handle.
	t.Cleanup(func() { st.Close() })

	// Pre-existing records: i-1 is overwritten by the batch, boom is only
	// seeded for the UPDATE-trigger case, lost is a stale record that the
	// registration request must never clean up, and the other service shares
	// instance id i-1.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.1:8080",
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(0),
	})
	if seedBoom {
		registerInstance(t, router, map[string]any{
			"service_name": "svc", "instance_id": "boom", "address": "10.0.0.3:8080",
			"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(1),
		})
	}
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "lost", "address": "10.0.0.4:8080",
		"port": 8080, "healthy": true, "weight": 3, "heartbeat_at": at(-120),
	})
	registerInstance(t, router, map[string]any{
		"service_name": unrelatedService, "instance_id": "i-1", "address": "10.0.0.2:8080",
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(0),
	})

	// One mixed batch: an overwrite first, the failing item in the middle
	// and one still-valid new item afterwards, so a successful response
	// would leave evidence of a later write.
	requestBody := map[string]any{
		"service_name": bodyServiceName,
		"instances": []any{
			map[string]any{
				"instance_id": "i-1", "address": "10.1.0.9:9090", "port": 9090,
				"healthy": false, "weight": 2.5, "heartbeat_at": at(30),
			},
			map[string]any{
				"instance_id": "boom", "address": "10.1.0.10:9091", "port": 9091,
				"healthy": true, "weight": 5, "heartbeat_at": at(31),
			},
			map[string]any{
				"instance_id": "i-3", "address": "10.1.0.11:9092", "port": 9092,
				"healthy": true, "weight": 7, "heartbeat_at": at(32),
			},
		},
	}

	seededI1 := map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.1:8080",
		"port": float64(8080), "healthy": true, "weight": float64(10), "heartbeat_at": at(0),
	}
	seededBoom := map[string]any{
		"service_name": "svc", "instance_id": "boom", "address": "10.0.0.3:8080",
		"port": float64(8080), "healthy": true, "weight": float64(10), "heartbeat_at": at(1),
	}
	seededLost := map[string]any{
		"service_name": "svc", "instance_id": "lost", "address": "10.0.0.4:8080",
		"port": float64(8080), "healthy": true, "weight": float64(3), "heartbeat_at": at(-120),
	}
	seededOtherI1 := map[string]any{
		"service_name": unrelatedService, "instance_id": "i-1", "address": "10.0.0.2:8080",
		"port": float64(8080), "healthy": true, "weight": float64(10), "heartbeat_at": at(0),
	}
	finalI1 := map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.1.0.9:9090",
		"port": float64(9090), "healthy": false, "weight": 2.5, "heartbeat_at": at(30),
	}
	finalBoom := map[string]any{
		"service_name": "svc", "instance_id": "boom", "address": "10.1.0.10:9091",
		"port": float64(9091), "healthy": true, "weight": float64(5), "heartbeat_at": at(31),
	}
	finalI3 := map[string]any{
		"service_name": "svc", "instance_id": "i-3", "address": "10.1.0.11:9092",
		"port": float64(9092), "healthy": true, "weight": float64(7), "heartbeat_at": at(32),
	}

	// Install the trigger in the persisted file, then reopen the store so
	// the request runs through the ordinary HTTP and storage code paths.
	if err := st.Close(); err != nil {
		t.Fatalf("close before trigger install: %v", err)
	}
	setBatchAbortTrigger(t, path, event, true)
	router, st = openRouterAtPath(t, path)

	failedRec := doRequest(t, router, http.MethodPost, target, requestBody)
	expectOnlyTopLevelError(t, failedRec, http.StatusServiceUnavailable, "storage_unavailable")

	assertRolledBack := func(phase string) {
		t.Helper()
		// The earlier overwrite must not have taken.
		expectRecordFields(t, phase+" svc/i-1", "svc", "i-1",
			getFullRecord(t, router, "svc", "i-1"), seededI1)
		// Every batch-created id is absent, including the item after the failure.
		for _, id := range missingAfterFailure {
			expectMissingInstance(t, router, "svc", id)
		}
		// The failing existing record (overwrite case) keeps every old field.
		if seedBoom {
			expectRecordFields(t, phase+" svc/boom", "svc", "boom",
				getFullRecord(t, router, "svc", "boom"), seededBoom)
		} else {
			expectMissingInstance(t, router, "svc", "boom")
		}
		// The stale record the registration never cleans is untouched.
		expectRecordFields(t, phase+" svc/lost", "svc", "lost",
			getFullRecord(t, router, "svc", "lost"), seededLost)
		expectInstanceIDSet(t, router, "svc", failureList)
		// Another service sharing an instance id submitted in the batch is
		// never read or written by either entry point.
		expectRecordFields(t, phase+" "+unrelatedService+"/i-1", unrelatedService, "i-1",
			getFullRecord(t, router, unrelatedService, "i-1"), seededOtherI1)
		expectMissingInstance(t, router, unrelatedService, "boom")
		expectMissingInstance(t, router, unrelatedService, "i-3")
		expectInstanceIDSet(t, router, unrelatedService, []string{"i-1"})
	}
	assertRolledBack("failure")

	// Closing and reopening the same database file must not expose any of
	// the rolled-back writes.
	router, st = reopenRouterAtPath(t, path, st)
	assertRolledBack("after-reopen")

	// Clear the storage failure: the identical request now succeeds.
	if err := st.Close(); err != nil {
		t.Fatalf("close before trigger removal: %v", err)
	}
	setBatchAbortTrigger(t, path, event, false)
	router, st = openRouterAtPath(t, path)

	rec := doRequest(t, router, http.MethodPost, target, requestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["registered"].(float64) != 3 {
		t.Fatalf("retry registered = %v, want 3", out["registered"])
	}
	entries := out["instances"].([]any)
	if len(entries) != 3 {
		t.Fatalf("retry instances len = %d, want 3", len(entries))
	}
	if ids := []string{
		entries[0].(map[string]any)["instance_id"].(string),
		entries[1].(map[string]any)["instance_id"].(string),
		entries[2].(map[string]any)["instance_id"].(string),
	}; ids[0] != "i-1" || ids[1] != "boom" || ids[2] != "i-3" {
		t.Fatalf("retry instances out of request order: %v", ids)
	}

	assertCommitted := func(phase string) {
		t.Helper()
		// The earlier instance now carries the whole-row overwrite values.
		expectRecordFields(t, phase+" svc/i-1", "svc", "i-1",
			getFullRecord(t, router, "svc", "i-1"), finalI1)
		expectRecordFields(t, phase+" svc/boom", "svc", "boom",
			getFullRecord(t, router, "svc", "boom"), finalBoom)
		expectRecordFields(t, phase+" svc/i-3", "svc", "i-3",
			getFullRecord(t, router, "svc", "i-3"), finalI3)
		// Registration still does not reap stale records.
		expectRecordFields(t, phase+" svc/lost", "svc", "lost",
			getFullRecord(t, router, "svc", "lost"), seededLost)
		expectInstanceIDSet(t, router, "svc", []string{"i-1", "boom", "i-3", "lost"})
		// The other service, including the id reused inside the batch, is
		// unchanged and never received the batch's new instances.
		expectRecordFields(t, phase+" "+unrelatedService+"/i-1", unrelatedService, "i-1",
			getFullRecord(t, router, unrelatedService, "i-1"), seededOtherI1)
		expectMissingInstance(t, router, unrelatedService, "boom")
		expectMissingInstance(t, router, unrelatedService, "i-3")
		expectInstanceIDSet(t, router, unrelatedService, []string{"i-1"})
	}
	assertCommitted("retry")

	// The committed retry survives another database reopen as well.
	router, st = reopenRouterAtPath(t, path, st)
	assertCommitted("after-reopen")
}
