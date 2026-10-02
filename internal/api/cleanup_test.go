package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runCleanup(t *testing.T, router http.Handler, target string, body any) map[string]any {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, target, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup %s: status %d body %s", target, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func removedInstances(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	rawList, ok := out["instances"].([]any)
	if !ok {
		t.Fatalf("instances is not a list: %v", out)
	}
	instances := make([]map[string]any, 0, len(rawList))
	for _, raw := range rawList {
		instances = append(instances, raw.(map[string]any))
	}
	return instances
}

func TestCleanupRemovesStrictlyLostAcrossServices(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc-b", "instance_id": "gone", "address": "10.0.0.1:8080",
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc-a", "instance_id": "lost", "healthy": true,
		"weight": 7, "heartbeat_at": at(2),
	})
	// Exactly on the lost boundary: still online, must be kept.
	registerInstance(t, router, map[string]any{
		"service_name": "svc-a", "instance_id": "boundary", "healthy": true,
		"weight": 5, "heartbeat_at": at(5),
	})
	// Unhealthy but fresh: must be kept.
	registerInstance(t, router, map[string]any{
		"service_name": "svc-a", "instance_id": "sick", "healthy": false,
		"weight": 3, "heartbeat_at": at(9),
	})

	out := runCleanup(t, router,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=5m", nil)

	if out["evaluate_at"] != at(10) {
		t.Fatalf("evaluate_at = %v, want %s", out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("heartbeat_timeout = %v, want 300", out["heartbeat_timeout"])
	}
	if out["removed"].(float64) != 2 {
		t.Fatalf("removed = %v, want 2", out["removed"])
	}

	instances := removedInstances(t, out)
	if len(instances) != 2 {
		t.Fatalf("instances = %v, want two entries", instances)
	}
	// Sorted by service name, then instance id.
	if instances[0]["service_name"] != "svc-a" || instances[0]["instance_id"] != "lost" ||
		instances[1]["service_name"] != "svc-b" || instances[1]["instance_id"] != "gone" {
		t.Fatalf("removed order = %v", instances)
	}
	// Removed entries are the full instance records.
	gone := instances[1]
	if gone["address"] != "10.0.0.1:8080" || gone["port"].(float64) != 8080 ||
		gone["healthy"] != true || gone["weight"].(float64) != 10 ||
		gone["heartbeat_at"] != at(0) {
		t.Fatalf("removed record incomplete: %v", gone)
	}

	// Only the strictly lost records are gone from storage.
	if _, found, _ := st.GetInstance("svc-a", "lost"); found {
		t.Fatalf("lost record still stored")
	}
	if _, found, _ := st.GetInstance("svc-b", "gone"); found {
		t.Fatalf("lost record still stored")
	}
	kept, err := st.ListInstances("svc-a")
	if err != nil || len(kept) != 2 {
		t.Fatalf("svc-a kept %d records, want 2, err=%v", len(kept), err)
	}
	for _, id := range []string{"boundary", "sick"} {
		if _, found, _ := st.GetInstance("svc-a", id); !found {
			t.Fatalf("%s must be kept", id)
		}
	}
}

func TestCleanupScopedToNamedService(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc-a", "instance_id": "lost-a", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc-b", "instance_id": "lost-b", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc-b", "instance_id": "fresh-b", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})

	out := runCleanup(t, router,
		"/api/v1/cleanup?service_name=svc-b&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if out["removed"].(float64) != 1 {
		t.Fatalf("removed = %v, want 1", out["removed"])
	}
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "lost-b" {
		t.Fatalf("removed ids = %v, want [lost-b]", ids)
	}

	// The other service's records are not affected, even strictly lost ones.
	if _, found, _ := st.GetInstance("svc-a", "lost-a"); !found {
		t.Fatalf("scoped cleanup touched another service")
	}
	if list, err := st.ListInstances("svc-b"); err != nil || len(list) != 1 ||
		list[0].InstanceID != "fresh-b" {
		t.Fatalf("svc-b records = %v, err=%v", list, err)
	}
}

func TestCleanupWithoutLostRecordsReturnsEmptyList(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "fresh", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["removed"].(float64) != 0 {
		t.Fatalf("removed = %v, want 0", out["removed"])
	}
	if !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("instances must be an empty array, body %s", rec.Body.String())
	}
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 1 {
		t.Fatalf("empty cleanup changed records: %d err=%v", len(list), err)
	}
}

func TestCleanupBoundaryAndSkewEdgeCases(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})

	// evaluate_at equal to the stored heartbeat is not a skew error.
	out := runCleanup(t, router,
		"/api/v1/cleanup?evaluate_at="+at(9)+"&heartbeat_timeout=30s", nil)
	if out["removed"].(float64) != 0 {
		t.Fatalf("removed = %v, want 0", out["removed"])
	}
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 1 {
		t.Fatalf("boundary cleanup changed records: %d err=%v", len(list), err)
	}
}

func TestCleanupAcceptsJSONBodyAndUnixSeconds(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "gone", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	evaluate, err := time.Parse(time.RFC3339, at(10))
	if err != nil {
		t.Fatalf("parse evaluate: %v", err)
	}
	out := runCleanup(t, router, "/api/v1/cleanup", map[string]any{
		"service_name":      "svc",
		"evaluate_at":       strconv.FormatInt(evaluate.Unix(), 10),
		"heartbeat_timeout": "2m",
	})
	if out["removed"].(float64) != 1 || out["heartbeat_timeout"].(float64) != 120 {
		t.Fatalf("cleanup via JSON body = %v", out)
	}
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 0 {
		t.Fatalf("lost record not removed: %d err=%v", len(list), err)
	}
}

func TestCleanupParameterErrorsChangeNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "lost", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "fresh", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})

	queries := []string{
		"heartbeat_timeout=1m",
		"evaluate_at=not-a-time&heartbeat_timeout=1m",
		"evaluate_at=" + at(10),
		"evaluate_at=" + at(10) + "&heartbeat_timeout=0",
		"evaluate_at=" + at(10) + "&heartbeat_timeout=-5",
		"evaluate_at=" + at(10) + "&heartbeat_timeout=soon",
		"service_name=&evaluate_at=" + at(10) + "&heartbeat_timeout=1m",
		"service_name=%20%20&evaluate_at=" + at(10) + "&heartbeat_timeout=1m",
		// evaluate_at earlier than a stored heartbeat is rejected.
		"evaluate_at=" + at(5) + "&heartbeat_timeout=1m",
		"service_name=svc&evaluate_at=" + at(5) + "&heartbeat_timeout=1m",
	}
	for _, query := range queries {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/cleanup?"+query, nil)
		expectParameterError(t, rec)
	}
	bodies := []map[string]any{
		{"service_name": "", "evaluate_at": at(10), "heartbeat_timeout": "1m"},
		{"evaluate_at": at(10), "heartbeat_timeout": "1m", "service_name": "   "},
		{"evaluate_at": at(10)},
	}
	for _, body := range bodies {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/cleanup", body)
		expectParameterError(t, rec)
	}

	if list, err := st.ListInstances("svc"); err != nil || len(list) != 2 {
		t.Fatalf("rejected cleanup changed records: %d err=%v", len(list), err)
	}
}

func TestCleanupSkewCheckScopesToEvaluatedService(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc-a", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc-b", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(20),
	})

	// Scoped to svc-a, svc-b's later heartbeat is not evaluated.
	out := runCleanup(t, router,
		"/api/v1/cleanup?service_name=svc-a&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if out["removed"].(float64) != 0 {
		t.Fatalf("removed = %v, want 0", out["removed"])
	}

	// Unscoped, svc-b's later heartbeat makes the request a skew error.
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	expectParameterError(t, rec)

	if list, err := st.ListAllInstances(); err != nil || len(list) != 2 {
		t.Fatalf("records changed: %d err=%v", len(list), err)
	}
}

func TestCleanupStorageUnavailable(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "gone", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "storage_unavailable" {
		t.Fatalf("code = %v, want storage_unavailable", errObj["code"])
	}
}

func TestCleanupKeepsOtherEntriesUntouched(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "gone", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	out := runCleanup(t, router,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if out["removed"].(float64) != 1 {
		t.Fatalf("removed = %v, want 1", out["removed"])
	}

	// The overview still sees a consistent, now empty, store and stays
	// read-only; discovery keeps its own per-service cleanup semantics.
	overview := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil))
	if list := overview["services"].([]any); len(list) != 0 {
		t.Fatalf("overview after cleanup = %v, want empty", list)
	}
	if list, err := st.ListAllInstances(); err != nil || len(list) != 0 {
		t.Fatalf("store after cleanup = %d records, err=%v", len(list), err)
	}
}
