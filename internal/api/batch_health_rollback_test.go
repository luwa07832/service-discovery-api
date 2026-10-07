package api

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

// This file pins the published batch health-update contract: when a real
// SQLite write fails in the middle of a batch — the first update already
// applied inside the transaction, the failing one aborts it, and a later
// valid entry is never reached — the whole batch rolls back. The failed
// request answers HTTP 503 with only the top-level storage_unavailable
// error object, every target keeps its pre-request record field by field,
// and the identical request succeeds once the fault clears. Every check
// runs against a real SQLite file and is repeated after closing and
// reopening the database. Fixed timestamps and freshly seeded records
// keep the result independent of the wall clock and other tests.

func TestBatchUpdateHealthMidBatchFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch-health-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })

	// Three targets the batch flips, one same-service instance the batch
	// never names, one already-lost record a health update must not clean
	// up, and the same instance id under a second service.
	seeds := []map[string]any{
		{"service_name": "svc", "instance_id": "i-1", "address": "10.0.1.1:9001",
			"port": 9001, "healthy": true, "weight": 11, "heartbeat_at": at(0)},
		{"service_name": "svc", "instance_id": "i-2", "address": "10.0.1.2:9002",
			"port": 9002, "healthy": false, "weight": 7, "heartbeat_at": at(1)},
		{"service_name": "svc", "instance_id": "i-3", "address": "10.0.1.3:9003",
			"port": 9003, "healthy": true, "weight": 3, "heartbeat_at": at(2)},
		{"service_name": "svc", "instance_id": "untouched", "address": "10.0.1.4:9004",
			"port": 9004, "healthy": false, "weight": 4, "heartbeat_at": at(3)},
		{"service_name": "svc", "instance_id": "stale", "address": "10.0.1.5:9005",
			"port": 9005, "healthy": true, "weight": 5, "heartbeat_at": at(-120)},
		{"service_name": "peer", "instance_id": "i-2", "address": "10.0.2.2:9102",
			"port": 9102, "healthy": true, "weight": 6, "heartbeat_at": at(4)},
	}
	for _, body := range seeds {
		registerInstance(t, router, body)
	}

	wantI1 := map[string]any{"service_name": "svc", "instance_id": "i-1",
		"address": "10.0.1.1:9001", "port": float64(9001), "healthy": true,
		"weight": float64(11), "heartbeat_at": at(0)}
	wantI2 := map[string]any{"service_name": "svc", "instance_id": "i-2",
		"address": "10.0.1.2:9002", "port": float64(9002), "healthy": false,
		"weight": float64(7), "heartbeat_at": at(1)}
	wantI3 := map[string]any{"service_name": "svc", "instance_id": "i-3",
		"address": "10.0.1.3:9003", "port": float64(9003), "healthy": true,
		"weight": float64(3), "heartbeat_at": at(2)}
	wantUntouched := map[string]any{"service_name": "svc", "instance_id": "untouched",
		"address": "10.0.1.4:9004", "port": float64(9004), "healthy": false,
		"weight": float64(4), "heartbeat_at": at(3)}
	wantStale := map[string]any{"service_name": "svc", "instance_id": "stale",
		"address": "10.0.1.5:9005", "port": float64(9005), "healthy": true,
		"weight": float64(5), "heartbeat_at": at(-120)}
	wantPeer := map[string]any{"service_name": "peer", "instance_id": "i-2",
		"address": "10.0.2.2:9102", "port": float64(9102), "healthy": true,
		"weight": float64(6), "heartbeat_at": at(4)}

	// Each update flips the healthy flag to the opposite of its seed
	// value. The failing entry is second: i-1 is written first and i-3,
	// still valid, is never reached once storage aborts on i-2.
	body := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-1", "healthy": false},
		map[string]any{"instance_id": "i-2", "healthy": true},
		map[string]any{"instance_id": "i-3", "healthy": false},
	}}
	target := "/api/v1/services/svc/instances/health"
	entry := "PUT /api/v1/services/svc/instances/health"

	// assertRolledBack pins the state every failed attempt must leave:
	// address, port, weight, heartbeat time and the locating fields of
	// every target are unchanged field by field, the unsubmitted same-
	// service instance, the other service's same-named instance and the
	// already-lost record all keep their seed values, and the health
	// batch never performs lost-record cleanup.
	assertRolledBack := func(phase string) {
		t.Helper()
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/i-1", "svc", "i-1",
			getFullRecord(t, router, "svc", "i-1"), wantI1)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/i-2", "svc", "i-2",
			getFullRecord(t, router, "svc", "i-2"), wantI2)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/i-3", "svc", "i-3",
			getFullRecord(t, router, "svc", "i-3"), wantI3)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/untouched", "svc", "untouched",
			getFullRecord(t, router, "svc", "untouched"), wantUntouched)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/stale", "svc", "stale",
			getFullRecord(t, router, "svc", "stale"), wantStale)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/i-2", "peer", "i-2",
			getFullRecord(t, router, "peer", "i-2"), wantPeer)
		assertHealthView(t, router, phase, "svc", "i-1", true)
		assertHealthView(t, router, phase, "svc", "i-2", false)
		assertHealthView(t, router, phase, "svc", "i-3", true)
	}

	// assertCommitted pins the state the successful retry must leave:
	// only healthy moved, every other field keeps its value, and the
	// out-of-batch records stay on their seed values.
	wantI1After := map[string]any{}
	for k, v := range wantI1 {
		wantI1After[k] = v
	}
	wantI1After["healthy"] = false
	wantI2After := map[string]any{}
	for k, v := range wantI2 {
		wantI2After[k] = v
	}
	wantI2After["healthy"] = true
	wantI3After := map[string]any{}
	for k, v := range wantI3 {
		wantI3After[k] = v
	}
	wantI3After["healthy"] = false

	assertCommitted := func(phase string) {
		t.Helper()
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/i-1", "svc", "i-1",
			getFullRecord(t, router, "svc", "i-1"), wantI1After)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/i-2", "svc", "i-2",
			getFullRecord(t, router, "svc", "i-2"), wantI2After)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/i-3", "svc", "i-3",
			getFullRecord(t, router, "svc", "i-3"), wantI3After)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/untouched", "svc", "untouched",
			getFullRecord(t, router, "svc", "untouched"), wantUntouched)
		expectRecordFields(t, phase+" GET /api/v1/services/svc/instances/stale", "svc", "stale",
			getFullRecord(t, router, "svc", "stale"), wantStale)
		expectRecordFields(t, phase+" GET /api/v1/services/peer/instances/i-2", "peer", "i-2",
			getFullRecord(t, router, "peer", "i-2"), wantPeer)
		assertHealthView(t, router, phase, "svc", "i-1", false)
		assertHealthView(t, router, phase, "svc", "i-2", true)
		assertHealthView(t, router, phase, "svc", "i-3", false)
	}

	clearFault := injectBatchWriteFault(t, path, "i-2")

	// The failed request is repeatable: each attempt returns the same 503
	// error shape and leaves the same fully rolled-back state.
	for attempt := 1; attempt <= 2; attempt++ {
		rec := doRequest(t, router, http.MethodPut, target, body)
		expectOnlyTopLevelError(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
		assertRolledBack(entry + " attempt-" + strconv.Itoa(attempt))
	}

	// The rollback survives closing and reopening the same database file.
	router, st = reopenRouterAtPath(t, path, st)
	assertRolledBack(entry + " after-reopen")

	// With the fault cleared, the identical request applies the whole
	// batch: updated is 3 and the complete records come back in strict
	// request order with only healthy changed to the requested values.
	clearFault()
	rec := doRequest(t, router, http.MethodPut, target, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s retry: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if updated, _ := out["updated"].(float64); updated != 3 {
		t.Fatalf("%s retry: updated = %v, want 3", entry, out["updated"])
	}
	results := out["instances"].([]any)
	wantAfter := []map[string]any{wantI1After, wantI2After, wantI3After}
	if len(results) != 3 {
		t.Fatalf("%s retry: instances len = %d, want 3", entry, len(results))
	}
	for i, raw := range results {
		result := raw.(map[string]any)
		wantID := body["updates"].([]any)[i].(map[string]any)["instance_id"].(string)
		if result["instance_id"] != wantID {
			t.Fatalf("%s retry: instances[%d] instance_id = %v, want %v (request order)",
				entry, i, result["instance_id"], wantID)
		}
		expectRecordFields(t, entry+" retry instances response", "svc", wantID, result, wantAfter[i])
	}

	// The health attribute query agrees with the full-record query.
	assertCommitted(entry + " after-retry")

	// The committed batch survives closing and reopening the database,
	// and the health views still agree with the full records afterwards.
	router, st = reopenRouterAtPath(t, path, st)
	assertCommitted(entry + " after-retry-reopen")
}

// assertHealthView reads the public health attribute entry and checks
// the boolean healthy flag and the health string against the full-record
// view, so the two public queries can never disagree after a batch.
func assertHealthView(t *testing.T, router http.Handler, phase, service, id string, healthy bool) {
	t.Helper()
	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+service+"/instances/"+id+"/health", nil))
	health := "unhealthy"
	if healthy {
		health = "healthy"
	}
	if out["healthy"] != healthy || out["health"] != health {
		t.Fatalf("%s GET /api/v1/services/%s/instances/%s/health = %v (%v), want %v (%s)",
			phase, service, id, out["healthy"], out["health"], healthy, health)
	}
}
