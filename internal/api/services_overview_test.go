package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func servicesOverview(t *testing.T, router http.Handler, target string) (int, map[string]any) {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview %s: status %d body %s", target, rec.Code, rec.Body.String())
	}
	return rec.Code, decodeBody(t, rec)
}

func overviewCounters(t *testing.T, out map[string]any) map[string]map[string]int {
	t.Helper()
	rawList, ok := out["services"].([]any)
	if !ok {
		t.Fatalf("services is not a list: %v", out)
	}
	counters := make(map[string]map[string]int, len(rawList))
	for _, raw := range rawList {
		item := raw.(map[string]any)
		name := item["service_name"].(string)
		counters[name] = map[string]int{
			"total":           int(item["total_instances"].(float64)),
			"available":       int(item["available_instances"].(float64)),
			"unhealthy_fresh": int(item["unhealthy_fresh_instances"].(float64)),
			"lost":            int(item["lost_instances"].(float64)),
		}
	}
	return counters
}

func TestServiceOverviewEmptySnapshot(t *testing.T) {
	router, _ := testRouter(t)
	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(10)+"&heartbeat_timeout=30s", nil))
	list, ok := out["services"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("services = %v, want empty list", out["services"])
	}
}

func TestServiceOverviewAggregatesAndSorts(t *testing.T) {
	router, _ := testRouter(t)

	// alpha: one healthy instance on the lost boundary (still available),
	// one unhealthy-but-fresh instance and one strictly lost instance.
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "boundary", "healthy": true,
		"weight": 10, "heartbeat_at": at(5),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "sick", "healthy": false,
		"weight": 5, "heartbeat_at": at(8),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "gone", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	// zeta sorts after alpha and only has one fresh healthy instance.
	registerInstance(t, router, map[string]any{
		"service_name": "zeta", "instance_id": "z-1", "healthy": true,
		"weight": 2, "heartbeat_at": at(9),
	})

	_, out := servicesOverview(t, router,
		"/api/v1/services?evaluate_at="+at(10)+"&heartbeat_timeout=5m")

	rawList := out["services"].([]any)
	if len(rawList) != 2 {
		t.Fatalf("services = %v, want two entries", rawList)
	}
	if rawList[0].(map[string]any)["service_name"] != "alpha" ||
		rawList[1].(map[string]any)["service_name"] != "zeta" {
		t.Fatalf("service order = %v, want [alpha zeta]", rawList)
	}

	counters := overviewCounters(t, out)
	alpha := counters["alpha"]
	if alpha["total"] != 3 || alpha["available"] != 1 ||
		alpha["unhealthy_fresh"] != 1 || alpha["lost"] != 1 {
		t.Fatalf("alpha counters = %v", alpha)
	}
	zeta := counters["zeta"]
	if zeta["total"] != 1 || zeta["available"] != 1 ||
		zeta["unhealthy_fresh"] != 0 || zeta["lost"] != 0 {
		t.Fatalf("zeta counters = %v", zeta)
	}
}

func TestServiceOverviewAcceptsUnixSecondsAndDurationText(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	evaluate, err := time.Parse(time.RFC3339, at(5))
	if err != nil {
		t.Fatalf("parse evaluate: %v", err)
	}
	target := "/api/v1/services?evaluate_at=" + strconv.FormatInt(evaluate.Unix(), 10) +
		"&heartbeat_timeout=300s"
	_, out := servicesOverview(t, router, target)
	counters := overviewCounters(t, out)["svc"]
	if counters["total"] != 1 || counters["available"] != 1 || counters["lost"] != 0 {
		t.Fatalf("boundary via unix/duration counters = %v", counters)
	}
}

func TestServiceOverviewIsReadOnly(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "gone", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "fresh", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})

	for range 2 {
		servicesOverview(t, router,
			"/api/v1/services?evaluate_at="+at(10)+"&heartbeat_timeout=1m")
	}
	// The overview counts lost records but never deletes them.
	if _, found, err := st.GetInstance("svc", "gone"); err != nil || !found {
		t.Fatalf("lost record changed by overview: found=%v err=%v", found, err)
	}
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 2 {
		t.Fatalf("overview changed records: %d err=%v", len(list), err)
	}

	// Discovery keeps its own synchronous lost cleanup behavior.
	discover := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil))
	if ids := instanceIDs(t, discover); len(ids) != 1 || ids[0] != "fresh" {
		t.Fatalf("discover ids = %v, want [fresh]", ids)
	}
	if _, found, _ := st.GetInstance("svc", "gone"); found {
		t.Fatalf("discovery cleanup no longer removes lost records")
	}
}

func TestServiceOverviewParameterErrors(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(9),
	})

	cases := []string{
		"heartbeat_timeout=1m",
		"evaluate_at=not-a-time&heartbeat_timeout=1m",
		"evaluate_at=" + at(10),
		"evaluate_at=" + at(10) + "&heartbeat_timeout=0",
		"evaluate_at=" + at(10) + "&heartbeat_timeout=-5",
		"evaluate_at=" + at(10) + "&heartbeat_timeout=soon",
		// evaluate_at earlier than any stored heartbeat is rejected.
		"evaluate_at=" + at(5) + "&heartbeat_timeout=1m",
	}
	for _, query := range cases {
		rec := doRequest(t, router, http.MethodGet, "/api/v1/services?"+query, nil)
		expectParameterError(t, rec)
	}
	if list, err := st.ListInstances("svc"); err != nil || len(list) != 1 {
		t.Fatalf("rejected overview changed records: %d err=%v", len(list), err)
	}
}

func TestServiceOverviewStorageUnavailable(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "storage_unavailable" {
		t.Fatalf("code = %v, want storage_unavailable", errObj["code"])
	}
	message := errObj["message"].(string)
	for _, leaked := range []string{"SQL", "sql", "/", ".go", "goroutine"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("error message leaks internals: %q", message)
		}
	}
}
