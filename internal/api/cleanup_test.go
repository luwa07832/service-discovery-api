package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func seedCleanupInstances(t *testing.T, router http.Handler) {
	t.Helper()
	// alpha/i-1 is strictly lost, alpha/i-2 sits exactly on the lost
	// boundary, alpha/i-3 is unhealthy but still fresh.
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "10.0.0.1:8080",
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-2", "healthy": true,
		"weight": 5, "heartbeat_at": at(5),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-3", "healthy": false,
		"weight": 3, "heartbeat_at": at(8),
	})
	// beta/i-1 is strictly lost, beta/i-2 is still online.
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-1", "healthy": true,
		"weight": 7, "heartbeat_at": at(1),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-2", "healthy": true,
		"weight": 2, "heartbeat_at": at(9),
	})
}

func cleanupRequest(t *testing.T, router http.Handler, target string, body any) (int, map[string]any) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, target, body)
	return rec.Code, decodeBody(t, rec)
}

func TestCleanupRemovesStrictlyLostAcrossAllServices(t *testing.T) {
	router, st := testRouter(t)
	seedCleanupInstances(t, router)

	code, out := cleanupRequest(t, router,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=5m", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %v", code, out)
	}
	if out["evaluate_at"] != at(10) {
		t.Fatalf("evaluate_at = %v, want %s", out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"] != float64(300) {
		t.Fatalf("heartbeat_timeout = %v, want 300 seconds", out["heartbeat_timeout"])
	}
	if out["removed"] != float64(2) {
		t.Fatalf("removed = %v, want 2", out["removed"])
	}

	rawList, ok := out["instances"].([]any)
	if !ok || len(rawList) != 2 {
		t.Fatalf("instances = %v, want two removed records", out["instances"])
	}
	first := rawList[0].(map[string]any)
	second := rawList[1].(map[string]any)
	if first["service_name"] != "alpha" || first["instance_id"] != "i-1" ||
		second["service_name"] != "beta" || second["instance_id"] != "i-1" {
		t.Fatalf("removed order = %v, want alpha/i-1 then beta/i-1", rawList)
	}
	if first["address"] != "10.0.0.1:8080" || first["port"] != float64(8080) ||
		first["healthy"] != true || first["weight"] != float64(10) ||
		first["heartbeat_at"] != at(0) {
		t.Fatalf("removed record mismatch: %v", first)
	}

	for _, key := range [][2]string{{"alpha", "i-1"}, {"beta", "i-1"}} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || found {
			t.Fatalf("%s/%s should be deleted: found=%v err=%v", key[0], key[1], found, err)
		}
	}
	// Boundary, unhealthy-but-fresh and online records all survive.
	for _, key := range [][2]string{{"alpha", "i-2"}, {"alpha", "i-3"}, {"beta", "i-2"}} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || !found {
			t.Fatalf("%s/%s should survive: found=%v err=%v", key[0], key[1], found, err)
		}
	}
}

func TestCleanupScopedToOneServiceLeavesOthersUntouched(t *testing.T) {
	router, st := testRouter(t)
	seedCleanupInstances(t, router)

	code, out := cleanupRequest(t, router, "/api/v1/cleanup", map[string]any{
		"service_name": "alpha", "evaluate_at": at(10), "heartbeat_timeout": "5m",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %v", code, out)
	}
	if out["removed"] != float64(1) {
		t.Fatalf("removed = %v, want 1", out["removed"])
	}
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "i-1" {
		t.Fatalf("removed ids = %v, want [i-1]", ids)
	}
	// The equally lost record of the other service must remain.
	if _, found, err := st.GetInstance("beta", "i-1"); err != nil || !found {
		t.Fatalf("beta/i-1 must be untouched: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetInstance("alpha", "i-1"); err != nil || found {
		t.Fatalf("alpha/i-1 should be deleted: found=%v err=%v", found, err)
	}
}

func TestCleanupAcceptsUnixEvaluateAtAndDurationText(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	evaluateAt := time.Date(2026, 10, 1, 12, 10, 0, 0, time.UTC)

	code, out := cleanupRequest(t, router, "/api/v1/cleanup", map[string]any{
		"evaluate_at": evaluateAt.Unix(), "heartbeat_timeout": "2m",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %v", code, out)
	}
	if out["heartbeat_timeout"] != float64(120) {
		t.Fatalf("heartbeat_timeout = %v, want 120 seconds", out["heartbeat_timeout"])
	}
	if out["removed"] != float64(1) {
		t.Fatalf("removed = %v, want 1", out["removed"])
	}
}

func TestCleanupKeepsBoundaryAndReportsEmptyResult(t *testing.T) {
	router, st := testRouter(t)
	// heartbeat at(5) with timeout 5m puts the lost boundary exactly at(10).
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "boundary", "healthy": true,
		"weight": 1, "heartbeat_at": at(5),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "sick", "healthy": false,
		"weight": 1, "heartbeat_at": at(6),
	})

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=300", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["removed"] != float64(0) {
		t.Fatalf("removed = %v, want 0", out["removed"])
	}
	if list, ok := out["instances"].([]any); !ok || len(list) != 0 {
		t.Fatalf("instances = %v, want empty list", out["instances"])
	}
	if !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("instances must serialize as an empty array: %s", rec.Body.String())
	}
	for _, id := range []string{"boundary", "sick"} {
		if _, found, err := st.GetInstance("svc", id); err != nil || !found {
			t.Fatalf("svc/%s must survive: found=%v err=%v", id, found, err)
		}
	}
}

func TestCleanupRejectsInvalidParametersWithoutDeleting(t *testing.T) {
	router, st := testRouter(t)
	seedCleanupInstances(t, router)

	targets := []string{
		"/api/v1/cleanup",                                                               // missing evaluate_at and timeout
		"/api/v1/cleanup?heartbeat_timeout=5m",                                          // missing evaluate_at
		"/api/v1/cleanup?evaluate_at=not-a-time&heartbeat_timeout=5m",                   // bad evaluate_at
		"/api/v1/cleanup?evaluate_at=" + at(10),                                         // missing heartbeat_timeout
		"/api/v1/cleanup?evaluate_at=" + at(10) + "&heartbeat_timeout=0",                // zero timeout
		"/api/v1/cleanup?evaluate_at=" + at(10) + "&heartbeat_timeout=-5",               // negative timeout
		"/api/v1/cleanup?evaluate_at=" + at(10) + "&heartbeat_timeout=abc",              // bad timeout
		"/api/v1/cleanup?evaluate_at=" + at(10) + "&heartbeat_timeout=5m&service_name=", // empty service_name
	}
	for _, target := range targets {
		rec := doRequest(t, router, http.MethodPost, target, nil)
		expectParameterError(t, rec)
	}
	// An explicit empty service_name in the JSON body is rejected as well.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"service_name": "", "evaluate_at": at(10), "heartbeat_timeout": "5m",
	})
	expectParameterError(t, rec)

	// evaluate_at earlier than an instance heartbeat in the evaluated scope.
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/cleanup?evaluate_at="+at(4)+"&heartbeat_timeout=5m", nil)
	expectParameterError(t, rec)

	// No rejected request may have deleted anything.
	remaining, err := st.ListAllInstances()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 5 {
		t.Fatalf("remaining = %d, want all 5 records untouched", len(remaining))
	}
}

func TestCleanupScopedIgnoresFutureHeartbeatsOfOtherServices(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	// beta's heartbeat is later than the evaluation point, but beta is not
	// part of the evaluated scope, so the request stays valid.
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(30),
	})

	code, out := cleanupRequest(t, router, "/api/v1/cleanup", map[string]any{
		"service_name": "alpha", "evaluate_at": at(10), "heartbeat_timeout": "5m",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %v", code, out)
	}
	if out["removed"] != float64(1) {
		t.Fatalf("removed = %v, want 1", out["removed"])
	}
	if _, found, err := st.GetInstance("beta", "i-1"); err != nil || !found {
		t.Fatalf("beta/i-1 must be untouched: found=%v err=%v", found, err)
	}
}
