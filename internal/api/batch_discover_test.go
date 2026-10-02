package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func batchDiscover(t *testing.T, router http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, router, http.MethodPost, "/api/v1/discover/batch", body)
}

func registerBatchFixture(t *testing.T, router http.Handler) {
	t.Helper()
	instances := []map[string]any{
		{"service_name": "svc-a", "instance_id": "i-a2", "weight": 5, "healthy": true, "heartbeat_at": at(0)},
		{"service_name": "svc-a", "instance_id": "i-a1", "weight": 10, "healthy": true, "heartbeat_at": at(0)},
		{"service_name": "svc-a", "instance_id": "i-a3", "weight": 1, "healthy": false, "heartbeat_at": at(0)},
		{"service_name": "svc-b", "instance_id": "i-b1", "weight": 3, "healthy": true, "heartbeat_at": at(-1)},
		{"service_name": "svc-other", "instance_id": "i-x", "weight": 9, "healthy": true, "heartbeat_at": at(-10)},
	}
	for _, instance := range instances {
		registerInstance(t, router, instance)
	}
}

func TestBatchDiscoverSharesEvaluationAcrossServices(t *testing.T) {
	router, st := testRouter(t)
	registerBatchFixture(t, router)

	rec := batchDiscover(t, router, map[string]any{
		"service_names":     []any{"svc-c", " svc-a ", "svc-b"},
		"evaluate_at":       at(5),
		"heartbeat_timeout": 300,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["evaluate_at"] != "2026-10-01T12:05:00Z" {
		t.Fatalf("evaluate_at = %v", out["evaluate_at"])
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("heartbeat_timeout = %v", out["heartbeat_timeout"])
	}
	services := out["services"].([]any)
	if len(services) != 3 {
		t.Fatalf("services len = %d, want 3", len(services))
	}
	first := services[0].(map[string]any)
	if first["service_name"] != "svc-c" {
		t.Fatalf("first service = %v, want svc-c", first["service_name"])
	}
	if empty, ok := first["instances"].([]any); !ok || len(empty) != 0 {
		t.Fatalf("missing service must return empty instances array: %v", first["instances"])
	}

	second := services[1].(map[string]any)
	if second["service_name"] != "svc-a" {
		t.Fatalf("second service = %v, want svc-a", second["service_name"])
	}
	hits := second["instances"].([]any)
	if len(hits) != 2 {
		t.Fatalf("svc-a hits = %d, want 2: %v", len(hits), hits)
	}
	if hits[0].(map[string]any)["instance_id"] != "i-a1" ||
		hits[1].(map[string]any)["instance_id"] != "i-a2" {
		t.Fatalf("svc-a order = %v, want [i-a1 i-a2]", hits)
	}
	firstHit := hits[0].(map[string]any)
	for _, key := range []string{"instance_id", "address", "port", "healthy", "weight", "heartbeat_at", "service_name"} {
		if _, ok := firstHit[key]; !ok {
			t.Fatalf("hit missing field %q: %v", key, firstHit)
		}
	}

	third := services[2].(map[string]any)
	if third["service_name"] != "svc-b" {
		t.Fatalf("third service = %v, want svc-b", third["service_name"])
	}
	if empty, ok := third["instances"].([]any); !ok || len(empty) != 0 {
		t.Fatalf("svc-b instances = %v, want empty after lost removal", third["instances"])
	}

	if _, found, err := st.GetInstance("svc-b", "i-b1"); err != nil || found {
		t.Fatalf("lost svc-b/i-b1 must be deleted: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetInstance("svc-a", "i-a3"); err != nil || !found {
		t.Fatalf("unhealthy fresh svc-a/i-a3 must be kept: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetInstance("svc-other", "i-x"); err != nil || !found {
		t.Fatalf("unspecified svc-other/i-x must be untouched: found=%v err=%v", found, err)
	}
}

func TestBatchDiscoverBoundaryKeepsInstanceOnline(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-edge", "weight": 2,
		"healthy": true, "heartbeat_at": at(0),
	})

	// heartbeat_at 12:00 + 5m == evaluate_at 12:05: the boundary is online.
	rec := batchDiscover(t, router, map[string]any{
		"service_names":     []any{"svc"},
		"evaluate_at":       time.Date(2026, 10, 1, 12, 5, 0, 0, time.UTC).Unix(),
		"heartbeat_timeout": "5m",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	services := out["services"].([]any)
	hits := services[0].(map[string]any)["instances"].([]any)
	if len(hits) != 1 {
		t.Fatalf("boundary instance must stay online: %v", hits)
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("heartbeat_timeout = %v, want 300", out["heartbeat_timeout"])
	}
	if _, found, err := st.GetInstance("svc", "i-edge"); err != nil || !found {
		t.Fatalf("boundary record must be kept: found=%v err=%v", found, err)
	}
}

func TestBatchDiscoverRejectsInvalidBodies(t *testing.T) {
	router, st := testRouter(t)
	registerBatchFixture(t, router)

	valid := `{"service_names":["svc-a"],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":300}`
	cases := map[string]string{
		"empty body":                "",
		"json array":                `["svc-a"]`,
		"json string":               `"svc-a"`,
		"json number":               `42`,
		"json null":                 `null`,
		"missing service_names":     `{"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":300}`,
		"empty service_names":       `{"service_names":[],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":300}`,
		"blank service name":        `{"service_names":["  "],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":300}`,
		"duplicate service names":   `{"service_names":["svc-a"," svc-a "],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":300}`,
		"non-string service name":   `{"service_names":[7],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":300}`,
		"missing evaluate_at":       `{"service_names":["svc-a"],"heartbeat_timeout":300}`,
		"unparseable evaluate_at":   `{"service_names":["svc-a"],"evaluate_at":"tomorrow","heartbeat_timeout":300}`,
		"missing heartbeat_timeout": `{"service_names":["svc-a"],"evaluate_at":"2026-10-01T12:05:00Z"}`,
		"zero heartbeat_timeout":    `{"service_names":["svc-a"],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":0}`,
		"negative duration":         `{"service_names":["svc-a"],"evaluate_at":"2026-10-01T12:05:00Z","heartbeat_timeout":"-10s"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doRawRequest(t, router, http.MethodPost, "/api/v1/discover/batch", raw)
			expectParameterError(t, rec)
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("error body must contain only error object: %v", out)
			}
			if _, found, err := st.GetInstance("svc-b", "i-b1"); err != nil || !found {
				t.Fatalf("rejected batch must not delete records: found=%v err=%v", found, err)
			}
		})
	}

	// Sanity check that the valid counterpart succeeds.
	if rec := doRawRequest(t, router, http.MethodPost, "/api/v1/discover/batch", valid); rec.Code != http.StatusOK {
		t.Fatalf("valid body status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBatchDiscoverClockSkewRejectsAndKeepsRecords(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "late", "instance_id": "i-late", "weight": 1,
		"healthy": true, "heartbeat_at": at(10),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "old", "instance_id": "i-old", "weight": 1,
		"healthy": true, "heartbeat_at": at(0),
	})

	// i-old would be lost at 12:05 with a 60s timeout, but i-late's
	// heartbeat is later than evaluate_at: the whole batch is rejected and
	// neither record may be deleted.
	rec := batchDiscover(t, router, map[string]any{
		"service_names":     []any{"old", "late"},
		"evaluate_at":       at(5),
		"heartbeat_timeout": 60,
	})
	expectParameterError(t, rec)
	if _, found, err := st.GetInstance("late", "i-late"); err != nil || !found {
		t.Fatalf("late record must be kept: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetInstance("old", "i-old"); err != nil || !found {
		t.Fatalf("would-be-lost record must be kept on rejection: found=%v err=%v", found, err)
	}
}

func TestBatchDiscoverStorageUnavailable(t *testing.T) {
	router, st := testRouter(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := batchDiscover(t, router, map[string]any{
		"service_names":     []any{"svc"},
		"evaluate_at":       at(5),
		"heartbeat_timeout": 300,
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["error"].(map[string]any)["code"] != "storage_unavailable" {
		t.Fatalf("code = %v", out["error"])
	}
}
