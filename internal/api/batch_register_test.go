package api

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

func batchEntry(instanceID string, weight any, heartbeat any) map[string]any {
	return map[string]any{
		"instance_id":  instanceID,
		"weight":       weight,
		"heartbeat_at": heartbeat,
	}
}

func TestBatchRegisterCreatesInstancesInRequestOrder(t *testing.T) {
	router, st := testRouter(t)

	first := batchEntry("i-1", 10, at(0))
	first["address"] = "10.0.0.8:8080"
	first["port"] = 8080
	first["healthy"] = true
	second := batchEntry("i-2", "2.5", 1759324800)
	body := map[string]any{"instances": []any{second, first}}

	rec := doRequest(t, router, http.MethodPost, "/api/v1/services/svc/instances/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "svc" {
		t.Fatalf("service_name = %v, want svc", out["service_name"])
	}
	if out["registered"].(float64) != 2 {
		t.Fatalf("registered = %v, want 2", out["registered"])
	}
	if ids := instanceIDs(t, out); ids[0] != "i-2" || ids[1] != "i-1" {
		t.Fatalf("order not preserved: %v", ids)
	}
	entries := out["instances"].([]any)
	lead := entries[0].(map[string]any)
	if lead["address"] != "" || lead["port"].(float64) != 0 || lead["healthy"] != false {
		t.Fatalf("defaults not applied: %v", lead)
	}
	if lead["weight"].(float64) != 2.5 {
		t.Fatalf("string weight = %v, want 2.5", lead["weight"])
	}
	if lead["heartbeat_at"] != time.Unix(1759324800, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("unix heartbeat = %v", lead["heartbeat_at"])
	}
	full := entries[1].(map[string]any)
	if full["service_name"] != "svc" || full["address"] != "10.0.0.8:8080" ||
		full["port"].(float64) != 8080 || full["healthy"] != true ||
		full["weight"].(float64) != 10 || full["heartbeat_at"] != baseTime {
		t.Fatalf("full entry mismatch: %v", full)
	}

	for _, id := range []string{"i-1", "i-2"} {
		if _, found, err := st.GetInstance("svc", id); err != nil || !found {
			t.Fatalf("%s not stored: found=%v err=%v", id, found, err)
		}
	}
}

func TestBatchRegisterBodyEntryReadsServiceNameFromBody(t *testing.T) {
	router, st := testRouter(t)

	body := map[string]any{
		"service_name": "svc",
		"instances":    []any{batchEntry("i-1", 1, at(0)), batchEntry("i-2", 2, at(1))},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "svc" || out["registered"].(float64) != 2 {
		t.Fatalf("unexpected response: %v", out)
	}
	if list, _ := st.ListInstances("svc"); len(list) != 2 {
		t.Fatalf("stored %d instances, want 2", len(list))
	}
}

func TestBatchRegisterPathEntryPrefersPathServiceName(t *testing.T) {
	router, st := testRouter(t)

	body := map[string]any{
		"service_name": "bodysvc",
		"instances":    []any{batchEntry("i-1", 1, at(0))},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/services/pathsvc/instances/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "pathsvc" {
		t.Fatalf("service_name = %v, want pathsvc", out["service_name"])
	}
	if _, found, _ := st.GetInstance("pathsvc", "i-1"); !found {
		t.Fatalf("instance not stored under path service")
	}
	if list, _ := st.ListInstances("bodysvc"); len(list) != 0 {
		t.Fatalf("body service_name must be ignored, got %d rows", len(list))
	}
}

func TestBatchRegisterOverwritesAndLeavesOthersUntouched(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())
	peer := heartbeatRecord()
	peer["instance_id"] = "i-9"
	peer["weight"] = 7
	registerInstance(t, router, peer)
	foreign := heartbeatRecord()
	foreign["service_name"] = "other"
	registerInstance(t, router, foreign)

	replacement := batchEntry("i-1", 20, at(30))
	replacement["address"] = "10.0.0.9:9090"
	body := map[string]any{"instances": []any{replacement, batchEntry("i-2", 3, at(31))}}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/services/svc/instances/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}

	overwritten, _, _ := st.GetInstance("svc", "i-1")
	if overwritten.Weight != 20 || overwritten.Address != "10.0.0.9:9090" ||
		!overwritten.HeartbeatAt.Equal(mustParseTime(at(30))) {
		t.Fatalf("i-1 not overwritten: %+v", overwritten)
	}
	untouched, _, _ := st.GetInstance("svc", "i-9")
	if untouched.Weight != 7 || !untouched.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("i-9 changed by batch: %+v", untouched)
	}
	other, _, _ := st.GetInstance("other", "i-1")
	if other.Weight != 10 || !other.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("other service changed by batch: %+v", other)
	}
}

func TestBatchRegisterValidationCreatesNothing(t *testing.T) {
	router, st := testRouter(t)

	cases := []struct {
		name   string
		target string
		body   any
	}{
		{"body not an object", "/api/v1/services/svc/instances/batch", []any{
			batchEntry("i-1", 1, at(0))}},
		{"missing instances", "/api/v1/services/svc/instances/batch",
			map[string]any{}},
		{"empty instances", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{}}},
		{"instances not a list", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": "i-1"}},
		{"entry not an object", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{"i-1"}}},
		{"entry missing instance id", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{map[string]any{"weight": 1, "heartbeat_at": at(0)}}}},
		{"entry blank instance id", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{batchEntry("  ", 1, at(0))}}},
		{"duplicate instance id", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{batchEntry("i-1", 1, at(0)), batchEntry("i-1", 2, at(1))}}},
		{"negative port", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{withField("port", -1)}}},
		{"fractional port", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{withField("port", 1.5)}}},
		{"string healthy", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{withField("healthy", "true")}}},
		{"missing weight", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{map[string]any{"instance_id": "i-1", "heartbeat_at": at(0)}}}},
		{"zero weight", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{batchEntry("i-1", 0, at(0))}}},
		{"non-numeric weight", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{batchEntry("i-1", "heavy", at(0))}}},
		{"missing heartbeat", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{map[string]any{"instance_id": "i-1", "weight": 1}}}},
		{"unparseable heartbeat", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{batchEntry("i-1", 1, "nope")}}},
		{"valid first entry invalid second", "/api/v1/services/svc/instances/batch",
			map[string]any{"instances": []any{batchEntry("i-1", 1, at(0)), batchEntry("i-2", -3, at(1))}}},
		{"body entry missing service name", "/api/v1/register/batch",
			map[string]any{"instances": []any{batchEntry("i-1", 1, at(0))}}},
		{"empty path service name", "/api/v1/services//instances/batch",
			map[string]any{"instances": []any{batchEntry("i-1", 1, at(0))}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPost, tc.target, tc.body)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			if list, _ := st.ListInstances("svc"); len(list) != 0 {
				t.Fatalf("invalid batch created records: %v", list)
			}
		})
	}
}

func withField(key string, value any) map[string]any {
	entry := batchEntry("i-1", 1, at(0))
	entry[key] = value
	return entry
}

func TestBatchRegisterStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	body := map[string]any{"instances": []any{batchEntry("i-1", 1, at(0))}}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/services/svc/instances/batch", body)
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")

	body["service_name"] = "svc"
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register/batch", body)
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
}

func TestBatchRegisterDoesNotTriggerLostCleanup(t *testing.T) {
	router, st := testRouter(t)
	stale := heartbeatRecord()
	stale["instance_id"] = "i-old"
	stale["heartbeat_at"] = at(0)
	registerInstance(t, router, stale)

	body := map[string]any{"instances": []any{batchEntry("i-1", 1, at(0))}}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/services/svc/instances/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	list, err := st.ListInstances("svc")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("batch register removed stale records: %v", list)
	}
}
