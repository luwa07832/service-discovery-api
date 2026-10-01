package api

import (
	"net/http"
	"testing"
	"time"
)

func lifecycleAt(minutes int) string {
	return time.Date(2026, 10, 1, 12, minutes, 0, 0, time.UTC).Format(time.RFC3339)
}

func discoverIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	rawList, ok := out["instances"].([]any)
	if !ok {
		t.Fatalf("instances is not a list: %v", out)
	}
	ids := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		ids = append(ids, raw.(map[string]any)["instance_id"].(string))
	}
	return ids
}

func registerForDiscovery(t *testing.T, router http.Handler, service, id string, healthy bool, weight int, heartbeatAt string, host string) {
	t.Helper()
	if host == "" {
		host = "10.0.0.1"
	}
	register(t, router, map[string]any{
		"service_name": service, "instance_id": id, "host": host, "port": 8080,
		"healthy": healthy, "weight": weight,
		"heartbeat_at": heartbeatAt, "request_at": heartbeatAt,
	})
}

func discover(t *testing.T, router http.Handler, target string) map[string]any {
	t.Helper()
	rec := lifecycleRequest(t, router, http.MethodGet, target, nil)
	if rec.Code != 200 {
		t.Fatalf("discover %s: %d %s", target, rec.Code, rec.Body.String())
	}
	return decodeLifecycleBody(t, rec)
}

func TestDiscoverFiltersUnhealthyAndLostInstances(t *testing.T) {
	router, st := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "healthy-fresh", true, 1, lifecycleAt(9), "a")
	registerForDiscovery(t, router, "svc", "healthy-boundary", true, 10, lifecycleAt(0), "b")
	registerForDiscovery(t, router, "svc", "healthy-lost", true, 8, lifecycleAt(-1), "b2")
	registerForDiscovery(t, router, "svc", "unhealthy-fresh", false, 10, lifecycleAt(9), "c")
	registerForDiscovery(t, router, "other", "must-not-appear", true, 100, lifecycleAt(9), "d")

	// evaluate 12:10 with timeout 10m: deadline 12:00. Only a heartbeat
	// strictly earlier than 12:00 is lost; one at exactly 12:00 survives.
	out := discover(t, router,
		"/api/v1/discover?service_name=svc&evaluate_at="+lifecycleAt(10)+"&heartbeat_timeout=10m")
	ids := discoverIDs(t, out)
	want := []string{"healthy-boundary", "healthy-fresh"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}

	// Seconds-style timeout: at 12:09:30 with 60s, deadline 12:08:30; the
	// heartbeat at 12:09:00 survives while both 12:00 and 11:59 are lost.
	evaluate := time.Date(2026, 10, 1, 12, 9, 30, 0, time.UTC).Format(time.RFC3339)
	out = discover(t, router,
		"/api/v1/discover?service_name=svc&evaluate_at="+evaluate+"&heartbeat_timeout=60")
	if ids := discoverIDs(t, out); len(ids) != 1 || ids[0] != "healthy-fresh" {
		t.Fatalf("seconds timeout ids = %v, want [healthy-fresh]", ids)
	}

	// Filtering is query-only: every original record remains stored.
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 4 {
		t.Fatalf("discovery changed storage: %d rows err=%v", len(list), err)
	}
	if other, err := st.ListInstances("other"); err != nil || len(other) != 1 {
		t.Fatalf("other service changed: %d rows err=%v", len(other), err)
	}
}

func TestDiscoverOrdersByWeightThenInstanceID(t *testing.T) {
	router, _ := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "z-high", true, 10, lifecycleAt(8), "")
	registerForDiscovery(t, router, "svc", "a-high", true, 10, lifecycleAt(2), "")
	registerForDiscovery(t, router, "svc", "m-high", true, 10, lifecycleAt(9), "")
	registerForDiscovery(t, router, "svc", "low", true, 1, lifecycleAt(9), "")

	out := discover(t, router,
		"/api/v1/discover?service_name=svc&evaluate_at="+lifecycleAt(10)+"&heartbeat_timeout=30m")
	ids := discoverIDs(t, out)
	want := []string{"a-high", "m-high", "z-high", "low"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v (weight desc, instance id asc)", ids, want)
		}
	}

	// Every result item carries the documented fields.
	items := out["instances"].([]any)
	first := items[0].(map[string]any)
	for _, key := range []string{"instance_id", "host", "port", "address", "healthy", "weight", "heartbeat_at"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("result item missing %s: %v", key, first)
		}
	}
}

func TestDiscoverEmptyResults(t *testing.T) {
	router, _ := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "i", true, 1, lifecycleAt(0), "")

	for _, target := range []string{
		"/api/v1/discover?service_name=svc&evaluate_at=" + lifecycleAt(10) + "&heartbeat_timeout=1m",
		"/api/v1/discover?service_name=unknown&evaluate_at=" + lifecycleAt(10) + "&heartbeat_timeout=1m",
	} {
		rec := lifecycleRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", target, rec.Code)
		}
		if ids := discoverIDs(t, decodeLifecycleBody(t, rec)); len(ids) != 0 {
			t.Fatalf("%s ids = %v, want empty", target, ids)
		}
	}
}

func TestDiscoverValidationTouchesNothing(t *testing.T) {
	router, st := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "i", true, 1, lifecycleAt(5), "")
	goodEvaluate := lifecycleAt(10)
	cases := []string{
		"/api/v1/discover?evaluate_at=" + goodEvaluate + "&heartbeat_timeout=1m",
		"/api/v1/discover?service_name=svc&heartbeat_timeout=1m",
		"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate,
		"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate + "&heartbeat_timeout=0",
		"/api/v1/discover?service_name=svc&evaluate_at=" + goodEvaluate + "&heartbeat_timeout=-5s",
		"/api/v1/discover?service_name=svc&evaluate_at=garbage&heartbeat_timeout=1m",
	}
	for _, target := range cases {
		rec := lifecycleRequest(t, router, http.MethodGet, target, nil)
		expectInvalidParameter(t, rec)
		if list, err := st.ListInstances("svc"); err != nil || len(list) != 1 {
			t.Fatalf("%s changed storage: %d rows err=%v", target, len(list), err)
		}
	}
}

func TestDiscoverAcceptsFutureHeartbeat(t *testing.T) {
	router, _ := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "clock-ahead", true, 1, lifecycleAt(9), "")
	// A heartbeat later than evaluate_at is not a discovery error: the
	// freshness comparison keeps the instance eligible.
	out := discover(t, router,
		"/api/v1/discover?service_name=svc&evaluate_at="+lifecycleAt(5)+"&heartbeat_timeout=10m")
	if ids := discoverIDs(t, out); len(ids) != 1 || ids[0] != "clock-ahead" {
		t.Fatalf("future heartbeat ids = %v", ids)
	}
}

func TestDiscoverAcceptsJSONBody(t *testing.T) {
	router, _ := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "i-1", true, 1, lifecycleAt(9), "")
	rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": lifecycleAt(10), "heartbeat_timeout": 300,
	})
	if rec.Code != 200 {
		t.Fatalf("post discover: %d %s", rec.Code, rec.Body.String())
	}
	if ids := discoverIDs(t, decodeLifecycleBody(t, rec)); len(ids) != 1 || ids[0] != "i-1" {
		t.Fatalf("post discover ids = %v", ids)
	}
}

func TestCleanupDeletesLostInstancesAndReturnsIds(t *testing.T) {
	router, st := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "z-lost", true, 10, lifecycleAt(-1), "")
	registerForDiscovery(t, router, "svc", "a-lost", false, 5, lifecycleAt(-5), "")
	registerForDiscovery(t, router, "svc", "boundary", true, 5, lifecycleAt(0), "")
	registerForDiscovery(t, router, "svc", "fresh", true, 1, lifecycleAt(9), "")
	registerForDiscovery(t, router, "other", "survivor", true, 1, lifecycleAt(-60), "")

	for _, target := range []string{
		"/api/v1/services/svc/cleanup",
	} {
		rec := lifecycleRequest(t, router, http.MethodPost, target, map[string]any{
			"evaluate_at": lifecycleAt(10), "heartbeat_timeout": "10m",
		})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		out := decodeLifecycleBody(t, rec)
		ids := out["deleted_instance_ids"].([]any)
		if len(ids) != 2 || ids[0] != "a-lost" || ids[1] != "z-lost" {
			t.Fatalf("deleted ids = %v, want [a-lost z-lost]", ids)
		}
	}

	if _, found, _ := st.GetInstance("svc", "boundary"); !found {
		t.Fatalf("heartbeat equal to the deadline must survive cleanup")
	}
	if _, found, _ := st.GetInstance("svc", "fresh"); !found {
		t.Fatalf("fresh instance must survive cleanup")
	}
	if _, found, _ := st.GetInstance("other", "survivor"); !found {
		t.Fatalf("cleanup touched another service")
	}

	// Running cleanup again removes nothing and returns an empty list.
	rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"service_name": "svc", "evaluate_at": lifecycleAt(10), "heartbeat_timeout": 600,
	})
	out := decodeLifecycleBody(t, rec)
	if ids := out["deleted_instance_ids"].([]any); len(ids) != 0 {
		t.Fatalf("second cleanup = %v, want []", ids)
	}

	// An unknown service is an empty deletion set, not an error.
	rec = lifecycleRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"service_name": "missing", "evaluate_at": lifecycleAt(10), "heartbeat_timeout": 600,
	})
	if rec.Code != 200 {
		t.Fatalf("unknown service cleanup: %d %s", rec.Code, rec.Body.String())
	}
	if ids := decodeLifecycleBody(t, rec)["deleted_instance_ids"].([]any); len(ids) != 0 {
		t.Fatalf("unknown service cleanup = %v, want []", ids)
	}
}

func TestCleanupValidationTouchesNothing(t *testing.T) {
	router, st := lifecycleRouter(t)
	registerForDiscovery(t, router, "svc", "lost", true, 1, lifecycleAt(0), "")
	cases := []map[string]any{
		{"evaluate_at": lifecycleAt(10), "heartbeat_timeout": "10m"},
		{"service_name": "svc", "heartbeat_timeout": "10m"},
		{"service_name": "svc", "evaluate_at": lifecycleAt(10)},
		{"service_name": "svc", "evaluate_at": lifecycleAt(10), "heartbeat_timeout": 0},
		{"service_name": "svc", "evaluate_at": lifecycleAt(10), "heartbeat_timeout": "garbage"},
	}
	for index, body := range cases {
		rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/cleanup", body)
		expectInvalidParameter(t, rec)
		if list, _ := st.ListInstances("svc"); len(list) != 1 {
			t.Fatalf("case %d changed storage: %d rows", index, len(list))
		}
	}
}
