package api

import (
	"net/http"
	"testing"
)

func TestDeleteEntryRemovesOnlyNamedInstance(t *testing.T) {
	router, st := lifecycleRouter(t)
	register(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "host": "h", "port": 1,
		"healthy": true, "weight": 1, "heartbeat_at": lifecycleBaseTime, "request_at": lifecycleBaseTime,
	})
	register(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-2", "host": "h", "port": 1,
		"healthy": true, "weight": 1, "heartbeat_at": lifecycleBaseTime, "request_at": lifecycleBaseTime,
	})

	rec := lifecycleRequest(t, router, http.MethodDelete,
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

	rec = lifecycleRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing instance status = %d, want 404", rec.Code)
	}

	rec = lifecycleRequest(t, router, http.MethodDelete,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rec.Code)
	}

	rec = lifecycleRequest(t, router, http.MethodPost, "/api/v1/deregister",
		map[string]any{"service_name": "svc"})
	expectInvalidParameter(t, rec)
	if list, _ := st.ListInstances("svc"); len(list) != 1 {
		t.Fatalf("invalid delete changed storage: %d rows", len(list))
	}
}

func TestEmptyServiceNamePathIsParameterError(t *testing.T) {
	router, st := lifecycleRouter(t)
	register(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "host": "h", "port": 1,
		"healthy": true, "weight": 1, "heartbeat_at": lifecycleBaseTime, "request_at": lifecycleBaseTime,
	})
	targets := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/v1/services//instances", map[string]any{
			"instance_id": "x", "host": "h", "port": 1, "weight": 1,
			"heartbeat_at": lifecycleBaseTime, "request_at": lifecycleBaseTime}},
		{http.MethodGet, "/api/v1/services//instances", nil},
		{http.MethodGet, "/api/v1/services//discover?evaluate_at=" + lifecycleAt(10) + "&heartbeat_timeout=1m", nil},
		{http.MethodPost, "/api/v1/services//cleanup", map[string]any{
			"evaluate_at": lifecycleAt(10), "heartbeat_timeout": "1m"}},
		{http.MethodDelete, "/api/v1/services//instances/i-1", nil},
	}
	for _, item := range targets {
		rec := lifecycleRequest(t, router, item.method, item.path, item.body)
		expectInvalidParameter(t, rec)
	}
	if list, _ := st.ListInstances("svc"); len(list) != 1 {
		t.Fatalf("empty-name requests changed storage: %d rows", len(list))
	}
}
