package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

func apiStore(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewRouter(st)
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func validBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"service_name": "billing",
		"instance_id":  "i-1",
		"address":      "10.0.0.1:9000",
		"healthy":      true,
		"weight":       10,
		"heartbeat_at": "2026-10-01T12:00:00Z",
	}
	for k, v := range overrides {
		body[k] = v
	}
	return body
}

func TestRegisterAndReadBack(t *testing.T) {
	_, handler := apiStore(t)

	rec := doJSON(t, handler, http.MethodPut, "/v1/instances", validBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d body = %s", rec.Code, rec.Body.String())
	}
	payload := decode(t, rec)
	inst := payload["instance"].(map[string]any)
	if inst["service_name"] != "billing" || inst["instance_id"] != "i-1" ||
		inst["address"] != "10.0.0.1:9000" || inst["healthy"] != true ||
		inst["weight"].(float64) != 10 || inst["heartbeat_at"] != "2026-10-01T12:00:00Z" {
		t.Fatalf("unexpected instance payload: %v", inst)
	}

	// Same service name + instance identifier replaces the current record.
	rec = doJSON(t, handler, http.MethodPut, "/v1/instances",
		validBody(map[string]any{"address": "10.0.0.2:9001", "weight": 3}))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-register status = %d body = %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		path string
		want map[string]any
	}{
		{"/v1/services/billing/instances/i-1", map[string]any{"address": "10.0.0.2:9001", "weight": 3.0}},
		{"/v1/services/billing/instances/i-1/health", map[string]any{"healthy": true}},
		{"/v1/services/billing/instances/i-1/weight", map[string]any{"weight": 3.0}},
		{"/v1/services/billing/instances/i-1/heartbeat", map[string]any{"heartbeat_at": "2026-10-01T12:00:00Z"}},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body = %s", tc.path, rec.Code, rec.Body.String())
		}
		got := decode(t, rec)
		if nested, ok := got["instance"].(map[string]any); ok {
			got = nested
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Fatalf("GET %s field %s = %v, want %v", tc.path, k, got[k], v)
			}
		}
	}
}

func TestRegisterInvalidParametersDoNotCreate(t *testing.T) {
	st, handler := apiStore(t)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]map[string]any{
		"empty service":     {"service_name": ""},
		"empty instance":    {"instance_id": ""},
		"negative weight":   {"weight": -0.5},
		"missing weight":    {"weight": nil},
		"missing heartbeat": {"heartbeat_at": nil},
		"bad heartbeat":     {"heartbeat_at": "not-a-time"},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			body := validBody(override)
			if body["weight"] == nil {
				delete(body, "weight")
			}
			if body["heartbeat_at"] == nil {
				delete(body, "heartbeat_at")
			}
			rec := doJSON(t, handler, http.MethodPut, "/v1/instances", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			errBody := decode(t, rec)["error"].(map[string]any)
			if errBody["code"] != "invalid_parameter" {
				t.Fatalf("error code = %v", errBody["code"])
			}
		})
	}
	if _, err := st.GetInstance(t.Context(), "billing", "i-1"); err != store.ErrNotFound {
		t.Fatalf("records must be untouched after failed validation: %v", err)
	}
	_ = base
}

func TestDiscoveryEndToEnd(t *testing.T) {
	_, handler := apiStore(t)
	base := "2026-10-01T12:00:00Z"

	register := func(id string, weight float64, healthy bool, secondsAfter int) {
		t.Helper()
		heartbeat := time.Date(2026, 10, 1, 12, 0, secondsAfter, 0, time.UTC).Format(time.RFC3339)
		rec := doJSON(t, handler, http.MethodPut, "/v1/instances", validBody(map[string]any{
			"instance_id": id, "weight": weight, "healthy": healthy, "heartbeat_at": heartbeat,
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	register("a", 1, true, 0)  // lost: heartbeat == deadline (eval 10s - timeout 10s)
	register("b", 5, true, 6)  // healthy
	register("c", 5, true, 9)  // healthy, tie weight, newer heartbeat
	register("d", 9, false, 9) // unhealthy, never returned
	register("e", 9, true, 10) // healthy, heartbeat == evaluate_at
	_ = base

	// Flat entry with query parameters exercises the empty-name case too.
	path := "/v1/instances?service_name=billing&evaluate_at=2026-10-01T12:00:10Z&timeout=10s"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status = %d body = %s", rec.Code, rec.Body.String())
	}
	payload := decode(t, rec)
	list := payload["instances"].([]any)
	gotIDs := make([]string, 0, len(list))
	for _, item := range list {
		gotIDs = append(gotIDs, item.(map[string]any)["instance_id"].(string))
	}
	wantIDs := []string{"e", "c", "b"}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("ids = %v, want %v", gotIDs, wantIDs)
		}
	}
	if payload["timeout"] != "10s" {
		t.Fatalf("timeout echo = %v", payload["timeout"])
	}

	// Resource-style entry returns the same set; lost instance is already evicted.
	req = httptest.NewRequest(http.MethodGet,
		"/v1/services/billing/instances?evaluate_at=2026-10-01T12:00:10Z&timeout=10s", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resource discover status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Lost instance is no longer fetchable; unhealthy one stays stored.
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/v1/services/billing/instances/a", http.StatusNotFound},
		{"/v1/services/billing/instances/d", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("GET %s = %d, want %d (%s)", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestDiscoveryEmptyList(t *testing.T) {
	_, handler := apiStore(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/instances?service_name=nope&evaluate_at=2026-10-01T12:00:10Z&timeout=10s", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	payload := decode(t, rec)
	list, ok := payload["instances"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("instances = %v, want []", payload["instances"])
	}
}

func TestDiscoveryValidation(t *testing.T) {
	_, handler := apiStore(t)
	cases := map[string]string{
		"empty service":    "/v1/instances?service_name=&evaluate_at=2026-10-01T12:00:10Z&timeout=10s",
		"missing evaluate": "/v1/instances?service_name=svc&timeout=10s",
		"zero timeout":     "/v1/instances?service_name=svc&evaluate_at=2026-10-01T12:00:10Z&timeout=0s",
		"negative timeout": "/v1/instances?service_name=svc&evaluate_at=2026-10-01T12:00:10Z&timeout=-5s",
		"missing timeout":  "/v1/instances?service_name=svc&evaluate_at=2026-10-01T12:00:10Z",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
		})
	}

	// evaluate_at earlier than a stored heartbeat is a parameter error with no eviction.
	rec := doJSON(t, handler, http.MethodPut, "/v1/instances", validBody(map[string]any{
		"heartbeat_at": "2026-10-01T12:00:05Z", "instance_id": "late"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %s", rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet,
		"/v1/instances?service_name=billing&evaluate_at=2026-10-01T12:00:00Z&timeout=10s", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("future heartbeat status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeleteInstance(t *testing.T) {
	_, handler := apiStore(t)
	if rec := doJSON(t, handler, http.MethodPut, "/v1/instances", validBody(nil)); rec.Code != http.StatusOK {
		t.Fatalf("register: %s", rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/services/billing/instances/i-1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["deleted"]; got != true {
		t.Fatalf("deleted = %v", got)
	}
	req = httptest.NewRequest(http.MethodDelete, "/v1/services/billing/instances/i-1", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}
