package api

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"

	_ "modernc.org/sqlite"
)

// discoverAtomicRouter wires a router backed by a SQLite file whose path is
// returned too, so a regression test can open a second connection and install
// a trigger that injects a storage failure mid-cleanup. No production test
// hook or extra persistent file is involved.
func discoverAtomicRouter(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), st, path
}

// execOnSQLite opens a short-lived second connection to the same SQLite file
// and runs the supplied DDL/statement. Triggers created this way are durable
// schema objects, so the connection can be closed immediately afterwards.
func execOnSQLite(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// failLostMidDeleteTrigger lets one DELETE execute and then aborts the next
// targeted one, reproducing a storage deletion error after at least one
// deletion has already been attempted inside the cleanup transaction.
const failLostMidDeleteTrigger = `
CREATE TRIGGER fail_lost_mid_delete
BEFORE DELETE ON service_instances
WHEN OLD.service_name = 'svc' AND OLD.instance_id = 'lost-b'
BEGIN
	SELECT RAISE(ABORT, 'injected delete failure');
END`

func seedDiscoverAtomicFixture(t *testing.T, router http.Handler) {
	t.Helper()
	instances := []map[string]any{
		// Three strictly lost records of the selected service. lost-b is
		// unhealthy: lost records must be deleted regardless of health.
		{"service_name": "svc", "instance_id": "lost-a", "address": "10.0.0.1:9001",
			"port": 9001, "healthy": true, "weight": 11, "heartbeat_at": at(0)},
		{"service_name": "svc", "instance_id": "lost-b", "address": "10.0.0.2:9002",
			"port": 9002, "healthy": false, "weight": 12, "heartbeat_at": at(0)},
		{"service_name": "svc", "instance_id": "lost-c", "address": "10.0.0.3:9003",
			"port": 9003, "healthy": true, "weight": 13, "heartbeat_at": at(0)},
		// Two healthy fresh instances (different weights) and one unhealthy
		// but fresh instance: all three stay stored; only the healthy pair
		// becomes a candidate.
		{"service_name": "svc", "instance_id": "fresh-hi", "address": "10.0.0.4:9004",
			"port": 9004, "healthy": true, "weight": 9, "heartbeat_at": at(9)},
		{"service_name": "svc", "instance_id": "fresh-lo", "address": "10.0.0.5:9005",
			"port": 9005, "healthy": true, "weight": 4, "heartbeat_at": at(9)},
		{"service_name": "svc", "instance_id": "fresh-sick", "address": "10.0.0.6:9006",
			"port": 9006, "healthy": false, "weight": 7, "heartbeat_at": at(9)},
		// Another service, including an equally lost record, must never be
		// read or touched by a request scoped to svc.
		{"service_name": "other", "instance_id": "lost-x", "address": "10.0.0.7:9007",
			"port": 9007, "healthy": true, "weight": 3, "heartbeat_at": at(0)},
		{"service_name": "other", "instance_id": "fresh-o", "address": "10.0.0.8:9008",
			"port": 9008, "healthy": true, "weight": 3, "heartbeat_at": at(9)},
	}
	for _, instance := range instances {
		registerInstance(t, router, instance)
	}
}

// publicInstanceMap reads one service through the public list entry and
// indexes every record by instance id, so a test can verify full records
// without touching the storage package directly.
func publicInstanceMap(t *testing.T, router http.Handler, serviceName string) map[string]map[string]any {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+serviceName+"/instances", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public list %s: status %d body %s", serviceName, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	rawList := out["instances"].([]any)
	byID := make(map[string]map[string]any, len(rawList))
	for _, raw := range rawList {
		item := raw.(map[string]any)
		byID[item["instance_id"].(string)] = item
	}
	return byID
}

func assertFullRecord(t *testing.T, record map[string]any, want map[string]any) {
	t.Helper()
	for key, expected := range want {
		if record[key] != expected {
			t.Fatalf("field %s = %v, want %v, full record %v", key, record[key], expected, record)
		}
	}
}

// TestDiscoverCleanupIsAtomicAcrossAllSingleServiceRoutes drives the four
// single-service discover routes (GET/POST on both path shapes) through a
// cleanup in which the first lost delete is attempted and the next one hits a
// storage error. The whole cleanup must roll back: every original record
// stays in place with all its fields, the other service is untouched, and the
// error response carries no candidate list. After the failure is removed, the
// identical request deletes every lost record, returns the remaining healthy
// candidates, and a repeated request returns the same candidates without
// changing anything else.
func TestDiscoverCleanupIsAtomicAcrossAllSingleServiceRoutes(t *testing.T) {
	router, _, dbPath := discoverAtomicRouter(t)
	seedDiscoverAtomicFixture(t, router)
	execOnSQLite(t, dbPath, failLostMidDeleteTrigger)

	query := "evaluate_at=" + at(10) + "&heartbeat_timeout=2m"
	routes := []struct {
		method string
		target string
		body   any
	}{
		{http.MethodGet, "/api/v1/services/svc/discover?" + query, nil},
		{http.MethodPost, "/api/v1/services/svc/discover", map[string]any{
			"evaluate_at": at(10), "heartbeat_timeout": "2m"}},
		{http.MethodGet, "/api/v1/discover?service_name=svc&" + query, nil},
		{http.MethodPost, "/api/v1/discover", map[string]any{
			"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": 120}},
	}
	for _, route := range routes {
		rec := doRequest(t, router, route.method, route.target, route.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503, body %s",
				route.method, route.target, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		if len(out) != 1 {
			t.Fatalf("%s %s: error response must contain only error object: %v",
				route.method, route.target, out)
		}
		errObj := out["error"].(map[string]any)
		if errObj["code"] != "storage_unavailable" {
			t.Fatalf("%s %s: code = %v, want storage_unavailable",
				route.method, route.target, errObj["code"])
		}
		if message, ok := errObj["message"].(string); !ok || message == "" {
			t.Fatalf("%s %s: message must be a non-empty string: %v",
				route.method, route.target, errObj["message"])
		}
		if _, present := out["instances"]; present {
			t.Fatalf("%s %s: failed cleanup must not return candidates: %v",
				route.method, route.target, out)
		}
		if body := rec.Body.String(); containsAny(body, "injected", "SQL", "service_instances") {
			t.Fatalf("%s %s: error body leaks internals: %s", route.method, route.target, body)
		}

		// All six original records of the selected service are still present
		// through the public query, with every field unchanged.
		svc := publicInstanceMap(t, router, "svc")
		if len(svc) != 6 {
			t.Fatalf("%s %s: selected service rows = %d, want all 6 after rollback",
				route.method, route.target, len(svc))
		}
		assertFullRecord(t, svc["lost-a"], map[string]any{
			"service_name": "svc", "instance_id": "lost-a", "address": "10.0.0.1:9001",
			"port": float64(9001), "healthy": true, "weight": float64(11),
			"heartbeat_at": at(0)})
		assertFullRecord(t, svc["lost-b"], map[string]any{
			"service_name": "svc", "instance_id": "lost-b", "address": "10.0.0.2:9002",
			"port": float64(9002), "healthy": false, "weight": float64(12),
			"heartbeat_at": at(0)})
		assertFullRecord(t, svc["lost-c"], map[string]any{
			"service_name": "svc", "instance_id": "lost-c", "address": "10.0.0.3:9003",
			"port": float64(9003), "healthy": true, "weight": float64(13),
			"heartbeat_at": at(0)})

		// The other service was never scoped: both records stay untouched.
		other := publicInstanceMap(t, router, "other")
		if len(other) != 2 {
			t.Fatalf("%s %s: other service rows = %d, want 2",
				route.method, route.target, len(other))
		}
		if _, found := other["lost-x"]; !found {
			t.Fatalf("%s %s: other service lost record missing", route.method, route.target)
		}
	}

	// Remove the injected failure and retry the identical discover input.
	execOnSQLite(t, dbPath, "DROP TRIGGER fail_lost_mid_delete")
	rec := doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": 120,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if ids := instanceIDs(t, out); len(ids) != 2 || ids[0] != "fresh-hi" || ids[1] != "fresh-lo" {
		t.Fatalf("retry candidates = %v, want [fresh-hi fresh-lo]", ids)
	}
	if out["heartbeat_timeout"] != float64(120) {
		t.Fatalf("retry heartbeat_timeout = %v, want 120", out["heartbeat_timeout"])
	}

	// All lost records (including the unhealthy lost one) are gone; the
	// unhealthy-but-fresh record stays.
	svc := publicInstanceMap(t, router, "svc")
	if len(svc) != 3 {
		t.Fatalf("selected service rows after retry = %d, want 3", len(svc))
	}
	for _, id := range []string{"lost-a", "lost-b", "lost-c"} {
		if _, found := svc[id]; found {
			t.Fatalf("lost record %s still stored after successful retry", id)
		}
	}
	if _, found := svc["fresh-sick"]; !found {
		t.Fatalf("unhealthy fresh record was removed")
	}
	other := publicInstanceMap(t, router, "other")
	if len(other) != 2 {
		t.Fatalf("other service rows after retry = %d, want 2", len(other))
	}

	// Same input again: identical candidates, and the retained records do
	// not change.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": 120,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat: status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 2 ||
		ids[0] != "fresh-hi" || ids[1] != "fresh-lo" {
		t.Fatalf("repeat candidates = %v, want [fresh-hi fresh-lo]", ids)
	}
	svc = publicInstanceMap(t, router, "svc")
	if len(svc) != 3 {
		t.Fatalf("selected service rows after repeat = %d, want 3", len(svc))
	}
	other = publicInstanceMap(t, router, "other")
	if len(other) != 2 {
		t.Fatalf("other service rows after repeat = %d, want 2", len(other))
	}
}

// TestDiscoverPOSTStrictBoundaryKeepsEqualRecord checks the POST discover
// route uses the same strict boundary as GET: evaluate_at equal to
// heartbeat_at + timeout keeps the instance; one second later deletes it.
func TestDiscoverPOSTStrictBoundaryKeepsEqualRecord(t *testing.T) {
	router, st, _ := discoverAtomicRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "edge", "address": "10.0.0.9:9009",
		"port": 9009, "healthy": true, "weight": 1, "heartbeat_at": at(5),
	})

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/discover", map[string]any{
			"evaluate_at": at(10), "heartbeat_timeout": "5m",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("boundary: status = %d body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "edge" {
		t.Fatalf("boundary candidates = %v, want [edge]", ids)
	}
	if _, found, _ := st.GetInstance("svc", "edge"); !found {
		t.Fatalf("boundary record was deleted")
	}

	// One second strictly past the boundary the record is lost: excluded and
	// deleted, and the empty result serializes as [].
	evaluate := "2026-10-01T12:10:01Z"
	rec = doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": evaluate, "heartbeat_timeout": 300,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("past-boundary: status = %d body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 0 {
		t.Fatalf("past-boundary candidates = %v, want empty", ids)
	}
	if _, found, _ := st.GetInstance("svc", "edge"); found {
		t.Fatalf("strictly lost record was not deleted")
	}
}

// TestDiscoverInvalidParametersRejectedAcrossRoutes verifies every
// single-service discover route rejects bad input with HTTP 400
// invalid_parameter, returns only the error object, and keeps every record
// unchanged.
func TestDiscoverInvalidParametersRejectedAcrossRoutes(t *testing.T) {
	router, _, _ := discoverAtomicRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i", "healthy": true,
		"weight": 1, "heartbeat_at": at(5),
	})
	goodEvaluate := at(10)

	cases := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"post missing service", http.MethodPost, "/api/v1/discover",
			map[string]any{"evaluate_at": goodEvaluate, "heartbeat_timeout": 120}},
		{"post empty service", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "  ", "evaluate_at": goodEvaluate, "heartbeat_timeout": 120}},
		{"post missing evaluate_at", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "heartbeat_timeout": 120}},
		{"post unparseable evaluate_at", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "evaluate_at": "tomorrow", "heartbeat_timeout": 120}},
		{"post missing timeout", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "evaluate_at": goodEvaluate}},
		{"post zero timeout", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "evaluate_at": goodEvaluate, "heartbeat_timeout": 0}},
		{"post negative timeout", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "evaluate_at": goodEvaluate, "heartbeat_timeout": "-1m"}},
		{"post unparseable timeout", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "evaluate_at": goodEvaluate, "heartbeat_timeout": "soon"}},
		{"post evaluate before heartbeat", http.MethodPost, "/api/v1/discover",
			map[string]any{"service_name": "svc", "evaluate_at": at(4), "heartbeat_timeout": 120}},
		{"get missing service", http.MethodGet,
			"/api/v1/discover?evaluate_at=" + goodEvaluate + "&heartbeat_timeout=2m", nil},
		{"get missing evaluate_at", http.MethodGet,
			"/api/v1/discover?service_name=svc&heartbeat_timeout=2m", nil},
		{"get missing timeout", http.MethodGet,
			"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate, nil},
		{"get zero timeout", http.MethodGet,
			"/api/v1/services/svc/discover?evaluate_at=" + goodEvaluate + "&heartbeat_timeout=0", nil},
		{"get bad evaluate_at", http.MethodGet,
			"/api/v1/services/svc/discover?evaluate_at=not-a-time&heartbeat_timeout=2m", nil},
		{"get evaluate before heartbeat", http.MethodGet,
			"/api/v1/discover?service_name=svc&evaluate_at=" + at(4) + "&heartbeat_timeout=2m", nil},
		{"get empty service name path", http.MethodGet,
			"/api/v1/services//discover?evaluate_at=" + goodEvaluate + "&heartbeat_timeout=2m", nil},
		{"post empty service name path", http.MethodPost,
			"/api/v1/services//discover", map[string]any{"evaluate_at": goodEvaluate, "heartbeat_timeout": 120}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body.String())
			}
			out := decodeBody(t, rec)
			if len(out) != 1 {
				t.Fatalf("error body must contain only error object: %v", out)
			}
			errObj := out["error"].(map[string]any)
			if errObj["code"] != "invalid_parameter" {
				t.Fatalf("code = %v, want invalid_parameter", errObj["code"])
			}
			if message, ok := errObj["message"].(string); !ok || message == "" {
				t.Fatalf("message must be a non-empty string: %v", errObj["message"])
			}
			if body := rec.Body.String(); containsAny(body, "SQL", "service_instances", "internal/") {
				t.Fatalf("error body leaks internals: %s", body)
			}
			if records := publicInstanceMap(t, router, "svc"); len(records) != 1 {
				t.Fatalf("rejected request changed storage: %d rows", len(records))
			}
		})
	}
}

// TestDiscoverStorageReadFailureReturns503 covers a storage read failure
// before any classification: the response is 503 storage_unavailable and
// contains no candidate list.
func TestDiscoverStorageReadFailureReturns503(t *testing.T) {
	router, st, _ := discoverAtomicRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=2m", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if len(out) != 1 || out["error"].(map[string]any)["code"] != "storage_unavailable" {
		t.Fatalf("body = %v, want only storage_unavailable error", out)
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
