package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

func healthRecord() map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  "i-1",
		"address":      "10.0.0.8:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       10,
		"heartbeat_at": baseTime,
	}
}

func assertHealthUnchanged(t *testing.T, st *store.Store) {
	t.Helper()
	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !got.Healthy || got.Address != "10.0.0.8:8080" || got.Port != 8080 ||
		got.Weight != 10 || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("record changed after failed request: %+v", got)
	}
}

func TestUpdateHealthChangesOnlyHealthy(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "svc" || instance["instance_id"] != "i-1" ||
		instance["healthy"] != false || instance["address"] != "10.0.0.8:8080" ||
		instance["port"].(float64) != 8080 || instance["weight"].(float64) != 10 {
		t.Fatalf("unexpected response record: %v", instance)
	}
	if instance["heartbeat_at"] != baseTime {
		t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], baseTime)
	}

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Healthy || got.Address != "10.0.0.8:8080" ||
		got.Port != 8080 || got.Weight != 10 ||
		!got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("stored record mismatch: %+v found=%v", got, found)
	}
}

func TestUpdateHealthAcceptsQueryParameter(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, healthRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health?healthy=false", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query false: status = %d body %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["instance"].(map[string]any)["healthy"] != false {
		t.Fatalf("healthy should be false")
	}

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health?healthy=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query true: status = %d body %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["instance"].(map[string]any)["healthy"] != true {
		t.Fatalf("healthy should be true")
	}
}

func TestUpdateHealthJSONBodyWinsOverQuery(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, healthRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health?healthy=true",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["instance"].(map[string]any)["healthy"] != false {
		t.Fatalf("healthy = false wanted, body must win over query")
	}
}

func TestUpdateHealthValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())

	cases := []struct {
		name    string
		path    string
		rawBody string
		body    any
		query   bool
	}{
		{name: "missing healthy", body: map[string]any{}},
		{name: "string healthy in json", body: map[string]any{"healthy": "false"}},
		{name: "numeric healthy in json", body: map[string]any{"healthy": 0}},
		{name: "null healthy in json", body: map[string]any{"healthy": nil}},
		{name: "uppercase query false", path: "/api/v1/services/svc/instances/i-1/health?healthy=False", query: true},
		{name: "numeric query", path: "/api/v1/services/svc/instances/i-1/health?healthy=1", query: true},
		{name: "word query", path: "/api/v1/services/svc/instances/i-1/health?healthy=yes", query: true},
		{name: "padded query", path: "/api/v1/services/svc/instances/i-1/health?healthy=false%20", query: true},
		{name: "empty body", body: nil},
		{name: "body not an object", rawBody: "[false]"},
		{name: "body malformed json", rawBody: "{bad"},
		{name: "json invalid wins over valid query", path: "/api/v1/services/svc/instances/i-1/health?healthy=false", body: map[string]any{"healthy": "no"}},
		{name: "empty service name", path: "/api/v1/services//instances/i-1/health", body: map[string]any{"healthy": false}},
		{name: "empty instance id", path: "/api/v1/services/svc/instances//health", body: map[string]any{"healthy": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/api/v1/services/svc/instances/i-1/health"
			}
			var rec *httptest.ResponseRecorder
			switch {
			case tc.rawBody != "":
				rec = doRawRequest(t, router, http.MethodPut, path, tc.rawBody)
			default:
				rec = doRequest(t, router, http.MethodPut, path, tc.body)
			}
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertHealthUnchanged(t, st)
		})
	}
}

func TestUpdateHealthMissingInstanceReturns404(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/gone/health",
		map[string]any{"healthy": false})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
	assertHealthUnchanged(t, st)
}

func TestBatchUpdateHealthAppliesInRequestOrder(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())
	second := healthRecord()
	second["instance_id"] = "i-2"
	second["weight"] = 3
	registerInstance(t, router, second)
	third := healthRecord()
	third["instance_id"] = "i-3"
	third["healthy"] = false
	registerInstance(t, router, third)
	other := healthRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)

	body := map[string]any{
		"updates": []any{
			map[string]any{"instance_id": "i-3", "healthy": true},
			map[string]any{"instance_id": "  i-1  ", "healthy": false},
		},
	}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/health", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("updated = %v, want 2", out["updated"])
	}
	entries := out["instances"].([]any)
	if len(entries) != 2 {
		t.Fatalf("instances len = %d", len(entries))
	}
	first := entries[0].(map[string]any)
	secondEntry := entries[1].(map[string]any)
	if first["instance_id"] != "i-3" || secondEntry["instance_id"] != "i-1" {
		t.Fatalf("order not preserved: %v %v", first["instance_id"], secondEntry["instance_id"])
	}
	if first["healthy"] != true || first["address"] != "10.0.0.8:8080" ||
		first["port"].(float64) != 8080 || first["weight"].(float64) != 10 {
		t.Fatalf("batch changed non-healthy fields: %v", first)
	}
	if first["heartbeat_at"] != baseTime {
		t.Fatalf("i-3 heartbeat changed: %v", first["heartbeat_at"])
	}
	if secondEntry["healthy"] != false {
		t.Fatalf("i-1 healthy = %v, want false", secondEntry["healthy"])
	}

	got2, _, _ := st.GetInstance("svc", "i-2")
	if got2.Healthy != true {
		t.Fatalf("i-2 health changed: %+v", got2)
	}
	peer, _, _ := st.GetInstance("other", "i-1")
	if !peer.Healthy {
		t.Fatalf("other service record touched: %+v", peer)
	}
}

func TestBatchUpdateHealthValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())
	second := healthRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	cases := []struct {
		name    string
		path    string
		rawBody string
		body    any
	}{
		{name: "missing updates", body: map[string]any{}},
		{name: "empty updates", body: map[string]any{"updates": []any{}}},
		{name: "updates not a list", body: map[string]any{"updates": "i-1"}},
		{name: "updates null", body: map[string]any{"updates": nil}},
		{name: "entry not an object", body: map[string]any{"updates": []any{"i-1"}}},
		{name: "entry missing instance id", body: map[string]any{"updates": []any{
			map[string]any{"healthy": false}}}},
		{name: "entry blank instance id", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "   ", "healthy": false}}}},
		{name: "duplicate instance id", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": false},
			map[string]any{"instance_id": " i-1 ", "healthy": true}}}},
		{name: "missing healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1"}}}},
		{name: "string healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": "false"}}}},
		{name: "numeric healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": 1}}}},
		{name: "null healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": nil}}}},
		{name: "body not an object", rawBody: "[]"},
		{name: "body malformed json", rawBody: "{not json"},
		{name: "empty body", rawBody: ""},
		{name: "empty service name", path: "/api/v1/services//instances/health", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": false}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/api/v1/services/svc/instances/health"
			}
			var rec *httptest.ResponseRecorder
			if tc.rawBody != "" || tc.name == "empty body" {
				rec = doRawRequest(t, router, http.MethodPut, path, tc.rawBody)
			} else {
				rec = doRequest(t, router, http.MethodPut, path, tc.body)
			}
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertHealthUnchanged(t, st)
			got2, found, _ := st.GetInstance("svc", "i-2")
			if !found || !got2.Healthy {
				t.Fatalf("i-2 changed after invalid batch: %+v", got2)
			}
		})
	}
}

func TestBatchUpdateHealthMissingInstanceRollsBack(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())
	second := healthRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	body := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-1", "healthy": false},
		map[string]any{"instance_id": "ghost", "healthy": true},
		map[string]any{"instance_id": "i-2", "healthy": false},
	}}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/health", body)
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	for _, id := range []string{"i-1", "i-2"} {
		got, found, _ := st.GetInstance("svc", id)
		if !found || !got.Healthy {
			t.Fatalf("%s changed after partial miss: %+v", id, got)
		}
	}
}

func TestHealthUpdateStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	registerInstance(t, router, healthRecord())
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": false})
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")

	batchBody := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-1", "healthy": false},
	}}
	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/health", batchBody)
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
}

func TestHealthUpdateAffectsDiscoveryButNotCleanupBoundary(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, healthRecord())
	stale := healthRecord()
	stale["instance_id"] = "i-2"
	stale["healthy"] = true
	stale["heartbeat_at"] = "2026-10-01T11:00:00Z"
	registerInstance(t, router, stale)

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/discover?"+
			"evaluate_at=2026-10-01T12:00:00Z&heartbeat_timeout=30s", nil))
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("unhealthy i-1 must not be discovered and stale i-2 is removed, got %v", ids)
	}

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-2/health",
		map[string]any{"healthy": true})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
}
