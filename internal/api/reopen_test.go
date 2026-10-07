package api

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// reopenableServer serves the public HTTP surface backed by one SQLite file
// so a test can close the store and open it again on the same path,
// simulating a service restart.
type reopenableServer struct {
	t      *testing.T
	path   string
	st     *store.Store
	router http.Handler
}

func newReopenableServer(t *testing.T) *reopenableServer {
	t.Helper()
	server := &reopenableServer{t: t, path: filepath.Join(t.TempDir(), "service.db")}
	server.open()
	t.Cleanup(server.close)
	return server
}

func (s *reopenableServer) open() {
	st, err := store.Open(s.path)
	if err != nil {
		s.t.Fatalf("open %s: %v", s.path, err)
	}
	s.st = st
	s.router = NewRouter(st)
}

func (s *reopenableServer) close() {
	if s.st == nil {
		return
	}
	if err := s.st.Close(); err != nil {
		s.t.Fatalf("close %s: %v", s.path, err)
	}
	s.st = nil
}

// reopen closes the database and opens a fresh store on the same file.
func (s *reopenableServer) reopen() {
	s.close()
	s.open()
}

// checkRecordFields compares every field of one record object, naming the
// entry, service, instance and mismatched field on failure.
func checkRecordFields(t *testing.T, entry, service, id string, instance map[string]any, want wantRecord) {
	t.Helper()
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"service_name", instance["service_name"], service},
		{"instance_id", instance["instance_id"], id},
		{"address", instance["address"], want.address},
		{"port", instance["port"], want.port},
		{"healthy", instance["healthy"], want.healthy},
		{"weight", instance["weight"], want.weight},
		{"heartbeat_at", instance["heartbeat_at"], want.heartbeat},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Fatalf("%s: %s/%s field %s = %v, want %v",
				entry, service, id, check.field, check.got, check.want)
		}
	}
}

// checkFullRecord reads the complete record through the public GET entry and
// compares every field.
func checkFullRecord(t *testing.T, router http.Handler, service, id string, want wantRecord) {
	t.Helper()
	entry := "GET /api/v1/services/" + service + "/instances/" + id
	rec := doRequest(t, router, http.MethodGet, "/api/v1/services/"+service+"/instances/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %s/%s status = %d, want 200, body %s",
			entry, service, id, rec.Code, rec.Body.String())
	}
	instance, ok := decodeBody(t, rec)["instance"].(map[string]any)
	if !ok {
		t.Fatalf("%s: %s/%s missing instance object: %s", entry, service, id, rec.Body.String())
	}
	checkRecordFields(t, entry, service, id, instance, want)
}

// checkAttributeQueries reads the health, weight and heartbeat attribute
// entries and compares each returned field.
func checkAttributeQueries(t *testing.T, router http.Handler, service, id string, want wantRecord) {
	t.Helper()
	prefix := "/api/v1/services/" + service + "/instances/" + id

	entry := "GET " + prefix + "/health"
	rec := doRequest(t, router, http.MethodGet, prefix+"/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %s/%s status = %d, want 200, body %s", entry, service, id, rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["healthy"]; got != want.healthy {
		t.Fatalf("%s: %s/%s field healthy = %v, want %v", entry, service, id, got, want.healthy)
	}

	entry = "GET " + prefix + "/weight"
	rec = doRequest(t, router, http.MethodGet, prefix+"/weight", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %s/%s status = %d, want 200, body %s", entry, service, id, rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["weight"]; got != want.weight {
		t.Fatalf("%s: %s/%s field weight = %v, want %v", entry, service, id, got, want.weight)
	}

	entry = "GET " + prefix + "/heartbeat"
	rec = doRequest(t, router, http.MethodGet, prefix+"/heartbeat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %s/%s status = %d, want 200, body %s", entry, service, id, rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["heartbeat_at"]; got != want.heartbeat {
		t.Fatalf("%s: %s/%s field heartbeat_at = %v, want %v", entry, service, id, got, want.heartbeat)
	}
}

// expectInstanceMissing verifies the public GET entry reports the instance as
// absent with the published single-error-object shape.
func expectInstanceMissing(t *testing.T, router http.Handler, service, id string) {
	t.Helper()
	entry := "GET /api/v1/services/" + service + "/instances/" + id
	rec := doRequest(t, router, http.MethodGet, "/api/v1/services/"+service+"/instances/"+id, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%s: %s/%s status = %d, want 404, body %s",
			entry, service, id, rec.Code, rec.Body.String())
	}
	expectSingleErrorObject(t, rec, http.StatusNotFound, "instance_not_found")
}

func TestReopenPreservesRenewalAndAttributeUpdates(t *testing.T) {
	server := newReopenableServer(t)

	// Two services share the instance id i-1 with different fields; alpha/i-2
	// is only reached by the batch renewal, beta/i-1 by nothing at all.
	alphaOne := wantRecord{address: "10.0.0.1:8080", port: 8080, healthy: true, weight: 10, heartbeat: at(0)}
	alphaTwo := wantRecord{address: "10.0.0.2:8080", port: 8081, healthy: false, weight: 5, heartbeat: at(1)}
	betaOne := wantRecord{address: "10.0.9.1:9000", port: 9000, healthy: true, weight: 7, heartbeat: at(2)}
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": alphaOne.address,
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": alphaOne.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "i-2", "address": alphaTwo.address,
		"port": 8081, "healthy": false, "weight": 5, "heartbeat_at": alphaTwo.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "beta", "instance_id": "i-1", "address": betaOne.address,
		"port": 9000, "healthy": true, "weight": 7, "heartbeat_at": betaOne.heartbeat,
	})

	// Batch renewal returns the full records in request order and changes
	// only heartbeat_at.
	entry := "POST /api/v1/heartbeat"
	rec := doRequest(t, server.router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "alpha",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "i-2", "heartbeat_at": at(21)},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: service alpha status = %d, want 200, body %s", entry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"] != float64(2) {
		t.Fatalf("%s: service alpha field updated = %v, want 2", entry, out["updated"])
	}
	items, ok := out["instances"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("%s: service alpha instances = %v, want 2 records in request order", entry, out["instances"])
	}
	alphaOne.heartbeat = at(20)
	alphaTwo.heartbeat = at(21)
	first, _ := items[0].(map[string]any)
	second, _ := items[1].(map[string]any)
	checkRecordFields(t, entry, "alpha", "i-1", first, alphaOne)
	checkRecordFields(t, entry, "alpha", "i-2", second, alphaTwo)

	// The single health update changes only healthy.
	entry = "PUT /api/v1/services/alpha/instances/i-1/health"
	rec = doRequest(t, server.router, http.MethodPut, "/api/v1/services/alpha/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: alpha/i-1 status = %d, want 200, body %s", entry, rec.Code, rec.Body.String())
	}
	instance, ok := decodeBody(t, rec)["instance"].(map[string]any)
	if !ok {
		t.Fatalf("%s: alpha/i-1 missing instance object: %s", entry, rec.Body.String())
	}
	alphaOne.healthy = false
	checkRecordFields(t, entry, "alpha", "i-1", instance, alphaOne)

	// The single weight update changes only weight.
	entry = "PUT /api/v1/services/alpha/instances/i-1/weight"
	rec = doRequest(t, server.router, http.MethodPut, "/api/v1/services/alpha/instances/i-1/weight",
		map[string]any{"weight": 42})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: alpha/i-1 status = %d, want 200, body %s", entry, rec.Code, rec.Body.String())
	}
	instance, ok = decodeBody(t, rec)["instance"].(map[string]any)
	if !ok {
		t.Fatalf("%s: alpha/i-1 missing instance object: %s", entry, rec.Body.String())
	}
	alphaOne.weight = 42
	checkRecordFields(t, entry, "alpha", "i-1", instance, alphaOne)

	// Reopening the same SQLite file must expose the latest values through
	// the full record and the attribute queries; address, port, the locator
	// fields and the untouched instances keep their original values.
	server.reopen()
	checkFullRecord(t, server.router, "alpha", "i-1", alphaOne)
	checkAttributeQueries(t, server.router, "alpha", "i-1", alphaOne)
	checkFullRecord(t, server.router, "alpha", "i-2", alphaTwo)
	checkAttributeQueries(t, server.router, "alpha", "i-2", alphaTwo)
	checkFullRecord(t, server.router, "beta", "i-1", betaOne)
	checkAttributeQueries(t, server.router, "beta", "i-1", betaOne)
}

func TestReopenPreservesFailedRenewalRollback(t *testing.T) {
	server := newReopenableServer(t)

	alphaOne := wantRecord{address: "10.0.0.1:8080", port: 8080, healthy: true, weight: 10, heartbeat: at(0)}
	betaOne := wantRecord{address: "10.0.9.1:9000", port: 9000, healthy: false, weight: 3, heartbeat: at(1)}
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": alphaOne.address,
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": alphaOne.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "beta", "instance_id": "i-1", "address": betaOne.address,
		"port": 9000, "healthy": false, "weight": 3, "heartbeat_at": betaOne.heartbeat,
	})

	// Existing instance first, missing instance second: the whole batch
	// fails with 404 and neither updates the leading instance nor creates
	// the missing one.
	entry := "POST /api/v1/heartbeat"
	rec := doRequest(t, server.router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "alpha",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "ghost", "heartbeat_at": at(21)},
		},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%s: service alpha status = %d, want 404, body %s", entry, rec.Code, rec.Body.String())
	}
	expectSingleErrorObject(t, rec, http.StatusNotFound, "instance_not_found")

	// A later entry missing heartbeat_at fails validation with 400.
	rec = doRequest(t, server.router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "alpha",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "i-2"},
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%s: service alpha status = %d, want 400, body %s", entry, rec.Code, rec.Body.String())
	}
	expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")

	// Neither rejected request updated the leading instance or created a
	// record; the other service is untouched.
	checkFullRecord(t, server.router, "alpha", "i-1", alphaOne)
	expectInstanceMissing(t, server.router, "alpha", "ghost")
	expectInstanceMissing(t, server.router, "alpha", "i-2")
	checkFullRecord(t, server.router, "beta", "i-1", betaOne)

	// The rollback state survives closing and reopening the database.
	server.reopen()
	checkFullRecord(t, server.router, "alpha", "i-1", alphaOne)
	expectInstanceMissing(t, server.router, "alpha", "ghost")
	expectInstanceMissing(t, server.router, "alpha", "i-2")
	checkFullRecord(t, server.router, "beta", "i-1", betaOne)

	// A valid renewal after the failures succeeds, and the update is still
	// visible after another reopen.
	rec = doRequest(t, server.router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "alpha",
		"instances":    []any{map[string]any{"instance_id": "i-1", "heartbeat_at": at(30)}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: alpha/i-1 status = %d, want 200, body %s", entry, rec.Code, rec.Body.String())
	}
	alphaOne.heartbeat = at(30)
	server.reopen()
	checkFullRecord(t, server.router, "alpha", "i-1", alphaOne)
	checkAttributeQueries(t, server.router, "alpha", "i-1", alphaOne)
}

func TestReopenDistinguishesDiscoveryDeletionFromReadOnly(t *testing.T) {
	server := newReopenableServer(t)

	// Fixed evaluation point and timeout: at(10) with 5m puts the lost
	// boundary exactly at at(5), so the result never depends on the system
	// clock. online/online-b stay healthy and fresh, sick is unhealthy but
	// fresh, edge sits exactly on the boundary, lost is strictly lost.
	evaluate := at(10)
	timeout := "5m"
	online := wantRecord{address: "10.0.0.1:8080", port: 8080, healthy: true, weight: 10, heartbeat: at(9)}
	onlineB := wantRecord{address: "10.0.0.2:8080", port: 8082, healthy: true, weight: 10, heartbeat: at(8)}
	sick := wantRecord{address: "10.0.0.3:8080", port: 8083, healthy: false, weight: 8, heartbeat: at(9)}
	edge := wantRecord{address: "10.0.0.4:8080", port: 8084, healthy: true, weight: 6, heartbeat: at(5)}
	lost := wantRecord{address: "10.0.0.5:8080", port: 8085, healthy: true, weight: 4, heartbeat: at(4)}
	other := wantRecord{address: "10.0.9.1:9000", port: 9000, healthy: true, weight: 9, heartbeat: at(0)}
	gammaSick := wantRecord{address: "10.0.8.1:9000", port: 9001, healthy: false, weight: 1, heartbeat: at(9)}
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "online", "address": online.address,
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": online.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "online-b", "address": onlineB.address,
		"port": 8082, "healthy": true, "weight": 10, "heartbeat_at": onlineB.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "sick", "address": sick.address,
		"port": 8083, "healthy": false, "weight": 8, "heartbeat_at": sick.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "edge", "address": edge.address,
		"port": 8084, "healthy": true, "weight": 6, "heartbeat_at": edge.heartbeat,
	})
	registerInstance(t, server.router, map[string]any{
		"service_name": "alpha", "instance_id": "lost", "address": lost.address,
		"port": 8085, "healthy": true, "weight": 4, "heartbeat_at": lost.heartbeat,
	})
	// beta/other is strictly lost too, but beta is never discovered here.
	registerInstance(t, server.router, map[string]any{
		"service_name": "beta", "instance_id": "other", "address": other.address,
		"port": 9000, "healthy": true, "weight": 9, "heartbeat_at": other.heartbeat,
	})
	// gamma only holds an unhealthy-but-fresh instance: no candidates, no
	// deletions.
	registerInstance(t, server.router, map[string]any{
		"service_name": "gamma", "instance_id": "only-unhealthy", "address": gammaSick.address,
		"port": 9001, "healthy": false, "weight": 1, "heartbeat_at": gammaSick.heartbeat,
	})

	// The read-only overview counts every record by the documented classes.
	entry := "GET /api/v1/services"
	rec := doRequest(t, server.router, http.MethodGet,
		"/api/v1/services?evaluate_at="+evaluate+"&heartbeat_timeout="+timeout, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200, body %s", entry, rec.Code, rec.Body.String())
	}
	services, ok := decodeBody(t, rec)["services"].([]any)
	if !ok || len(services) != 3 {
		t.Fatalf("%s: services = %v, want 3 entries", entry, services)
	}
	checkServiceCounters(t, entry, services[0], "alpha", 5, 3, 1, 1)
	checkServiceCounters(t, entry, services[1], "beta", 1, 0, 0, 1)
	checkServiceCounters(t, entry, services[2], "gamma", 1, 0, 1, 0)

	// The instance list keeps every record, and the strictly lost record is
	// still queryable before any discovery runs.
	entry = "GET /api/v1/services/alpha/instances"
	rec = doRequest(t, server.router, http.MethodGet, "/api/v1/services/alpha/instances", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200, body %s", entry, rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 5 {
		t.Fatalf("%s: service alpha ids = %v, want 5 records", entry, ids)
	}
	checkFullRecord(t, server.router, "alpha", "lost", lost)

	// Discovery returns only healthy, not-lost instances ordered by weight
	// descending, ties broken by instance id ascending; the strictly lost
	// record is deleted.
	discoverAlpha := func() []string {
		t.Helper()
		target := "/api/v1/discover?service_name=alpha&evaluate_at=" + evaluate + "&heartbeat_timeout=" + timeout
		rec := doRequest(t, server.router, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200, body %s", target, rec.Code, rec.Body.String())
		}
		return instanceIDs(t, decodeBody(t, rec))
	}
	wantIDs := []string{"online", "online-b", "edge"}
	if ids := discoverAlpha(); !sameIDs(ids, wantIDs) {
		t.Fatalf("GET /api/v1/discover: service alpha ids = %v, want %v", ids, wantIDs)
	}

	// Reopening keeps the deletion: the lost instance stays gone while every
	// other record, including the other service's lost one, is preserved.
	server.reopen()
	expectInstanceMissing(t, server.router, "alpha", "lost")
	checkFullRecord(t, server.router, "alpha", "online", online)
	checkFullRecord(t, server.router, "alpha", "online-b", onlineB)
	checkFullRecord(t, server.router, "alpha", "sick", sick)
	checkFullRecord(t, server.router, "alpha", "edge", edge)
	checkFullRecord(t, server.router, "beta", "other", other)

	// Repeated discovery yields the same candidates in the same order.
	if ids := discoverAlpha(); !sameIDs(ids, wantIDs) {
		t.Fatalf("GET /api/v1/discover after reopen: service alpha ids = %v, want %v", ids, wantIDs)
	}

	// No candidates: gamma's only instance is unhealthy, and an unknown
	// service has no records at all. Both answer with an empty array and
	// delete nothing.
	for _, service := range []string{"gamma", "unknown"} {
		target := "/api/v1/discover?service_name=" + service + "&evaluate_at=" + evaluate + "&heartbeat_timeout=" + timeout
		rec := doRequest(t, server.router, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200, body %s", target, rec.Code, rec.Body.String())
		}
		if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 0 {
			t.Fatalf("GET %s: ids = %v, want []", target, ids)
		}
		if !strings.Contains(rec.Body.String(), `"instances":[]`) {
			t.Fatalf("GET %s: instances must serialize as an empty array: %s", target, rec.Body.String())
		}
	}
	checkFullRecord(t, server.router, "gamma", "only-unhealthy", gammaSick)
}

// checkServiceCounters compares one overview entry with the expected counts.
func checkServiceCounters(t *testing.T, entry string, raw any, service string, total, available, unhealthyFresh, lost int) {
	t.Helper()
	item, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s: service %s entry is not an object: %v", entry, service, raw)
	}
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"service_name", item["service_name"], service},
		{"total_instances", item["total_instances"], float64(total)},
		{"available_instances", item["available_instances"], float64(available)},
		{"unhealthy_fresh_instances", item["unhealthy_fresh_instances"], float64(unhealthyFresh)},
		{"lost_instances", item["lost_instances"], float64(lost)},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Fatalf("%s: service %s field %s = %v, want %v",
				entry, service, check.field, check.got, check.want)
		}
	}
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
