package api

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// openRouterAtPath opens the SQLite file at path and wires a router to it.
// The caller owns closing the returned store.
func openRouterAtPath(t *testing.T, path string) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return NewRouter(st), st
}

// reopenRouterAtPath closes the current handle and opens a fresh one on the
// same database file, simulating a service restart against persisted state.
func reopenRouterAtPath(t *testing.T, path string, st *store.Store) (http.Handler, *store.Store) {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}
	return openRouterAtPath(t, path)
}

// expectRecordFields compares the named fields of one decoded instance record
// and reports the request entry, service, instance and mismatched field.
func expectRecordFields(t *testing.T, entry, service, id string, got map[string]any, want map[string]any) {
	t.Helper()
	for field, wantValue := range want {
		if got[field] != wantValue {
			t.Errorf("%s service=%s instance=%s field %s = %v, want %v (record %v)",
				entry, service, id, field, got[field], wantValue, got)
		}
	}
}

// getFullRecord reads one instance through the public full-record query entry.
func getFullRecord(t *testing.T, router http.Handler, service, id string) map[string]any {
	t.Helper()
	target := "/api/v1/services/" + service + "/instances/" + id
	rec := doRequest(t, router, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d body %s", target, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)["instance"].(map[string]any)
}

func TestReopenReadsLatestStateAfterTargetedUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })

	// Two services share one instance id but carry different field values.
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "10.0.0.8:8080",
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-2", "address": "10.0.0.9:8080",
		"port": 8081, "healthy": true, "weight": 5, "heartbeat_at": at(1),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-1", "address": "10.0.9.9:9000",
		"port": 9000, "healthy": false, "weight": 3, "heartbeat_at": at(2),
	})

	// Batch heartbeat renewal on alpha: full records come back in request
	// order and only heartbeat_at changes.
	const batchHeartbeatEntry = "POST /api/v1/heartbeat"
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "alpha",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "i-2", "heartbeat_at": at(21)},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s service=alpha: status %d body %s", batchHeartbeatEntry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("%s service=alpha: updated = %v, want 2", batchHeartbeatEntry, out["updated"])
	}
	entries := out["instances"].([]any)
	if len(entries) != 2 {
		t.Fatalf("%s service=alpha: instances len = %d, want 2", batchHeartbeatEntry, len(entries))
	}
	first := entries[0].(map[string]any)
	second := entries[1].(map[string]any)
	if first["instance_id"] != "i-1" || second["instance_id"] != "i-2" {
		t.Fatalf("%s service=alpha: request order lost: %v then %v",
			batchHeartbeatEntry, first["instance_id"], second["instance_id"])
	}
	expectRecordFields(t, batchHeartbeatEntry, "alpha", "i-1", first, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "10.0.0.8:8080",
		"port": float64(8080), "healthy": true, "weight": float64(10), "heartbeat_at": at(20),
	})
	expectRecordFields(t, batchHeartbeatEntry, "alpha", "i-2", second, map[string]any{
		"service_name": "alpha", "instance_id": "i-2", "address": "10.0.0.9:8080",
		"port": float64(8081), "healthy": true, "weight": float64(5), "heartbeat_at": at(21),
	})

	// Single health update on alpha/i-1 changes only the healthy flag.
	const healthEntry = "PUT /api/v1/services/alpha/instances/i-1/health"
	rec = doRequest(t, router, http.MethodPut, "/api/v1/services/alpha/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", healthEntry, rec.Code, rec.Body.String())
	}
	expectRecordFields(t, healthEntry, "alpha", "i-1", decodeBody(t, rec)["instance"].(map[string]any), map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "10.0.0.8:8080",
		"port": float64(8080), "healthy": false, "weight": float64(10), "heartbeat_at": at(20),
	})

	// Single weight update on alpha/i-1 changes only the weight.
	const weightEntry = "PUT /api/v1/services/alpha/instances/i-1/weight"
	rec = doRequest(t, router, http.MethodPut, "/api/v1/services/alpha/instances/i-1/weight",
		map[string]any{"weight": 42})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", weightEntry, rec.Code, rec.Body.String())
	}
	expectRecordFields(t, weightEntry, "alpha", "i-1", decodeBody(t, rec)["instance"].(map[string]any), map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "10.0.0.8:8080",
		"port": float64(8080), "healthy": false, "weight": float64(42), "heartbeat_at": at(20),
	})

	// Reopen the same SQLite file: every later read sees the latest values.
	router, st = reopenRouterAtPath(t, path, st)

	const getEntry = "GET /api/v1/services/alpha/instances/i-1"
	expectRecordFields(t, getEntry, "alpha", "i-1", getFullRecord(t, router, "alpha", "i-1"), map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "10.0.0.8:8080",
		"port": float64(8080), "healthy": false, "weight": float64(42), "heartbeat_at": at(20),
	})

	// Attribute queries agree with the reopened full record.
	health := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances/i-1/health", nil))
	if health["healthy"] != false || health["health"] != "unhealthy" {
		t.Errorf("GET /api/v1/services/alpha/instances/i-1/health service=alpha instance=i-1 field healthy = %v (%v), want false (unhealthy)",
			health["healthy"], health["health"])
	}
	weight := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances/i-1/weight", nil))
	if weight["weight"] != float64(42) {
		t.Errorf("GET /api/v1/services/alpha/instances/i-1/weight service=alpha instance=i-1 field weight = %v, want 42",
			weight["weight"])
	}
	heartbeat := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances/i-1/heartbeat", nil))
	if heartbeat["heartbeat_at"] != at(20) {
		t.Errorf("GET /api/v1/services/alpha/instances/i-1/heartbeat service=alpha instance=i-1 field heartbeat_at = %v, want %s",
			heartbeat["heartbeat_at"], at(20))
	}

	// The instance the health and weight updates never touched keeps every
	// field except the batch-renewed heartbeat.
	expectRecordFields(t, "GET /api/v1/services/alpha/instances/i-2", "alpha", "i-2",
		getFullRecord(t, router, "alpha", "i-2"), map[string]any{
			"service_name": "alpha", "instance_id": "i-2", "address": "10.0.0.9:8080",
			"port": float64(8081), "healthy": true, "weight": float64(5), "heartbeat_at": at(21),
		})

	// The other service with the same instance id is completely unaffected.
	expectRecordFields(t, "GET /api/v1/services/beta/instances/i-1", "beta", "i-1",
		getFullRecord(t, router, "beta", "i-1"), map[string]any{
			"service_name": "beta", "instance_id": "i-1", "address": "10.0.9.9:9000",
			"port": float64(9000), "healthy": false, "weight": float64(3), "heartbeat_at": at(2),
		})
}

func TestReopenPreservesRollbackStateAfterFailedRenewal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })

	registerInstance(t, router, map[string]any{
		"service_name": "gamma", "instance_id": "g-1", "address": "10.1.0.1:7001",
		"port": 7001, "healthy": true, "weight": 11, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "gamma", "instance_id": "g-2", "address": "10.1.0.2:7002",
		"port": 7002, "healthy": false, "weight": 7, "heartbeat_at": at(1),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "delta", "instance_id": "g-1", "address": "10.2.0.1:8001",
		"port": 8001, "healthy": true, "weight": 2, "heartbeat_at": at(2),
	})

	const batchHeartbeatEntry = "POST /api/v1/heartbeat"

	// Existing instance first, missing instance last: 404 and a full rollback.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "gamma",
		"instances": []any{
			map[string]any{"instance_id": "g-1", "heartbeat_at": at(30)},
			map[string]any{"instance_id": "ghost", "heartbeat_at": at(31)},
		},
	})
	expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")

	// Later entry missing heartbeat_at: 400, again nothing may change.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "gamma",
		"instances": []any{
			map[string]any{"instance_id": "g-1", "heartbeat_at": at(30)},
			map[string]any{"instance_id": "g-2"},
		},
	})
	expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")

	// Neither failed request updated the leading entries, created the missing
	// instance or touched the other service — before or after the reopen.
	assertRolledBack := func(phase string) {
		t.Helper()
		expectRecordFields(t, phase+" GET /api/v1/services/gamma/instances/g-1", "gamma", "g-1",
			getFullRecord(t, router, "gamma", "g-1"), map[string]any{
				"service_name": "gamma", "instance_id": "g-1", "address": "10.1.0.1:7001",
				"port": float64(7001), "healthy": true, "weight": float64(11), "heartbeat_at": at(0),
			})
		expectRecordFields(t, phase+" GET /api/v1/services/gamma/instances/g-2", "gamma", "g-2",
			getFullRecord(t, router, "gamma", "g-2"), map[string]any{
				"service_name": "gamma", "instance_id": "g-2", "address": "10.1.0.2:7002",
				"port": float64(7002), "healthy": false, "weight": float64(7), "heartbeat_at": at(1),
			})
		rec := doRequest(t, router, http.MethodGet, "/api/v1/services/gamma/instances/ghost", nil)
		expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")
		expectRecordFields(t, phase+" GET /api/v1/services/delta/instances/g-1", "delta", "g-1",
			getFullRecord(t, router, "delta", "g-1"), map[string]any{
				"service_name": "delta", "instance_id": "g-1", "address": "10.2.0.1:8001",
				"port": float64(8001), "healthy": true, "weight": float64(2), "heartbeat_at": at(2),
			})
	}
	assertRolledBack("before-reopen")

	router, st = reopenRouterAtPath(t, path, st)
	assertRolledBack("after-reopen")

	// A valid renewal after the failures succeeds and survives another reopen.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "gamma",
		"instances": []any{
			map[string]any{"instance_id": "g-1", "heartbeat_at": at(40)},
			map[string]any{"instance_id": "g-2", "heartbeat_at": at(41)},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s service=gamma: status %d body %s", batchHeartbeatEntry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("%s service=gamma: updated = %v, want 2", batchHeartbeatEntry, out["updated"])
	}

	router, st = reopenRouterAtPath(t, path, st)
	expectRecordFields(t, "GET /api/v1/services/gamma/instances/g-1", "gamma", "g-1",
		getFullRecord(t, router, "gamma", "g-1"), map[string]any{
			"service_name": "gamma", "instance_id": "g-1", "address": "10.1.0.1:7001",
			"port": float64(7001), "healthy": true, "weight": float64(11), "heartbeat_at": at(40),
		})
	heartbeat := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/gamma/instances/g-2/heartbeat", nil))
	if heartbeat["heartbeat_at"] != at(41) {
		t.Errorf("GET /api/v1/services/gamma/instances/g-2/heartbeat service=gamma instance=g-2 field heartbeat_at = %v, want %s",
			heartbeat["heartbeat_at"], at(41))
	}
}

func TestReopenDistinguishesDiscoveryDeletionFromReadOnlyQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })

	// Fixed evaluation point and timeout keep every verdict independent of
	// the wall clock: with evaluate_at 12:10 and timeout 5m the lost
	// boundary is exactly 12:05.
	registrations := []map[string]any{
		{"service_name": "ro-svc", "instance_id": "online-healthy", "address": "10.3.0.1:9101",
			"port": 9101, "healthy": true, "weight": 10, "heartbeat_at": at(9)},
		{"service_name": "ro-svc", "instance_id": "boundary", "address": "10.3.0.2:9102",
			"port": 9102, "healthy": true, "weight": 10, "heartbeat_at": at(5)},
		{"service_name": "ro-svc", "instance_id": "online-low", "address": "10.3.0.3:9103",
			"port": 9103, "healthy": true, "weight": 1, "heartbeat_at": at(8)},
		{"service_name": "ro-svc", "instance_id": "unhealthy-fresh", "address": "10.3.0.4:9104",
			"port": 9104, "healthy": false, "weight": 8, "heartbeat_at": at(9)},
		{"service_name": "ro-svc", "instance_id": "lost", "address": "10.3.0.5:9105",
			"port": 9105, "healthy": true, "weight": 99, "heartbeat_at": at(4)},
		{"service_name": "peer-svc", "instance_id": "peer-lost", "address": "10.4.0.1:9201",
			"port": 9201, "healthy": true, "weight": 5, "heartbeat_at": at(0)},
	}
	for _, body := range registrations {
		registerInstance(t, router, body)
	}

	params := "evaluate_at=" + at(10) + "&heartbeat_timeout=5m"

	// The read-only overview counts every record by class and deletes nothing.
	const overviewEntry = "GET /api/v1/services"
	rec := doRequest(t, router, http.MethodGet, "/api/v1/services?"+params, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", overviewEntry, rec.Code, rec.Body.String())
	}
	services := decodeBody(t, rec)["services"].([]any)
	counters := make(map[string]map[string]any, len(services))
	for _, raw := range services {
		item := raw.(map[string]any)
		counters[item["service_name"].(string)] = item
	}
	for service, want := range map[string]map[string]float64{
		"ro-svc":   {"total_instances": 5, "available_instances": 3, "unhealthy_fresh_instances": 1, "lost_instances": 1},
		"peer-svc": {"total_instances": 1, "available_instances": 0, "unhealthy_fresh_instances": 0, "lost_instances": 1},
	} {
		item, ok := counters[service]
		if !ok {
			t.Fatalf("%s: service %s missing from overview %v", overviewEntry, service, services)
		}
		for field, wantValue := range want {
			if item[field] != wantValue {
				t.Errorf("%s service=%s field %s = %v, want %v", overviewEntry, service, field, item[field], wantValue)
			}
		}
	}

	// The instance list still holds the strictly lost record, and the lost
	// record stays queryable before any discovery runs.
	const listEntry = "GET /api/v1/services/ro-svc/instances"
	list := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/ro-svc/instances", nil))
	if ids := instanceIDs(t, list); len(ids) != 5 {
		t.Fatalf("%s service=ro-svc: ids = %v, want 5 records", listEntry, ids)
	}
	expectRecordFields(t, "GET /api/v1/services/ro-svc/instances/lost", "ro-svc", "lost",
		getFullRecord(t, router, "ro-svc", "lost"), map[string]any{
			"service_name": "ro-svc", "instance_id": "lost", "address": "10.3.0.5:9105",
			"port": float64(9105), "healthy": true, "weight": float64(99), "heartbeat_at": at(4),
		})

	// Discovery returns only healthy, non-lost instances: weight descending,
	// instance id ascending on ties. The boundary record is still online.
	const discoverEntry = "GET /api/v1/services/ro-svc/discover"
	discoverTarget := "/api/v1/services/ro-svc/discover?" + params
	rec = doRequest(t, router, http.MethodGet, discoverTarget, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", discoverEntry, rec.Code, rec.Body.String())
	}
	wantIDs := []string{"boundary", "online-healthy", "online-low"}
	if ids := instanceIDs(t, decodeBody(t, rec)); strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("%s service=ro-svc: ids = %v, want %v", discoverEntry, ids, wantIDs)
	}

	// Reopen: the lost record stays deleted, everything else survives.
	router, st = reopenRouterAtPath(t, path, st)

	rec = doRequest(t, router, http.MethodGet, "/api/v1/services/ro-svc/instances/lost", nil)
	expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")
	rec = doRequest(t, router, http.MethodGet, "/api/v1/services/ro-svc/instances/lost/heartbeat", nil)
	expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")

	list = decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/services/ro-svc/instances", nil))
	if ids := instanceIDs(t, list); len(ids) != 4 {
		t.Fatalf("%s service=ro-svc after reopen: ids = %v, want 4 records", listEntry, ids)
	}
	expectRecordFields(t, "GET /api/v1/services/ro-svc/instances/unhealthy-fresh", "ro-svc", "unhealthy-fresh",
		getFullRecord(t, router, "ro-svc", "unhealthy-fresh"), map[string]any{
			"service_name": "ro-svc", "instance_id": "unhealthy-fresh", "address": "10.3.0.4:9104",
			"port": float64(9104), "healthy": false, "weight": float64(8), "heartbeat_at": at(9),
		})
	expectRecordFields(t, "GET /api/v1/services/ro-svc/instances/boundary", "ro-svc", "boundary",
		getFullRecord(t, router, "ro-svc", "boundary"), map[string]any{
			"service_name": "ro-svc", "instance_id": "boundary", "address": "10.3.0.2:9102",
			"port": float64(9102), "healthy": true, "weight": float64(10), "heartbeat_at": at(5),
		})

	// Discovery on ro-svc never touched the other service's lost record.
	expectRecordFields(t, "GET /api/v1/services/peer-svc/instances/peer-lost", "peer-svc", "peer-lost",
		getFullRecord(t, router, "peer-svc", "peer-lost"), map[string]any{
			"service_name": "peer-svc", "instance_id": "peer-lost", "address": "10.4.0.1:9201",
			"port": float64(9201), "healthy": true, "weight": float64(5), "heartbeat_at": at(0),
		})

	// Repeated discovery sees the same candidates in the same order.
	rec = doRequest(t, router, http.MethodGet, discoverTarget, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s after reopen: status %d body %s", discoverEntry, rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("%s service=ro-svc after reopen: ids = %v, want %v", discoverEntry, ids, wantIDs)
	}

	// A service whose only record is strictly lost yields an empty list.
	rec = doRequest(t, router, http.MethodGet, "/api/v1/services/peer-svc/discover?"+params, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/services/peer-svc/discover: status %d body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 0 {
		t.Fatalf("GET /api/v1/services/peer-svc/discover service=peer-svc: ids = %v, want []", ids)
	}
	if !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("GET /api/v1/services/peer-svc/discover service=peer-svc: instances must be [], body %s", rec.Body.String())
	}
}
