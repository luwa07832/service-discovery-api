package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func overviewServices(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	rawList, ok := out["services"].([]any)
	if !ok {
		t.Fatalf("services is not a list: %v", out)
	}
	services := make([]map[string]any, 0, len(rawList))
	for _, raw := range rawList {
		services = append(services, raw.(map[string]any))
	}
	return services
}

func registerOverviewInstance(t *testing.T, router http.Handler, serviceName, instanceID string, healthy bool, heartbeatAt string) {
	t.Helper()
	registerInstance(t, router, map[string]any{
		"service_name": serviceName,
		"instance_id":  instanceID,
		"weight":       1,
		"healthy":      healthy,
		"heartbeat_at": heartbeatAt,
	})
}

func TestServiceOverviewEmptyWhenNoRecords(t *testing.T) {
	router, _ := testRouter(t)

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+baseTime+"&heartbeat_timeout=30s", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if services := overviewServices(t, out); len(services) != 0 {
		t.Fatalf("services = %v, want empty list", services)
	}
}

func TestServiceOverviewCountsBucketsAndSortsServices(t *testing.T) {
	router, st := testRouter(t)
	registerOverviewInstance(t, router, "zebra", "z-lost", true, at(0))
	registerOverviewInstance(t, router, "zebra", "z-healthy", true, "2026-10-01T12:59:45Z")
	registerOverviewInstance(t, router, "zebra", "z-unhealthy", false, "2026-10-01T12:59:50Z")
	registerOverviewInstance(t, router, "zebra", "z-boundary", true, "2026-10-01T12:59:30Z")
	registerOverviewInstance(t, router, "alpha", "a-healthy", true, "2026-10-01T12:59:45Z")

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(60)+"&heartbeat_timeout=30s", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	services := overviewServices(t, decodeBody(t, rec))
	if len(services) != 2 {
		t.Fatalf("len = %d, want 2: %v", len(services), services)
	}
	if services[0]["service_name"] != "alpha" || services[1]["service_name"] != "zebra" {
		t.Fatalf("service order = %s, %s", services[0]["service_name"], services[1]["service_name"])
	}
	zebra := services[1]
	expectCounts := func(item map[string]any, total, available, unhealthyFresh, lost float64) {
		t.Helper()
		if item["total_instances"] != total ||
			item["available_instances"] != available ||
			item["unhealthy_fresh_instances"] != unhealthyFresh ||
			item["lost_instances"] != lost {
			t.Fatalf("counts = total %v available %v unhealthyFresh %v lost %v, want %v/%v/%v/%v",
				item["total_instances"], item["available_instances"],
				item["unhealthy_fresh_instances"], item["lost_instances"],
				total, available, unhealthyFresh, lost)
		}
	}
	expectCounts(services[0], 1, 1, 0, 0)
	// The boundary instance (heartbeat 12:30, evaluated 12:60 + 30s) is not lost.
	expectCounts(zebra, 4, 2, 1, 1)

	// Read-only: every record, including the strictly lost one, is still stored.
	for _, key := range [][2]string{{"zebra", "z-lost"}, {"zebra", "z-boundary"}, {"alpha", "a-healthy"}} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || !found {
			t.Fatalf("record %s/%s missing after overview: found=%v err=%v", key[0], key[1], found, err)
		}
	}
}

func TestServiceOverviewAcceptsUnixSecondsAndDurationForms(t *testing.T) {
	router, _ := testRouter(t)
	registerOverviewInstance(t, router, "svc", "i-1", true, baseTime)

	baseUnix := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()
	// Unix seconds for evaluate_at and numeric seconds for heartbeat_timeout.
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+strconv.FormatInt(baseUnix, 10)+"&heartbeat_timeout=60", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unix/numeric: status = %d, body %s", rec.Code, rec.Body.String())
	}
	services := overviewServices(t, decodeBody(t, rec))
	if len(services) != 1 || services[0]["available_instances"] != float64(1) {
		t.Fatalf("unexpected services: %v", services)
	}

	// Minute duration text for heartbeat_timeout.
	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+strconv.FormatInt(baseUnix+60, 10)+"&heartbeat_timeout=2m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("2m duration: status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestServiceOverviewRejectsMissingOrInvalidParameters(t *testing.T) {
	router, _ := testRouter(t)

	for _, target := range []string{
		"/api/v1/services?heartbeat_timeout=30s",
		"/api/v1/services?evaluate_at=not-a-time&heartbeat_timeout=30s",
		"/api/v1/services?evaluate_at=" + baseTime,
		"/api/v1/services?evaluate_at=" + baseTime + "&heartbeat_timeout=0",
		"/api/v1/services?evaluate_at=" + baseTime + "&heartbeat_timeout=-5s",
		"/api/v1/services?evaluate_at=" + baseTime + "&heartbeat_timeout=not-a-duration",
	} {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		expectParameterError(t, rec)
	}
}

func TestServiceOverviewRejectsEvaluateAtEarlierThanAnyHeartbeat(t *testing.T) {
	router, st := testRouter(t)
	registerOverviewInstance(t, router, "svc-a", "early", true, at(0))
	registerOverviewInstance(t, router, "svc-b", "late", true, at(30))

	// 12:10 is earlier than svc-b's 12:30 heartbeat even though it follows svc-a's.
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(10)+"&heartbeat_timeout=30s", nil)
	expectParameterError(t, rec)

	if _, found, err := st.GetInstance("svc-a", "early"); err != nil || !found {
		t.Fatalf("svc-a record changed after rejected overview: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetInstance("svc-b", "late"); err != nil || !found {
		t.Fatalf("svc-b record changed after rejected overview: found=%v err=%v", found, err)
	}
}

func TestServiceOverviewNeverRunsLostCleanup(t *testing.T) {
	router, st := testRouter(t)
	registerOverviewInstance(t, router, "svc", "lost", true, at(0))

	for range 2 {
		rec := doRequest(t, router, http.MethodGet,
			"/api/v1/services?evaluate_at="+at(60)+"&heartbeat_timeout=30s", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		services := overviewServices(t, decodeBody(t, rec))
		if services[0]["lost_instances"] != float64(1) {
			t.Fatalf("lost = %v, want 1: %v", services[0]["lost_instances"], services)
		}
	}
	if _, found, err := st.GetInstance("svc", "lost"); err != nil || !found {
		t.Fatalf("lost record removed by overview: found=%v err=%v", found, err)
	}
}
