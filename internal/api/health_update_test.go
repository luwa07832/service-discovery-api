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
	if got.Address != "10.0.0.8:8080" || got.Port != 8080 || !got.Healthy || got.Weight != 10 ||
		!got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("record changed after failed request: %+v", got)
	}
}

func TestUpdateHealthChangesOnlyHealth(t *testing.T) {
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
		instance["address"] != "10.0.0.8:8080" || instance["port"].(float64) != 8080 ||
		instance["healthy"] != false || instance["weight"].(float64) != 10 {
		t.Fatalf("unexpected response record: %v", instance)
	}
	if instance["heartbeat_at"] != baseTime {
		t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], baseTime)
	}

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Healthy || got.Address != "10.0.0.8:8080" ||
		got.Port != 8080 || got.Weight != 10 || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
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
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["healthy"] != false {
		t.Fatalf("healthy = %v, want false", instance["healthy"])
	}

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health?healthy=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query true: status = %d body %s", rec.Code, rec.Body.String())
	}
	instance = decodeBody(t, rec)["instance"].(map[string]any)
	if instance["healthy"] != true {
		t.Fatalf("healthy = %v, want true", instance["healthy"])
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
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["healthy"] != false {
		t.Fatalf("healthy = %v, want false (body wins)", instance["healthy"])
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
	}{
		{name: "missing healthy", path: "/api/v1/services/svc/instances/i-1/health", body: map[string]any{}},
		{name: "null healthy", path: "/api/v1/services/svc/instances/i-1/health", body: map[string]any{"healthy": nil}},
		{name: "string healthy", path: "/api/v1/services/svc/instances/i-1/health", body: map[string]any{"healthy": "true"}},
		{name: "number healthy", path: "/api/v1/services/svc/instances/i-1/health", body: map[string]any{"healthy": 1}},
		{name: "string body wins over valid query", path: "/api/v1/services/svc/instances/i-1/health?healthy=true", body: map[string]any{"healthy": "true"}},
		{name: "query uppercase", path: "/api/v1/services/svc/instances/i-1/health?healthy=True"},
		{name: "query all caps", path: "/api/v1/services/svc/instances/i-1/health?healthy=TRUE"},
		{name: "query number text", path: "/api/v1/services/svc/instances/i-1/health?healthy=1"},
		{name: "query other text", path: "/api/v1/services/svc/instances/i-1/health?healthy=yes"},
		{name: "query blank", path: "/api/v1/services/svc/instances/i-1/health?healthy="},
		{name: "empty body", path: "/api/v1/services/svc/instances/i-1/health"},
		{name: "body not an object", path: "/api/v1/services/svc/instances/i-1/health", rawBody: "[]"},
		{name: "body malformed json", path: "/api/v1/services/svc/instances/i-1/health", rawBody: "{not json"},
		{name: "empty service name", path: "/api/v1/services//instances/i-1/health", body: map[string]any{"healthy": false}},
		{name: "empty instance id", path: "/api/v1/services/svc/instances//health", body: map[string]any{"healthy": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			if tc.rawBody != "" {
				rec = doRawRequest(t, router, http.MethodPut, tc.path, tc.rawBody)
			} else {
				rec = doRequest(t, router, http.MethodPut, tc.path, tc.body)
			}
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertHealthUnchanged(t, st)
		})
	}
}

func TestUpdateHealthMissingInstanceReturnsNotFound(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/ghost/health",
		map[string]any{"healthy": false})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
	assertHealthUnchanged(t, st)

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/other/instances/i-1/health",
		map[string]any{"healthy": false})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
	assertHealthUnchanged(t, st)
}

func TestUpdateHealthKeepsLostInstanceStored(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}

	// The health update never renews the heartbeat and never triggers the
	// discovery lost cleanup: the record stays stored with its original
	// heartbeat even when it is strictly past any reasonable timeout.
	got, found, _ := st.GetInstance("svc", "i-1")
	if !found {
		t.Fatalf("record removed by health update")
	}
	if got.Healthy || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("record mismatch after health update: %+v", got)
	}
}

func TestBatchUpdateHealthChangesOnlyHealthInRequestOrder(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())
	second := healthRecord()
	second["instance_id"] = "i-2"
	second["healthy"] = false
	second["heartbeat_at"] = "2025-10-01T12:05:00Z"
	registerInstance(t, router, second)
	peer := healthRecord()
	peer["service_name"] = "other"
	registerInstance(t, router, peer)

	body := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-2", "healthy": true},
		map[string]any{"instance_id": "i-1", "healthy": false},
	}}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/health", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("updated = %v, want 2", out["updated"])
	}
	instances := out["instances"].([]any)
	if len(instances) != 2 {
		t.Fatalf("instances = %v", instances)
	}
	first := instances[0].(map[string]any)
	if first["instance_id"] != "i-2" || first["healthy"] != true ||
		first["weight"].(float64) != 10 || first["heartbeat_at"] != "2025-10-01T12:05:00Z" {
		t.Fatalf("first record mismatch: %v", first)
	}
	secondOut := instances[1].(map[string]any)
	if secondOut["instance_id"] != "i-1" || secondOut["healthy"] != false ||
		secondOut["address"] != "10.0.0.8:8080" || secondOut["port"].(float64) != 8080 ||
		secondOut["heartbeat_at"] != baseTime {
		t.Fatalf("second record mismatch: %v", secondOut)
	}

	got1, _, _ := st.GetInstance("svc", "i-1")
	if got1.Healthy || got1.Weight != 10 || !got1.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("i-1 mismatch: %+v", got1)
	}
	got2, _, _ := st.GetInstance("svc", "i-2")
	if !got2.Healthy {
		t.Fatalf("i-2 health not updated: %+v", got2)
	}
	peerGot, _, _ := st.GetInstance("other", "i-1")
	if !peerGot.Healthy {
		t.Fatalf("other service record touched: %+v", peerGot)
	}
}

func TestBatchUpdateHealthValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, healthRecord())
	second := healthRecord()
	second["instance_id"] = "i-2"
	second["healthy"] = false
	registerInstance(t, router, second)

	cases := []struct {
		name    string
		path    string
		rawBody string
		body    any
		useRaw  bool
	}{
		{name: "missing updates", body: map[string]any{}},
		{name: "empty updates", body: map[string]any{"updates": []any{}}},
		{name: "updates not a list", body: map[string]any{"updates": "i-1"}},
		{name: "entry not an object", body: map[string]any{"updates": []any{"i-1"}}},
		{name: "entry missing instance id", body: map[string]any{"updates": []any{
			map[string]any{"healthy": true}}}},
		{name: "entry blank instance id", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "  ", "healthy": true}}}},
		{name: "duplicate instance id", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": true},
			map[string]any{"instance_id": "i-1", "healthy": false}}}},
		{name: "missing healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1"}}}},
		{name: "string healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": "true"}}}},
		{name: "number healthy", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "healthy": 1}}}},
		{name: "body not an object", rawBody: "[]", useRaw: true},
		{name: "body malformed json", rawBody: "{not json", useRaw: true},
		{name: "empty body", rawBody: "", useRaw: true},
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
			if tc.useRaw {
				rec = doRawRequest(t, router, http.MethodPut, path, tc.rawBody)
			} else {
				rec = doRequest(t, router, http.MethodPut, path, tc.body)
			}
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertHealthUnchanged(t, st)
			got2, found, _ := st.GetInstance("svc", "i-2")
			if !found || got2.Healthy {
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
	second["healthy"] = false
	registerInstance(t, router, second)

	body := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-1", "healthy": false},
		map[string]any{"instance_id": "ghost", "healthy": true},
		map[string]any{"instance_id": "i-2", "healthy": true},
	}}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/health", body)
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	got1, _, _ := st.GetInstance("svc", "i-1")
	if !got1.Healthy {
		t.Fatalf("i-1 changed after partial miss: %+v", got1)
	}
	got2, _, _ := st.GetInstance("svc", "i-2")
	if got2.Healthy {
		t.Fatalf("i-2 changed after partial miss: %+v", got2)
	}
}

func TestUpdateHealthStorageUnavailable(t *testing.T) {
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
