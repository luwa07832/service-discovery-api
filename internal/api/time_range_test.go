package api

import (
	"net/http"
	"testing"
	"time"
)

// registerBody builds a minimal valid registration body with the given
// heartbeat value, so each case can vary exactly the timestamp under test.
func registerBody(heartbeat any) map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  "i-1",
		"address":      "10.0.0.8:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       10,
		"heartbeat_at": heartbeat,
	}
}

// TestRegisterRejectsOutOfRangeAndMalformedTimes covers every heartbeat_at
// shape that must fail with HTTP 400 and invalid_parameter without creating
// a record: the exclusive upper bound, values just past either bound,
// non-finite numbers in every spelling, unparseable numeric text, finite
// numbers large enough to overflow the int64 conversion and wrap, and
// non-scalar JSON values.
func TestRegisterRejectsOutOfRangeAndMalformedTimes(t *testing.T) {
	router, st := testRouter(t)

	cases := []struct {
		name  string
		value any
	}{
		{"upper bound excluded", 253402300800},
		{"above upper bound", 253402300800.5},
		{"far above upper bound", 1e300},
		{"below lower bound", -62167219201},
		{"just below lower bound", -62167219200.5},
		{"far below lower bound", -1e300},
		{"nan text", "NaN"},
		{"inf text", "Inf"},
		{"plus inf text", "+Inf"},
		{"minus inf text", "-Inf"},
		{"infinity text", "Infinity"},
		{"minus infinity text", "-Infinity"},
		{"unparseable numeric text", "12x34"},
		{"boolean", true},
		{"array", []any{1}},
		{"object", map[string]any{"at": 1}},
		{"null", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register", registerBody(tc.value))
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			if _, found, _ := st.GetInstance("svc", "i-1"); found {
				t.Fatalf("record created after rejected heartbeat_at %v", tc.value)
			}
		})
	}
}

// TestRegisterRejectsInvalidTimeFromQuery covers the query-parameter entry:
// an out-of-range Unix second count in the query string is rejected exactly
// like one in the JSON body, and a missing heartbeat keeps the same error.
func TestRegisterRejectsInvalidTimeFromQuery(t *testing.T) {
	router, st := testRouter(t)

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/register?service_name=svc&instance_id=i-1&weight=10&heartbeat_at=253402300800", nil)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/register?service_name=svc&instance_id=i-1&weight=10", nil)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatal("record created after rejected query-parameter heartbeat_at")
	}
}

// TestRegisterAcceptsBoundaryAndLegacyTimes covers the timestamps that must
// keep working: the inclusive lower bound, the last second before the upper
// bound, a negative fractional Unix second, a numeric string, an RFC3339
// value with nanoseconds and an offset time judged by its UTC instant.
func TestRegisterAcceptsBoundaryAndLegacyTimes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"lower bound included", -62167219200, "0000-01-01T00:00:00Z"},
		{"last second before upper bound", 253402300799, "9999-12-31T23:59:59Z"},
		{"negative fraction", -0.25, "1969-12-31T23:59:59.75Z"},
		{"numeric string", "1759324800", "2025-10-01T13:20:00Z"},
		{"rfc3339 nano", "2026-10-01T12:00:00.123456789Z", "2026-10-01T12:00:00.123456789Z"},
		{"offset judged as utc", "0000-01-01T01:00:00+01:00", "0000-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register", registerBody(tc.value))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			instance := decodeBody(t, rec)["instance"].(map[string]any)
			if instance["heartbeat_at"] != tc.want {
				t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], tc.want)
			}
			// The stored record must round-trip through the RFC3339 storage
			// format and a full query must return the same instant.
			got, found, err := st.GetInstance("svc", "i-1")
			if err != nil || !found {
				t.Fatalf("get: found=%v err=%v", found, err)
			}
			want, _ := time.Parse(time.RFC3339Nano, tc.want)
			if !got.HeartbeatAt.Equal(want) {
				t.Fatalf("stored heartbeat = %v, want %v", got.HeartbeatAt, want)
			}
			query := doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances/i-1", nil)
			if query.Code != http.StatusOK {
				t.Fatalf("query status = %d body %s", query.Code, query.Body.String())
			}
			queried := decodeBody(t, query)["instance"].(map[string]any)
			if queried["heartbeat_at"] != tc.want {
				t.Fatalf("queried heartbeat_at = %v, want %s", queried["heartbeat_at"], tc.want)
			}
		})
	}
}

// TestRegisterRejectsOffsetTimesOutsideRange converts offset times to UTC
// before the range check: an offset time whose UTC instant falls before the
// lower bound or at the upper bound is rejected even though its local
// representation looks in range.
func TestRegisterRejectsOffsetTimesOutsideRange(t *testing.T) {
	router, st := testRouter(t)

	cases := []struct {
		name  string
		value any
	}{
		{"offset before lower bound", "0000-01-01T00:00:00+01:00"},
		{"offset exactly at upper bound", "9999-12-31T23:59:59-00:01"},
		{"offset past upper bound", "9999-12-31T23:30:00-01:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register", registerBody(tc.value))
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			if _, found, _ := st.GetInstance("svc", "i-1"); found {
				t.Fatalf("record created after rejected heartbeat_at %v", tc.value)
			}
		})
	}
}

// TestRegisterEvaluateAtRange covers the optional evaluate_at on
// registration: out-of-range values are rejected like heartbeat_at, and a
// valid in-range pair still passes the clock-skew guard.
func TestRegisterEvaluateAtRange(t *testing.T) {
	router, st := testRouter(t)

	body := registerBody(at(0))
	body["evaluate_at"] = 253402300800
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register", body)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatal("record created after rejected evaluate_at")
	}

	body = registerBody(at(0))
	body["evaluate_at"] = "NaN"
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register", body)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatal("record created after rejected evaluate_at")
	}

	// A valid evaluation point at the heartbeat instant itself is accepted.
	body = registerBody(at(0))
	body["evaluate_at"] = at(0)
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
}

// TestHeartbeatRenewalRangeValidation renews with a negative fractional Unix
// second and confirms the stored record and every other field, then confirms
// out-of-range renewals are rejected without touching the record.
func TestHeartbeatRenewalRangeValidation(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": -0.25})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["heartbeat_at"] != "1969-12-31T23:59:59.75Z" {
		t.Fatalf("heartbeat_at = %v", instance["heartbeat_at"])
	}
	if instance["address"] != "10.0.0.8:8080" || instance["port"].(float64) != 8080 ||
		instance["healthy"] != true || instance["weight"].(float64) != 10 {
		t.Fatalf("renewal changed other fields: %v", instance)
	}

	// A full query after the renewal returns the same instant.
	query := doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances/i-1/heartbeat", nil)
	if query.Code != http.StatusOK {
		t.Fatalf("query status = %d body %s", query.Code, query.Body.String())
	}
	if got := decodeBody(t, query)["heartbeat_at"]; got != "1969-12-31T23:59:59.75Z" {
		t.Fatalf("queried heartbeat_at = %v", got)
	}

	for _, value := range []any{253402300800, -62167219201, "Inf", "12x34", false, nil} {
		rec := doRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat",
			map[string]any{"heartbeat_at": value})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, found, _ := st.GetInstance("svc", "i-1")
		if !found || got.HeartbeatAt.Format(time.RFC3339Nano) != "1969-12-31T23:59:59.75Z" {
			t.Fatalf("record changed after rejected renewal %v: %+v", value, got)
		}
	}
}

// TestHeartbeatRenewalMissingTargetStill404 keeps the existing contract: a
// valid renewal for a missing instance is a 404, not a parameter error.
func TestHeartbeatRenewalMissingTargetStill404(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/ghost/heartbeat",
		map[string]any{"heartbeat_at": at(5)})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
}

// TestBatchRegisterRejectsWholeBatchOnInvalidTime registers two instances in
// one batch where one heartbeat is out of range: the whole batch fails and
// neither record exists afterwards.
func TestBatchRegisterRejectsWholeBatchOnInvalidTime(t *testing.T) {
	router, st := testRouter(t)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "weight": 10, "heartbeat_at": at(0)},
			map[string]any{"instance_id": "i-2", "weight": 5, "heartbeat_at": 253402300800},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register/batch", body)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

	for _, id := range []string{"i-1", "i-2"} {
		if _, found, _ := st.GetInstance("svc", id); found {
			t.Fatalf("%s created by a rejected batch", id)
		}
	}
}

// TestBatchHeartbeatRejectsWholeBatchOnInvalidTime renews two instances in
// one batch where one heartbeat is out of range: the whole batch fails and
// both records keep their previous heartbeats.
func TestBatchHeartbeatRejectsWholeBatchOnInvalidTime(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))
	second := registerBody(at(0))
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(10)},
			map[string]any{"instance_id": "i-2", "heartbeat_at": "Infinity"},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

	for _, id := range []string{"i-1", "i-2"} {
		got, found, _ := st.GetInstance("svc", id)
		if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
			t.Fatalf("%s changed after rejected batch: %+v", id, got)
		}
	}
}

// TestBatchHeartbeatSuccessKeepsRequestOrder confirms a fully valid batch
// renewal still applies every entry and answers in request order.
func TestBatchHeartbeatSuccessKeepsRequestOrder(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))
	second := registerBody(at(0))
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-2", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "i-1", "heartbeat_at": -0.25},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	instances := out["instances"].([]any)
	if len(instances) != 2 {
		t.Fatalf("instances len = %d", len(instances))
	}
	first := instances[0].(map[string]any)
	secondEntry := instances[1].(map[string]any)
	if first["instance_id"] != "i-2" || secondEntry["instance_id"] != "i-1" {
		t.Fatalf("order not preserved: %v %v", first["instance_id"], secondEntry["instance_id"])
	}
	if secondEntry["heartbeat_at"] != "1969-12-31T23:59:59.75Z" {
		t.Fatalf("i-1 heartbeat_at = %v", secondEntry["heartbeat_at"])
	}
}

// TestEvaluateEndpointsRejectInvalidTimes covers discover, batch discover,
// cleanup and the service overview: an invalid evaluate_at fails with HTTP
// 400 and invalid_parameter and changes no record.
func TestEvaluateEndpointsRejectInvalidTimes(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))

	invalid := []any{253402300800, -62167219201, "NaN", "-Infinity", "12x34", true, nil}
	for _, value := range invalid {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
			"service_name": "svc", "evaluate_at": value, "heartbeat_timeout": 1800,
		})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

		rec = doRequest(t, router, http.MethodPost, "/api/v1/discover/batch", map[string]any{
			"service_names": []any{"svc"}, "evaluate_at": value, "heartbeat_timeout": 1800,
		})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

		rec = doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
			"service_name": "svc", "evaluate_at": value, "heartbeat_timeout": 1800,
		})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	}

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at=253402300800&heartbeat_timeout=1800", nil)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("record changed after rejected evaluations: %+v", got)
	}
}

// TestDiscoverSkewGuardAndBoundaryBehaviour keeps the discovery contract:
// an evaluation earlier than a stored heartbeat is a 400, an instance
// exactly on the heartbeat-plus-timeout boundary stays online, only a
// strictly later evaluation cleans it up, and a scoped request never
// touches another service. Weight-descending order is preserved.
func TestDiscoverSkewGuardAndBoundaryBehaviour(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))
	second := registerBody(at(0))
	second["instance_id"] = "i-2"
	second["weight"] = 20
	registerInstance(t, router, second)
	other := registerBody(at(0))
	other["service_name"] = "other"
	other["instance_id"] = "i-9"
	registerInstance(t, router, other)

	// Evaluation earlier than a stored heartbeat is rejected and deletes
	// nothing.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": "2026-10-01T11:00:00Z", "heartbeat_timeout": 1800,
	})
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if instances, _ := st.ListInstances("svc"); len(instances) != 2 {
		t.Fatalf("svc records changed after skew rejection: %d", len(instances))
	}

	// Exactly on the boundary both instances are still online, heavier
	// weight first.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": at(30), "heartbeat_timeout": 1800,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if ids := instanceIDs(t, out); len(ids) != 2 || ids[0] != "i-2" || ids[1] != "i-1" {
		t.Fatalf("ids = %v, want [i-2 i-1]", ids)
	}

	// Strictly past the boundary both svc records are cleaned up while the
	// other service keeps its record.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": at(31), "heartbeat_timeout": 1800,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 0 {
		t.Fatalf("ids = %v, want empty", ids)
	}
	if instances, _ := st.ListInstances("svc"); len(instances) != 0 {
		t.Fatalf("svc records remain after cleanup: %d", len(instances))
	}
	if _, found, _ := st.GetInstance("other", "i-9"); !found {
		t.Fatal("other service record touched by scoped discovery")
	}
}

// TestCleanupBoundaryAndScoping covers the standalone cleanup entry: the
// boundary instant removes nothing, a strictly later evaluation removes
// exactly the lost records of the named service, and an invalid evaluate_at
// removes nothing.
func TestCleanupBoundaryAndScoping(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))
	second := registerBody(at(0))
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)
	other := registerBody(at(0))
	other["service_name"] = "other"
	other["instance_id"] = "i-9"
	registerInstance(t, router, other)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"service_name": "svc", "evaluate_at": at(30), "heartbeat_timeout": 1800,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if removed := decodeBody(t, rec)["removed"].(float64); removed != 0 {
		t.Fatalf("removed = %v on the boundary, want 0", removed)
	}

	rec = doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"service_name": "svc", "evaluate_at": at(31), "heartbeat_timeout": 1800,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if removed := decodeBody(t, rec)["removed"].(float64); removed != 2 {
		t.Fatalf("removed = %v past the boundary, want 2", removed)
	}
	if _, found, _ := st.GetInstance("other", "i-9"); !found {
		t.Fatal("other service record touched by scoped cleanup")
	}
}

// TestServicesOverviewStaysReadOnly confirms the overview aggregates at a
// valid evaluation point without deleting lost records, and that a scoped
// request leaves other services' records alone.
func TestServicesOverviewStaysReadOnly(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))
	other := registerBody(at(0))
	other["service_name"] = "other"
	other["instance_id"] = "i-9"
	registerInstance(t, router, other)

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(31)+"&heartbeat_timeout=1800", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	services := out["services"].([]any)
	if len(services) != 2 {
		t.Fatalf("services len = %d", len(services))
	}
	for _, raw := range services {
		service := raw.(map[string]any)
		if service["total_instances"].(float64) != 1 || service["lost_instances"].(float64) != 1 {
			t.Fatalf("unexpected counters: %v", service)
		}
	}
	// Read-only: both records are still stored after the overview ran.
	if all, _ := st.ListAllInstances(); len(all) != 2 {
		t.Fatalf("overview changed stored records: %d", len(all))
	}
}

// TestRepeatedRegistrationStillOverwrites keeps the upsert contract: a
// repeated registration with a valid time replaces the whole record.
func TestRepeatedRegistrationStillOverwrites(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, registerBody(at(0)))

	updated := registerBody(at(5))
	updated["weight"] = 42
	updated["healthy"] = false
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register", updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Weight != 42 || got.Healthy ||
		!got.HeartbeatAt.Equal(mustParseTime(at(5))) {
		t.Fatalf("record not overwritten: %+v", got)
	}
}
