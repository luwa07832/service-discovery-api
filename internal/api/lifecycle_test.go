package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

const lifecycleBaseTime = "2026-10-01T12:00:00Z"

func lifecycleRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ignored.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), st
}

func lifecycleRequest(t *testing.T, router http.Handler, method, target string, body any) *httptest.ResponseRecorder {
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
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func register(t *testing.T, router http.Handler, body map[string]any) map[string]any {
	t.Helper()
	rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/register", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("register %v: status %d body %s", body, rec.Code, rec.Body.String())
	}
	return decodeLifecycleBody(t, rec)
}

func decodeLifecycleBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func expectInvalidParameter(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body.String())
	}
	out := decodeLifecycleBody(t, rec)
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("code = %v, want INVALID_ARGUMENT", errObj["code"])
	}
}

func goodRegistration() map[string]any {
	return map[string]any{
		"service_name": "svc", "instance_id": "i-1",
		"host": "10.0.0.1", "port": 9000,
		"healthy": true, "weight": 7,
		"heartbeat_at": lifecycleBaseTime, "request_at": lifecycleBaseTime,
	}
}

func TestRegistrationRecordsInstanceAndExistence(t *testing.T) {
	router, st := lifecycleRouter(t)

	out := register(t, router, goodRegistration())
	if out["registered"] != true || out["already_registered"] != false {
		t.Fatalf("first registration flags: %v", out)
	}
	instance := out["instance"].(map[string]any)
	if instance["service_name"] != "svc" || instance["instance_id"] != "i-1" ||
		instance["host"] != "10.0.0.1" || instance["port"].(float64) != 9000 ||
		instance["address"] != "10.0.0.1:9000" || instance["healthy"] != true ||
		instance["weight"].(float64) != 7 || instance["heartbeat_at"] != lifecycleBaseTime {
		t.Fatalf("registration record mismatch: %v", instance)
	}

	// The same (service, instance) pair overwrites and flips the flag.
	again := goodRegistration()
	again["host"] = "10.0.0.2"
	again["port"] = 80
	again["weight"] = 3
	out = register(t, router, again)
	if out["already_registered"] != true {
		t.Fatalf("repeat registration should report already_registered: %v", out)
	}
	instance = out["instance"].(map[string]any)
	if instance["host"] != "10.0.0.2" || instance["port"].(float64) != 80 || instance["weight"].(float64) != 3 {
		t.Fatalf("overwritten record mismatch: %v", instance)
	}
	list, err := st.ListInstances("svc")
	if err != nil || len(list) != 1 {
		t.Fatalf("repeat registration created a record: %d rows", len(list))
	}

	// Different instance ids are stored independently under the same service.
	other := goodRegistration()
	other["instance_id"] = "i-2"
	register(t, router, other)
	if list, _ := st.ListInstances("svc"); len(list) != 2 {
		t.Fatalf("second instance not stored: %d rows", len(list))
	}
}

func TestRegistrationAcceptsLegacyHostPortAddress(t *testing.T) {
	router, _ := lifecycleRouter(t)
	body := map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.1:9000",
		"healthy": true, "weight": 7, "heartbeat_at": lifecycleBaseTime, "request_at": lifecycleBaseTime,
	}
	out := register(t, router, body)
	instance := out["instance"].(map[string]any)
	if instance["host"] != "10.0.0.1" || instance["port"].(float64) != 9000 || instance["address"] != "10.0.0.1:9000" {
		t.Fatalf("legacy address fallback mismatch: %v", instance)
	}
}

func TestRegistrationValidationCreatesNothing(t *testing.T) {
	router, st := lifecycleRouter(t)
	good := goodRegistration()
	cases := []map[string]any{
		without(good, "service_name"),
		without(good, "instance_id"),
		without(good, "host"),
		without(good, "port"),
		without(good, "request_at"),
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": -1, "weight": 1, "request_at": lifecycleBaseTime},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 70000, "weight": 1, "request_at": lifecycleBaseTime},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": 1, "request_at": lifecycleBaseTime, "healthy": 2},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": -1, "request_at": lifecycleBaseTime},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": 0, "request_at": lifecycleBaseTime},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": "abc", "request_at": lifecycleBaseTime},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": 1, "request_at": "not-a-time"},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": 1,
			"request_at": lifecycleBaseTime, "heartbeat_at": lifecycleAt(1)},
		{"service_name": "svc", "instance_id": "i", "host": "h", "port": 9000, "weight": 1,
			"request_at": lifecycleBaseTime, "heartbeat_at": "not-a-time"},
	}
	for index, body := range cases {
		rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/register", body)
		expectInvalidParameter(t, rec)
		if list, err := st.ListInstances("svc"); err != nil || len(list) != 0 {
			t.Fatalf("case %d changed storage: %d rows err=%v", index, len(list), err)
		}
	}
}

func without(original map[string]any, key string) map[string]any {
	copyBody := make(map[string]any, len(original))
	for k, v := range original {
		copyBody[k] = v
	}
	delete(copyBody, key)
	return copyBody
}

func TestHeartbeatDefaultsToRequestTime(t *testing.T) {
	router, st := lifecycleRouter(t)
	register(t, router, goodRegistration())
	out := register(t, router, func() map[string]any {
		body := goodRegistration()
		delete(body, "heartbeat_at")
		return body
	}())
	instance := out["instance"].(map[string]any)
	if instance["heartbeat_at"] != lifecycleBaseTime {
		t.Fatalf("default heartbeat = %v, want %s", instance["heartbeat_at"], lifecycleBaseTime)
	}
	_ = st
}

func TestPublicQueriesReadStoredRecord(t *testing.T) {
	router, _ := lifecycleRouter(t)
	register(t, router, goodRegistration())

	for _, target := range []string{
		"/api/v1/services/svc/instances/i-1",
		"/api/v1/instances?service_name=svc&instance_id=i-1",
	} {
		rec := lifecycleRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		instance := decodeLifecycleBody(t, rec)["instance"].(map[string]any)
		if instance["host"] != "10.0.0.1" || instance["port"].(float64) != 9000 || instance["weight"].(float64) != 7 {
			t.Fatalf("%s unexpected record %v", target, instance)
		}
	}

	health := decodeLifecycleBody(t, lifecycleRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1/health", nil))
	if health["healthy"] != true || health["health"] != "healthy" {
		t.Fatalf("health query mismatch: %v", health)
	}
	weight := decodeLifecycleBody(t, lifecycleRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1/weight", nil))
	if weight["weight"].(float64) != 7 {
		t.Fatalf("weight query mismatch: %v", weight)
	}
	heartbeat := decodeLifecycleBody(t, lifecycleRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1/heartbeat", nil))
	if heartbeat["heartbeat_at"] != lifecycleBaseTime {
		t.Fatalf("heartbeat query mismatch: %v", heartbeat)
	}
}
