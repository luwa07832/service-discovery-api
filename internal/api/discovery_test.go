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

func testRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), st
}

func doRequest(t *testing.T, router http.Handler, method, target string, body any) *httptest.ResponseRecorder {
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

func registerInstance(t *testing.T, router http.Handler, body map[string]any) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("register %v: status %d body %s", body, rec.Code, rec.Body.String())
	}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

const baseTime = "2026-10-01T12:00:00Z"

func at(minutes int) string {
	return time.Date(2026, 10, 1, 12, minutes, 0, 0, time.UTC).Format(time.RFC3339)
}

func instanceIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	rawList, ok := out["instances"].([]any)
	if !ok {
		t.Fatalf("instances is not a list: %v", out)
	}
	ids := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		item := raw.(map[string]any)
		ids = append(ids, item["instance_id"].(string))
	}
	return ids
}

func expectParameterError(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "invalid_parameter" {
		t.Fatalf("code = %v, want invalid_parameter", errObj["code"])
	}
}

func TestRegisterPersistsAndPublicQueriesReadIt(t *testing.T) {
	router, st := testRouter(t)
	body := map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.1:9000",
		"healthy": true, "weight": 7, "heartbeat_at": baseTime,
	}
	registerInstance(t, router, body)

	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("stored record missing: found=%v err=%v", found, err)
	}
	if got.Address != "10.0.0.1:9000" || got.Weight != 7 || !got.Healthy {
		t.Fatalf("stored record mismatch: %+v", got)
	}

	for _, target := range []string{
		"/api/v1/services/svc/instances/i-1",
		"/api/v1/instances?service_name=svc&instance_id=i-1",
	} {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		instance := out["instance"].(map[string]any)
		if instance["address"] != "10.0.0.1:9000" || instance["weight"].(float64) != 7 {
			t.Fatalf("%s unexpected instance %v", target, instance)
		}
	}

	health := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1/health", nil))
	if health["healthy"] != true || health["health"] != "healthy" {
		t.Fatalf("health query mismatch: %v", health)
	}
	weight := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1/weight", nil))
	if weight["weight"].(float64) != 7 {
		t.Fatalf("weight query mismatch: %v", weight)
	}
	heartbeat := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1/heartbeat", nil))
	if heartbeat["heartbeat_at"] != baseTime {
		t.Fatalf("heartbeat query mismatch: %v", heartbeat)
	}
}

func TestRepeatedRegistrationOverwritesRecord(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "old",
		"healthy": true, "weight": 1, "heartbeat_at": at(0),
	})
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1", map[string]any{
			"address": "new", "healthy": false, "weight": 3, "heartbeat_at": at(5),
		})
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	list, err := st.ListInstances("svc")
	if err != nil || len(list) != 1 {
		t.Fatalf("expected one record, got %d err=%v", len(list), err)
	}
	if list[0].Address != "new" || list[0].Healthy || list[0].Weight != 3 {
		t.Fatalf("record not overwritten: %+v", list[0])
	}
}

func TestRegisterValidationCreatesNothing(t *testing.T) {
	router, st := testRouter(t)
	cases := []map[string]any{
		{"instance_id": "i", "weight": 1, "heartbeat_at": baseTime},
		{"service_name": "svc", "weight": 1, "heartbeat_at": baseTime},
		{"service_name": "svc", "instance_id": "i", "heartbeat_at": baseTime},
		{"service_name": "svc", "instance_id": "i", "weight": -1, "heartbeat_at": baseTime},
		{"service_name": "svc", "instance_id": "i", "weight": "abc", "heartbeat_at": baseTime},
		{"service_name": "svc", "instance_id": "i", "weight": 1},
		{"service_name": "svc", "instance_id": "i", "weight": 1, "heartbeat_at": "not-a-time"},
		{"service_name": "svc", "instance_id": "i", "weight": 1, "heartbeat_at": baseTime, "evaluate_at": at(-1)},
	}
	for index, body := range cases {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/register", body)
		expectParameterError(t, rec)
		if list, err := st.ListInstances("svc"); err != nil || len(list) != 0 {
			t.Fatalf("case %d changed storage: %d rows err=%v", index, len(list), err)
		}
	}
}

func TestDiscoverFiltersUnhealthyAndLostInstances(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "lost-old", "address": "a",
		"healthy": true, "weight": 10, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "fresh", "address": "b",
		"healthy": true, "weight": 1, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "unhealthy-fresh", "address": "c",
		"healthy": false, "weight": 10, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "newest", "address": "d",
		"healthy": true, "weight": 5, "heartbeat_at": at(10),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "other", "instance_id": "must-not-appear",
		"healthy": true, "weight": 100, "heartbeat_at": at(9),
	})

	// The boundary is inclusive: evaluate 12:10 with timeout 10m puts the
	// lost boundary at exactly 12:00, so the 12:00 heartbeat is still online.
	target := "/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=10m"
	out := decodeBody(t, doRequest(t, router, http.MethodGet, target, nil))
	ids := instanceIDs(t, out)
	want := []string{"lost-old", "newest", "fresh"}
	if len(ids) != len(want) {
		t.Fatalf("boundary ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("boundary ids = %v, want %v", ids, want)
		}
	}
	// Nothing was strictly past the boundary: every record remains stored.
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 4 {
		t.Fatalf("boundary query changed storage: %d rows err=%v", len(list), err)
	}

	// Strictly past the boundary evicts and deletes: at 12:10:30 with 10m
	// the boundary is 12:00:30, so the 12:00 record is lost.
	evaluate := time.Date(2026, 10, 1, 12, 10, 30, 0, time.UTC).Format(time.RFC3339)
	target = "/api/v1/discover?service_name=svc&evaluate_at=" + evaluate + "&heartbeat_timeout=10m"
	out = decodeBody(t, doRequest(t, router, http.MethodGet, target, nil))
	if ids := instanceIDs(t, out); len(ids) != 2 || ids[0] != "newest" || ids[1] != "fresh" {
		t.Fatalf("after-loss ids = %v, want [newest fresh]", ids)
	}
	if _, found, _ := st.GetInstance("svc", "lost-old"); found {
		t.Fatalf("lost record was not deleted")
	}
	// Unhealthy but fresh records stay stored; other services are untouched.
	if _, found, _ := st.GetInstance("svc", "unhealthy-fresh"); !found {
		t.Fatalf("unhealthy fresh record was removed")
	}
	if other, err := st.ListInstances("other"); err != nil || len(other) != 1 {
		t.Fatalf("other service changed: %d rows err=%v", len(other), err)
	}
}

func TestDiscoverDeterministicOrdering(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "low-weight", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "same-weight-newer", "healthy": true,
		"weight": 5, "heartbeat_at": at(8),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "same-weight-older", "healthy": true,
		"weight": 5, "heartbeat_at": at(5),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "tie-a", "healthy": true,
		"weight": 5, "heartbeat_at": at(8),
	})

	target := "/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=10m"
	out := decodeBody(t, doRequest(t, router, http.MethodGet, target, nil))
	ids := instanceIDs(t, out)
	// Weight descending; equal weights order by instance id ascending,
	// regardless of heartbeat recency.
	want := []string{"same-weight-newer", "same-weight-older", "tie-a", "low-weight"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}

	// Result items carry the documented fields.
	items := out["instances"].([]any)
	first := items[0].(map[string]any)
	for _, key := range []string{"instance_id", "address", "port", "healthy", "weight", "heartbeat_at"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("result item missing %s: %v", key, first)
		}
	}
}

func TestDiscoverEmptyResults(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	for _, target := range []string{
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=1m",
		"/api/v1/discover?service_name=unknown&evaluate_at=" + at(10) + "&heartbeat_timeout=1m",
	} {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", target, rec.Code)
		}
		out := decodeBody(t, rec)
		if ids := instanceIDs(t, out); len(ids) != 0 {
			t.Fatalf("%s ids = %v, want empty", target, ids)
		}
	}
}

func TestDiscoverValidationTouchesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i", "healthy": true,
		"weight": 1, "heartbeat_at": at(5),
	})
	goodEvaluate := at(10)
	cases := []string{
		"/api/v1/discover?evaluate_at=" + goodEvaluate + "&heartbeat_timeout=1m",
		"/api/v1/discover?service_name=svc&heartbeat_timeout=1m",
		"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate,
		"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate + "&heartbeat_timeout=0",
		"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate + "&heartbeat_timeout=-5s",
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(4) + "&heartbeat_timeout=1m",
	}
	for _, target := range cases {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		expectParameterError(t, rec)
		if list, err := st.ListInstances("svc"); err != nil || len(list) != 1 {
			t.Fatalf("%s changed storage: %d rows err=%v", target, len(list), err)
		}
	}
}

func TestDiscoverIsScopedToService(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc-a", "instance_id": "a1", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc-b", "instance_id": "b1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	target := "/api/v1/discover?service_name=svc-a&evaluate_at=" + at(10) + "&heartbeat_timeout=10m"
	out := decodeBody(t, doRequest(t, router, http.MethodGet, target, nil))
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "a1" {
		t.Fatalf("cross-service leakage: %v", ids)
	}
}

func TestDeleteEntryRemovesOnlyNamedInstance(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": baseTime,
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-2", "healthy": true,
		"weight": 1, "heartbeat_at": baseTime,
	})

	rec := doRequest(t, router, http.MethodDelete,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatalf("deleted instance still stored")
	}
	if _, found, _ := st.GetInstance("svc", "i-2"); !found {
		t.Fatalf("sibling instance was removed")
	}

	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing instance status = %d, want 404", rec.Code)
	}

	rec = doRequest(t, router, http.MethodDelete,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rec.Code)
	}

	// Invalid delete parameters change nothing.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/deregister",
		map[string]any{"service_name": "svc"})
	expectParameterError(t, rec)
	if list, _ := st.ListInstances("svc"); len(list) != 1 {
		t.Fatalf("invalid delete changed storage: %d rows", len(list))
	}
}

func TestDiscoverAcceptsJSONBody(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})
	rec := doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": 300,
	})
	if rec.Code != 200 {
		t.Fatalf("post discover: %d %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "i-1" {
		t.Fatalf("post discover ids = %v", ids)
	}
}

func TestEmptyServiceNamePathIsParameterError(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": baseTime,
	})
	targets := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/v1/services//instances", map[string]any{
			"instance_id": "x", "weight": 1, "heartbeat_at": baseTime}},
		{http.MethodGet, "/api/v1/services//instances", nil},
		{http.MethodGet, "/api/v1/services//discover?evaluate_at=" + at(10) + "&heartbeat_timeout=1m", nil},
		{http.MethodDelete, "/api/v1/services//instances/i-1", nil},
	}
	for _, item := range targets {
		rec := doRequest(t, router, item.method, item.path, item.body)
		expectParameterError(t, rec)
	}
	if list, _ := st.ListInstances("svc"); len(list) != 1 {
		t.Fatalf("empty-name requests changed storage: %d rows", len(list))
	}
}

func TestRegisterRejectsNonPositiveWeight(t *testing.T) {
	router, st := testRouter(t)
	cases := []map[string]any{
		{"service_name": "svc", "instance_id": "i-0", "weight": 0, "heartbeat_at": baseTime},
		{"service_name": "svc", "instance_id": "i-neg", "weight": -2, "heartbeat_at": baseTime},
		{"service_name": "svc", "instance_id": "i-str", "weight": "0", "heartbeat_at": baseTime},
	}
	for index, body := range cases {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/register", body)
		expectParameterError(t, rec)
		if list, err := st.ListInstances("svc"); err != nil || len(list) != 0 {
			t.Fatalf("case %d changed storage: %d rows err=%v", index, len(list), err)
		}
	}

	// A rejected update keeps the stored record unchanged.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 5, "heartbeat_at": baseTime,
	})
	rec := doRequest(t, router, http.MethodPut, "/api/v1/services/svc/instances/i-1",
		map[string]any{"weight": 0, "heartbeat_at": at(5)})
	expectParameterError(t, rec)
	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("record missing: found=%v err=%v", found, err)
	}
	wantHeartbeat, _ := time.Parse(time.RFC3339, baseTime)
	if got.Weight != 5 || !got.HeartbeatAt.Equal(wantHeartbeat) {
		t.Fatalf("rejected update changed record: %+v", got)
	}
}

func TestRegisterAcceptsOnlyBooleanHealth(t *testing.T) {
	router, st := testRouter(t)
	for index, value := range []any{"healthy", "unhealthy", "yes", "true", "1", 1, 0} {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/register", map[string]any{
			"service_name": "svc", "instance_id": "i", "weight": 1,
			"heartbeat_at": baseTime, "healthy": value,
		})
		expectParameterError(t, rec)
		if list, err := st.ListInstances("svc"); err != nil || len(list) != 0 {
			t.Fatalf("case %d changed storage: %d rows err=%v", index, len(list), err)
		}
	}

	// Boolean true/false are the only accepted explicit values.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i", "healthy": true,
		"weight": 1, "heartbeat_at": baseTime,
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i", "healthy": false,
		"weight": 1, "heartbeat_at": baseTime,
	})
	if got, _, _ := st.GetInstance("svc", "i"); got.Healthy {
		t.Fatalf("healthy=false not stored: %+v", got)
	}
	// Omitted health keeps the documented default of false.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "j", "weight": 1, "heartbeat_at": baseTime,
	})
	if got, _, _ := st.GetInstance("svc", "j"); got.Healthy {
		t.Fatalf("default health changed: %+v", got)
	}
}

func TestRegisterStoresPortAndDiscoverReturnsIt(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.1",
		"port": 8080, "healthy": true, "weight": 2, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-2",
		"healthy": true, "weight": 1, "heartbeat_at": at(9),
	})

	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found || got.Port != 8080 {
		t.Fatalf("stored port mismatch: %+v found=%v err=%v", got, found, err)
	}
	rec := doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances/i-1", nil)
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["port"].(float64) != 8080 {
		t.Fatalf("get instance port = %v", instance["port"])
	}

	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=5m", nil))
	items := out["instances"].([]any)
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["instance_id"] != "i-1" || first["port"].(float64) != 8080 {
		t.Fatalf("discover first item = %v", first)
	}
	if second["instance_id"] != "i-2" || second["port"].(float64) != 0 {
		t.Fatalf("discover default port = %v", second)
	}

	// Invalid ports are rejected and change nothing.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register", map[string]any{
		"service_name": "svc", "instance_id": "i-3", "port": -1,
		"weight": 1, "heartbeat_at": baseTime,
	})
	expectParameterError(t, rec)
	if list, _ := st.ListInstances("svc"); len(list) != 2 {
		t.Fatalf("invalid port changed storage: %d rows", len(list))
	}
}

func TestDiscoverBoundaryKeepsEqualAndEvictsStrictlyLater(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "edge", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	// Heartbeat exactly at heartbeat_at + timeout is still online.
	target := "/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=10m"
	out := decodeBody(t, doRequest(t, router, http.MethodGet, target, nil))
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "edge" {
		t.Fatalf("boundary ids = %v, want [edge]", ids)
	}
	if _, found, _ := st.GetInstance("svc", "edge"); !found {
		t.Fatalf("boundary query deleted an online record")
	}

	// One second past the boundary the instance is lost: excluded and deleted.
	evaluate := time.Date(2026, 10, 1, 12, 10, 1, 0, time.UTC).Format(time.RFC3339)
	target = "/api/v1/discover?service_name=svc&evaluate_at=" + evaluate + "&heartbeat_timeout=10m"
	out = decodeBody(t, doRequest(t, router, http.MethodGet, target, nil))
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("past-boundary ids = %v, want []", ids)
	}
	if _, found, _ := st.GetInstance("svc", "edge"); found {
		t.Fatalf("lost record was not deleted")
	}
}

func TestHeartbeatRestoresOnlyThatInstance(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "a", "healthy": true,
		"weight": 5, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "b", "healthy": true,
		"weight": 8, "heartbeat_at": at(0),
	})

	// A new heartbeat for one instance must not revive the other lost one.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "a", "healthy": true,
		"weight": 5, "heartbeat_at": at(10),
	})
	peer, found, err := st.GetInstance("svc", "b")
	if err != nil || !found {
		t.Fatalf("peer record missing: found=%v err=%v", found, err)
	}
	wantHeartbeat, _ := time.Parse(time.RFC3339, at(0))
	if !peer.HeartbeatAt.Equal(wantHeartbeat) {
		t.Fatalf("heartbeat touched another instance: %+v", peer)
	}

	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil))
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("ids = %v, want [a]", ids)
	}
	if _, found, _ := st.GetInstance("svc", "b"); found {
		t.Fatalf("lost peer was not cleaned up")
	}
}

func TestRepeatedStateChangesKeepOnlyLatest(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "a", "healthy": true,
		"weight": 10, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "b", "healthy": true,
		"weight": 5, "heartbeat_at": at(9),
	})
	discover := func() []string {
		out := decodeBody(t, doRequest(t, router, http.MethodGet,
			"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=5m", nil))
		return instanceIDs(t, out)
	}
	if ids := discover(); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("initial ids = %v, want [a b]", ids)
	}

	// Lowering the weight of the same instance id only changes the order.
	rec := doRequest(t, router, http.MethodPut, "/api/v1/services/svc/instances/a",
		map[string]any{"healthy": true, "weight": 1, "heartbeat_at": at(9)})
	if rec.Code != 200 {
		t.Fatalf("weight update: %d %s", rec.Code, rec.Body.String())
	}
	if list, _ := st.ListInstances("svc"); len(list) != 2 {
		t.Fatalf("duplicate instance rows: %d", len(list))
	}
	if ids := discover(); len(ids) != 2 || ids[0] != "b" || ids[1] != "a" {
		t.Fatalf("after weight drop ids = %v, want [b a]", ids)
	}

	// Toggling health removes the instance from hits; a fresh heartbeat with
	// healthy=true restores it. Only the latest state survives.
	rec = doRequest(t, router, http.MethodPut, "/api/v1/services/svc/instances/a",
		map[string]any{"healthy": false, "weight": 1, "heartbeat_at": at(9)})
	if rec.Code != 200 {
		t.Fatalf("health update: %d %s", rec.Code, rec.Body.String())
	}
	if ids := discover(); len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("after unhealthy ids = %v, want [b]", ids)
	}
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "a", "healthy": true,
		"weight": 1, "heartbeat_at": at(10),
	})
	if ids := discover(); len(ids) != 2 || ids[0] != "b" || ids[1] != "a" {
		t.Fatalf("after heartbeat ids = %v, want [b a]", ids)
	}
	if list, _ := st.ListInstances("svc"); len(list) != 2 {
		t.Fatalf("state changes duplicated rows: %d", len(list))
	}
}

func TestDiscoverDeletesOnlyLostRecords(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "lost", "healthy": true,
		"weight": 10, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "fresh", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "unhealthy-fresh", "healthy": false,
		"weight": 5, "heartbeat_at": at(9),
	})

	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil))
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "fresh" {
		t.Fatalf("ids = %v, want [fresh]", ids)
	}

	// The lost record is deleted by the cleanup; online and unhealthy but
	// fresh records remain stored and visible through the public list entry.
	list := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances", nil))
	if ids := instanceIDs(t, list); len(ids) != 2 {
		t.Fatalf("public list after cleanup = %v, want 2 entries", ids)
	}
	stored, err := st.ListInstances("svc")
	if err != nil || len(stored) != 2 {
		t.Fatalf("storage rows = %d err=%v", len(stored), err)
	}
	if _, found, _ := st.GetInstance("svc", "lost"); found {
		t.Fatalf("lost record still stored")
	}
	if _, found, _ := st.GetInstance("svc", "unhealthy-fresh"); !found {
		t.Fatalf("unhealthy fresh record was removed")
	}
}
