package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

// This file pins the published batch health-update contract: when the
// storage layer fails in the middle of a batch — the first update already
// written inside the batch transaction, the second hitting a real SQLite
// write error, the third never reached — the whole batch rolls back. The
// failed request answers HTTP 503 with only the top-level
// storage_unavailable error object, every target keeps its pre-request
// values field by field, and the identical request succeeds once the fault
// clears. Every check runs against a real SQLite file and is repeated after
// closing and reopening the database.

// expectHealthQuery reads one instance through the public health query entry
// and pins the healthy flag and the derived health word.
func expectHealthQuery(t *testing.T, entry string, router http.Handler, service, id string, wantHealthy bool) {
	t.Helper()
	target := "/api/v1/services/" + service + "/instances/" + id + "/health"
	rec := doRequest(t, router, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s GET %s: status %d body %s", entry, target, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	wantHealth := "unhealthy"
	if wantHealthy {
		wantHealth = "healthy"
	}
	if out["service_name"] != service || out["instance_id"] != id ||
		out["healthy"] != wantHealthy || out["health"] != wantHealth {
		t.Errorf("%s GET %s service=%s instance=%s field healthy = %v (%v), want %v (%s)",
			entry, target, service, id, out["healthy"], out["health"], wantHealthy, wantHealth)
	}
}

func TestBatchUpdateHealthMidBatchFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch-health-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })

	// Seed records: the three batch targets h-1/h-2/h-3, one hsvc instance
	// the batch never names, one stale hsvc record whose heartbeat is long
	// past (a health update must never trigger lost cleanup), and an
	// instance with the same id as the first target under a second service.
	seeds := []map[string]any{
		{"service_name": "hsvc", "instance_id": "h-1", "address": "10.5.0.1:7101",
			"port": 7101, "healthy": true, "weight": 11, "heartbeat_at": at(1)},
		{"service_name": "hsvc", "instance_id": "h-2", "address": "10.5.0.2:7102",
			"port": 7102, "healthy": false, "weight": 12, "heartbeat_at": at(2)},
		{"service_name": "hsvc", "instance_id": "h-3", "address": "10.5.0.3:7103",
			"port": 7103, "healthy": true, "weight": 13, "heartbeat_at": at(3)},
		{"service_name": "hsvc", "instance_id": "h-keep", "address": "10.5.0.4:7104",
			"port": 7104, "healthy": false, "weight": 14, "heartbeat_at": at(4)},
		{"service_name": "hsvc", "instance_id": "h-stale", "address": "10.5.0.5:7105",
			"port": 7105, "healthy": true, "weight": 15, "heartbeat_at": at(0)},
		{"service_name": "peer", "instance_id": "h-1", "address": "10.6.0.1:7201",
			"port": 7201, "healthy": false, "weight": 21, "heartbeat_at": at(5)},
	}
	for _, body := range seeds {
		registerInstance(t, router, body)
	}
	wantSeedH1 := map[string]any{"service_name": "hsvc", "instance_id": "h-1",
		"address": "10.5.0.1:7101", "port": float64(7101), "healthy": true,
		"weight": float64(11), "heartbeat_at": at(1)}
	wantSeedH2 := map[string]any{"service_name": "hsvc", "instance_id": "h-2",
		"address": "10.5.0.2:7102", "port": float64(7102), "healthy": false,
		"weight": float64(12), "heartbeat_at": at(2)}
	wantSeedH3 := map[string]any{"service_name": "hsvc", "instance_id": "h-3",
		"address": "10.5.0.3:7103", "port": float64(7103), "healthy": true,
		"weight": float64(13), "heartbeat_at": at(3)}
	wantSeedKeep := map[string]any{"service_name": "hsvc", "instance_id": "h-keep",
		"address": "10.5.0.4:7104", "port": float64(7104), "healthy": false,
		"weight": float64(14), "heartbeat_at": at(4)}
	wantSeedStale := map[string]any{"service_name": "hsvc", "instance_id": "h-stale",
		"address": "10.5.0.5:7105", "port": float64(7105), "healthy": true,
		"weight": float64(15), "heartbeat_at": at(0)}
	wantSeedPeer := map[string]any{"service_name": "peer", "instance_id": "h-1",
		"address": "10.6.0.1:7201", "port": float64(7201), "healthy": false,
		"weight": float64(21), "heartbeat_at": at(5)}

	// The batch flips every target's healthy flag. The failing entry sits in
	// the middle: h-1 is already written when storage fails on h-2 and the
	// valid h-3 entry is never reached.
	const entry = "PUT /api/v1/services/hsvc/instances/health"
	const target = "/api/v1/services/hsvc/instances/health"
	body := map[string]any{"updates": []any{
		map[string]any{"instance_id": "h-1", "healthy": false},
		map[string]any{"instance_id": "h-2", "healthy": true},
		map[string]any{"instance_id": "h-3", "healthy": false},
	}}

	// The applied batch changes only the healthy flag of each target.
	wantAppliedH1 := map[string]any{"service_name": "hsvc", "instance_id": "h-1",
		"address": "10.5.0.1:7101", "port": float64(7101), "healthy": false,
		"weight": float64(11), "heartbeat_at": at(1)}
	wantAppliedH2 := map[string]any{"service_name": "hsvc", "instance_id": "h-2",
		"address": "10.5.0.2:7102", "port": float64(7102), "healthy": true,
		"weight": float64(12), "heartbeat_at": at(2)}
	wantAppliedH3 := map[string]any{"service_name": "hsvc", "instance_id": "h-3",
		"address": "10.5.0.3:7103", "port": float64(7103), "healthy": false,
		"weight": float64(13), "heartbeat_at": at(3)}

	// assertRolledBack pins the state every failed attempt must leave: all
	// three targets keep their seed values field by field in both the full
	// record and the health query, and no other record moved or was removed.
	assertRolledBack := func(phase string) {
		t.Helper()
		for id, want := range map[string]map[string]any{
			"h-1": wantSeedH1, "h-2": wantSeedH2, "h-3": wantSeedH3,
		} {
			expectRecordFields(t, phase+" GET /api/v1/services/hsvc/instances/"+id, "hsvc", id,
				getFullRecord(t, router, "hsvc", id), want)
			expectHealthQuery(t, phase, router, "hsvc", id, want["healthy"].(bool))
		}
		expectRecordFields(t, phase+" GET /api/v1/services/hsvc/instances/h-keep", "hsvc", "h-keep",
			getFullRecord(t, router, "hsvc", "h-keep"), wantSeedKeep)
		expectRecordFields(t, phase+" GET /api/v1/services/hsvc/instances/h-stale", "hsvc", "h-stale",
			getFullRecord(t, router, "hsvc", "h-stale"), wantSeedStale)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/h-1", "peer", "h-1",
			getFullRecord(t, router, "peer", "h-1"), wantSeedPeer)
		list := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/hsvc/instances", nil))
		expectInstanceIDSet(t, phase+" GET /api/v1/services/hsvc/instances", list,
			"h-1", "h-2", "h-3", "h-keep", "h-stale")
	}

	// assertApplied pins the state the successful retry must leave: the batch
	// applied in full with only healthy flipped, and every out-of-batch
	// record — including the stale one — unchanged.
	assertApplied := func(phase string) {
		t.Helper()
		for id, want := range map[string]map[string]any{
			"h-1": wantAppliedH1, "h-2": wantAppliedH2, "h-3": wantAppliedH3,
		} {
			expectRecordFields(t, phase+" GET /api/v1/services/hsvc/instances/"+id, "hsvc", id,
				getFullRecord(t, router, "hsvc", id), want)
			expectHealthQuery(t, phase, router, "hsvc", id, want["healthy"].(bool))
		}
		expectRecordFields(t, phase+" GET /api/v1/services/hsvc/instances/h-keep", "hsvc", "h-keep",
			getFullRecord(t, router, "hsvc", "h-keep"), wantSeedKeep)
		expectRecordFields(t, phase+" GET /api/v1/services/hsvc/instances/h-stale", "hsvc", "h-stale",
			getFullRecord(t, router, "hsvc", "h-stale"), wantSeedStale)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/h-1", "peer", "h-1",
			getFullRecord(t, router, "peer", "h-1"), wantSeedPeer)
		list := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/hsvc/instances", nil))
		expectInstanceIDSet(t, phase+" GET /api/v1/services/hsvc/instances", list,
			"h-1", "h-2", "h-3", "h-keep", "h-stale")
	}

	clearFault := injectBatchWriteFault(t, path, "h-2")

	// The failed request is repeatable: each attempt returns the same 503
	// error shape and leaves the same fully rolled-back state.
	for attempt := 1; attempt <= 2; attempt++ {
		rec := doRequest(t, router, http.MethodPut, target, body)
		expectOnlyTopLevelError(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
		assertRolledBack(fmt.Sprintf("%s attempt-%d", entry, attempt))
	}

	// The rollback survives closing and reopening the same database file.
	router, st = reopenRouterAtPath(t, path, st)
	assertRolledBack(entry + " after-reopen")

	// With the fault cleared, the identical request succeeds.
	clearFault()
	rec := doRequest(t, router, http.MethodPut, target, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s retry: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 3 {
		t.Fatalf("%s retry: updated = %v, want 3", entry, out["updated"])
	}
	results := out["instances"].([]any)
	if len(results) != 3 {
		t.Fatalf("%s retry: instances len = %d, want 3", entry, len(results))
	}
	wantByID := map[string]map[string]any{"h-1": wantAppliedH1, "h-2": wantAppliedH2, "h-3": wantAppliedH3}
	for i, wantID := range []string{"h-1", "h-2", "h-3"} {
		result := results[i].(map[string]any)
		if result["instance_id"] != wantID {
			t.Fatalf("%s retry: instances[%d] instance_id = %v, want %v (request order)",
				entry, i, result["instance_id"], wantID)
		}
		expectRecordFields(t, fmt.Sprintf("%s retry instances[%d]", entry, i), "hsvc", wantID, result, wantByID[wantID])
	}

	assertApplied(entry + " after-retry")

	// The committed batch survives closing and reopening the database again.
	router, st = reopenRouterAtPath(t, path, st)
	assertApplied(entry + " after-retry-reopen")
}
