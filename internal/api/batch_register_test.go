package api

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

func batchEntry(id string) map[string]any {
	return map[string]any{
		"instance_id":  id,
		"address":      "10.0.0.8:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       10,
		"heartbeat_at": at(0),
	}
}

func TestBatchRegisterByPathRegistersInOrder(t *testing.T) {
	router, st := testRouter(t)
	body := map[string]any{
		// Even if the body carries a service name, the path value wins.
		"service_name": "ignored",
		"instances": []any{
			batchEntry("i-2"),
			batchEntry("i-1"),
		},
	}
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/billing/instances/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "billing" {
		t.Fatalf("service_name = %v, want billing", out["service_name"])
	}
	if out["registered"].(float64) != 2 {
		t.Fatalf("registered = %v, want 2", out["registered"])
	}
	entries := out["instances"].([]any)
	if len(entries) != 2 {
		t.Fatalf("instances len = %d", len(entries))
	}
	if entries[0].(map[string]any)["instance_id"] != "i-2" ||
		entries[1].(map[string]any)["instance_id"] != "i-1" {
		t.Fatalf("order not preserved")
	}
	for _, id := range []string{"i-1", "i-2"} {
		got, found, err := st.GetInstance("billing", id)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", id, found, err)
		}
		if got.ServiceName != "billing" {
			t.Fatalf("%s stored under %q", id, got.ServiceName)
		}
	}
	if _, found, _ := st.GetInstance("ignored", "i-1"); found {
		t.Fatalf("body service name must be ignored on the path entry")
	}
}

func TestBatchRegisterByBodyAndOverwrite(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	// Defaults: no address -> "", no port -> 0, no healthy -> false.
	first := map[string]any{
		"instance_id":  "i-1",
		"weight":       "2.5",
		"heartbeat_at": 1759324800,
	}
	second := map[string]any{
		"instance_id":  "i-2",
		"port":         0,
		"healthy":      false,
		"weight":       1,
		"heartbeat_at": at(30),
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register/batch", map[string]any{
		"service_name": "svc",
		"instances":    []any{first, second},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "svc" || out["registered"].(float64) != 2 {
		t.Fatalf("unexpected envelope: %v", out)
	}
	entries := out["instances"].([]any)
	overwritten := entries[0].(map[string]any)
	if overwritten["address"] != "" || overwritten["port"].(float64) != 0 ||
		overwritten["healthy"] != false || overwritten["weight"].(float64) != 2.5 {
		t.Fatalf("defaults/overwrite wrong: %v", overwritten)
	}
	if overwritten["heartbeat_at"] != time.Unix(1759324800, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("unix heartbeat = %v", overwritten["heartbeat_at"])
	}

	// The pre-existing i-1 record is fully overwritten by the batch entry.
	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Address != "" || got.Port != 0 || got.Healthy ||
		got.Weight != 2.5 || !got.HeartbeatAt.Equal(time.Unix(1759324800, 0).UTC()) {
		t.Fatalf("i-1 not overwritten: %+v", got)
	}
}

func TestBatchRegisterDoesNotTouchOtherInstances(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())
	other := heartbeatRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/register/batch", map[string]any{
		"service_name": "svc",
		"instances": []any{map[string]any{
			"instance_id":  "i-2",
			"weight":       1,
			"heartbeat_at": at(40),
		}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	untouched, found, _ := st.GetInstance("other", "i-1")
	if !found || !untouched.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("other service instance touched: %+v", untouched)
	}
	same, found, _ := st.GetInstance("svc", "i-1")
	if !found || !same.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("out-of-batch instance touched: %+v", same)
	}
}

func TestBatchRegisterValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, heartbeatRecord())

	validEntry := func() map[string]any {
		entry := batchEntry("i-9")
		return entry
	}
	cases := []struct {
		name   string
		target string
		body   any
	}{
		{"body is a list", "/api/v1/register/batch", []any{validEntry()}},
		{"body is a string", "/api/v1/register/batch", "nope"},
		{"missing service name", "/api/v1/register/batch", map[string]any{
			"instances": []any{validEntry()}}},
		{"empty service name", "/api/v1/register/batch", map[string]any{
			"service_name": "  ", "instances": []any{validEntry()}}},
		{"missing instances", "/api/v1/register/batch", map[string]any{
			"service_name": "svc"}},
		{"empty instances", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{}}},
		{"instances not a list", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": "one"}},
		{"entry not an object", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{"i-9"}}},
		{"missing instance id", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"weight": 1, "heartbeat_at": at(0)}}}},
		{"blank instance id", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": " ", "weight": 1, "heartbeat_at": at(0)}}}},
		{"duplicate instance id", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{validEntry(), validEntry()}}},
		{"missing weight", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "heartbeat_at": at(0)}}}},
		{"zero weight", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 0, "heartbeat_at": at(0)}}}},
		{"negative weight", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": -1, "heartbeat_at": at(0)}}}},
		{"negative port", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 1, "port": -1, "heartbeat_at": at(0)}}}},
		{"fractional port", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 1, "port": 1.5, "heartbeat_at": at(0)}}}},
		{"healthy as string", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 1, "healthy": "true", "heartbeat_at": at(0)}}}},
		{"healthy as number", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 1, "healthy": 1, "heartbeat_at": at(0)}}}},
		{"missing heartbeat", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 1}}}},
		{"unparseable heartbeat", "/api/v1/register/batch", map[string]any{
			"service_name": "svc", "instances": []any{map[string]any{
				"instance_id": "i-9", "weight": 1, "heartbeat_at": "nope"}}}},
		{"empty path service name", "/api/v1/services//instances/batch", map[string]any{
			"instances": []any{validEntry()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPost, tc.target, tc.body)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			got, found, _ := st.GetInstance("svc", "i-1")
			if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
				t.Fatalf("record changed after invalid batch: %+v", got)
			}
			if _, found, _ := st.GetInstance("svc", "i-9"); found {
				t.Fatalf("invalid batch created i-9")
			}
		})
	}
}

func TestBatchRegisterStorageUnavailable(t *testing.T) {
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

	for _, target := range []string{
		"/api/v1/services/svc/instances/batch",
		"/api/v1/register/batch",
	} {
		rec := doRequest(t, router, http.MethodPost, target, map[string]any{
			"service_name": "svc",
			"instances": []any{
				batchEntry("i-1"),
				batchEntry("i-2"),
			},
		})
		expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	}
}
