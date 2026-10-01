package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

func heartbeatRecord() map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  "i-1",
		"address":      "10.0.0.8:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       10,
		"heartbeat_at": at(0),
	}
}

func expectErrorShape(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, wantStatus, rec.Body.String())
	}
	out := decodeBody(t, rec)
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no error object: %s", rec.Body.String())
	}
	if errObj["code"] != wantCode {
		t.Fatalf("code = %v, want %s", errObj["code"], wantCode)
	}
	message, _ := errObj["message"].(string)
	if message == "" {
		t.Fatalf("error message is empty: %s", rec.Body.String())
	}
	lowered := strings.ToLower(message)
	for _, banned := range []string{"sql", "select ", "update ", ".go:", "/", "\n"} {
		if strings.Contains(lowered, banned) {
			t.Fatalf("message leaks internal detail %q: %s", banned, message)
		}
	}
}

func TestHeartbeatReplacesOnlyHeartbeatAt(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": 1759324800})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	instance := out["instance"].(map[string]any)
	if instance["service_name"] != "svc" || instance["instance_id"] != "i-1" ||
		instance["address"] != "10.0.0.8:8080" || instance["port"].(float64) != 8080 ||
		instance["healthy"] != true || instance["weight"].(float64) != 10 {
		t.Fatalf("fields changed by heartbeat: %v", instance)
	}
	wantTime := time.Unix(1759324800, 0).UTC().Format(time.RFC3339)
	if instance["heartbeat_at"] != wantTime {
		t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], wantTime)
	}

	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !got.HeartbeatAt.Equal(time.Unix(1759324800, 0).UTC()) {
		t.Fatalf("stored heartbeat = %v", got.HeartbeatAt)
	}
}

func TestHeartbeatAcceptsRFC3339(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": at(12)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	wantTime, _ := time.Parse(time.RFC3339, at(12))
	parsed, err := time.Parse(time.RFC3339Nano, instance["heartbeat_at"].(string))
	if err != nil || !parsed.Equal(wantTime) {
		t.Fatalf("heartbeat_at = %v", instance["heartbeat_at"])
	}
}

func TestHeartbeatMissingInstanceReturns404(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/gone/heartbeat",
		map[string]any{"heartbeat_at": at(5)})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("existing record changed after 404: %+v", got)
	}
}

func TestHeartbeatValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	cases := []struct {
		name string
		path string
		body any
	}{
		{"missing heartbeat", "/api/v1/services/svc/instances/i-1/heartbeat", map[string]any{}},
		{"unparseable heartbeat", "/api/v1/services/svc/instances/i-1/heartbeat", map[string]any{"heartbeat_at": "not-a-time"}},
		{"empty body", "/api/v1/services/svc/instances/i-1/heartbeat", nil},
		{"empty service name", "/api/v1/services//instances/i-1/heartbeat", map[string]any{"heartbeat_at": at(5)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPost, tc.path, tc.body)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			got, found, _ := st.GetInstance("svc", "i-1")
			if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
				t.Fatalf("record changed after invalid request: %+v", got)
			}
		})
	}
}

func TestBatchHeartbeatUpdatesInRequestOrder(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())
	second := heartbeatRecord()
	second["instance_id"] = "i-2"
	second["weight"] = 3
	second["heartbeat_at"] = at(1)
	registerInstance(t, router, second)
	other := heartbeatRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-2", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "i-1", "heartbeat_at": 1759324800},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
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
	if first["instance_id"] != "i-2" || secondEntry["instance_id"] != "i-1" {
		t.Fatalf("order not preserved: %v %v", first["instance_id"], secondEntry["instance_id"])
	}
	if first["healthy"] != true || first["address"] != "10.0.0.8:8080" ||
		first["weight"].(float64) != 3 || first["port"].(float64) != 8080 {
		t.Fatalf("batch changed non-heartbeat fields: %v", first)
	}

	want20, _ := time.Parse(time.RFC3339, at(20))
	if parsed, _ := time.Parse(time.RFC3339Nano, first["heartbeat_at"].(string)); !parsed.Equal(want20) {
		t.Fatalf("i-2 heartbeat = %v", first["heartbeat_at"])
	}
	if secondEntry["heartbeat_at"] != time.Unix(1759324800, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("i-1 heartbeat = %v", secondEntry["heartbeat_at"])
	}

	peer, _, _ := st.GetInstance("other", "i-1")
	if !peer.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("other service record touched: %+v", peer)
	}
}

func TestBatchHeartbeatValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	cases := []struct {
		name string
		body any
	}{
		{"missing service name", map[string]any{"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(5)}}}},
		{"empty service name", map[string]any{"service_name": "  ", "instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(5)}}}},
		{"missing instances", map[string]any{"service_name": "svc"}},
		{"empty instances", map[string]any{"service_name": "svc", "instances": []any{}}},
		{"instances not a list", map[string]any{"service_name": "svc", "instances": "i-1"}},
		{"entry missing instance id", map[string]any{"service_name": "svc", "instances": []any{
			map[string]any{"heartbeat_at": at(5)}}}},
		{"entry blank instance id", map[string]any{"service_name": "svc", "instances": []any{
			map[string]any{"instance_id": "  ", "heartbeat_at": at(5)}}}},
		{"duplicate instance id", map[string]any{"service_name": "svc", "instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(5)},
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(6)}}}},
		{"missing heartbeat", map[string]any{"service_name": "svc", "instances": []any{
			map[string]any{"instance_id": "i-1"}}}},
		{"unparseable heartbeat", map[string]any{"service_name": "svc", "instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": "nope"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", tc.body)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			got, found, _ := st.GetInstance("svc", "i-1")
			if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
				t.Fatalf("record changed after invalid batch: %+v", got)
			}
		})
	}
}

func TestBatchHeartbeatMissingInstanceRollsBack(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())
	second := heartbeatRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "ghost", "heartbeat_at": at(21)},
			map[string]any{"instance_id": "i-2", "heartbeat_at": at(22)},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	for _, id := range []string{"i-1", "i-2"} {
		got, found, _ := st.GetInstance("svc", id)
		if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
			t.Fatalf("%s changed after partial miss: %+v", id, got)
		}
	}
}

func TestHeartbeatStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	registerInstance(t, router, heartbeatRecord())
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": at(5)})
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")

	rec = doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "svc",
		"instances":    []any{map[string]any{"instance_id": "i-1", "heartbeat_at": at(5)}},
	})
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
}

func TestHeartbeatKeepsDiscoveryCleanupSemantics(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, heartbeatRecord())
	second := heartbeatRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": at(9)})
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}

	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil))
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "i-1" {
		t.Fatalf("ids = %v, want [i-1]", ids)
	}
}

func mustParseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
