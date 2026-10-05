package api

import (
	"net/http"
	"testing"
	"time"
)

// The accepted range is [0000-01-01T00:00:00Z, 10000-01-01T00:00:00Z) in UTC.
const (
	lowerBoundUnix = -62167219200
	upperBoundUnix = 253402300800
)

func timeRangeRecord() map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  "i-1",
		"address":      "10.0.0.9:9090",
		"port":         9090,
		"healthy":      true,
		"weight":       5,
		"heartbeat_at": at(0),
	}
}

func TestTimeRangeLowerBoundAccepted(t *testing.T) {
	router, st := testRouter(t)

	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"unix lower bound", lowerBoundUnix, "0000-01-01T00:00:00Z"},
		{"unix lower bound string", "-62167219200", "0000-01-01T00:00:00Z"},
		{"rfc3339 lower bound", "0000-01-01T00:00:00Z", "0000-01-01T00:00:00Z"},
		{"rfc3339nano lower bound", "0000-01-01T00:00:00.000000001Z", "0000-01-01T00:00:00.000000001Z"},
		{"unix just below upper bound", upperBoundUnix - 1, "9999-12-31T23:59:59Z"},
		{"rfc3339 just below upper bound", "9999-12-31T23:59:59Z", "9999-12-31T23:59:59Z"},
		{"rfc3339nano fraction", "2026-10-01T12:00:00.123456789Z", "2026-10-01T12:00:00.123456789Z"},
		{"negative fractional unix", -0.25, "1969-12-31T23:59:59.75Z"},
		{"negative unix before epoch", -1, "1969-12-31T23:59:59Z"},
		{"numeric string", "1759324800", "2025-10-01T13:20:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := timeRangeRecord()
			record["heartbeat_at"] = tc.value
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			instance := decodeBody(t, rec)["instance"].(map[string]any)
			if instance["heartbeat_at"] != tc.want {
				t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], tc.want)
			}

			// The stored record reads back as the same instant.
			got, found, err := st.GetInstance("svc", "i-1")
			if err != nil || !found {
				t.Fatalf("get: found=%v err=%v", found, err)
			}
			want, err := time.Parse(time.RFC3339Nano, tc.want)
			if err != nil {
				t.Fatalf("parse want: %v", err)
			}
			if !got.HeartbeatAt.Equal(want) {
				t.Fatalf("stored heartbeat = %v, want %v", got.HeartbeatAt, want)
			}

			// The full query entry reports the same moment.
			rec = doRequest(t, router, http.MethodGet,
				"/api/v1/services/svc/instances/i-1", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("query status = %d body %s", rec.Code, rec.Body.String())
			}
			queried := decodeBody(t, rec)["instance"].(map[string]any)
			if queried["heartbeat_at"] != tc.want {
				t.Fatalf("queried heartbeat_at = %v, want %s", queried["heartbeat_at"], tc.want)
			}
		})
	}
}

func TestTimeRangeUpperBoundRejected(t *testing.T) {
	router, st := testRouter(t)

	cases := []struct {
		name  string
		value any
	}{
		{"unix upper bound", upperBoundUnix},
		{"unix upper bound string", "253402300800"},
		{"unix above upper bound", upperBoundUnix + 1},
		{"rfc3339 year 10000", "10000-01-01T00:00:00Z"},
		{"below lower bound", lowerBoundUnix - 1},
		{"below lower bound string", "-62167219200.5"},
		// Magnitudes that overflow int64 must not wrap back into a legal date.
		{"int64 overflow positive", 9.223372036854776e18},
		{"int64 overflow negative", -9.223372036854776e18},
		{"huge positive", 1e300},
		{"huge negative", -1e300},
		{"huge string", "1e300"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := timeRangeRecord()
			record["heartbeat_at"] = tc.value
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			if _, found, _ := st.GetInstance("svc", "i-1"); found {
				t.Fatalf("record created for out-of-range heartbeat %v", tc.value)
			}
		})
	}
}

func TestTimeRangeRejectsNonFiniteAndNonTimeValues(t *testing.T) {
	router, st := testRouter(t)

	cases := []struct {
		name  string
		value any
	}{
		{"NaN text", "NaN"},
		{"lowercase nan", "nan"},
		{"Inf text", "Inf"},
		{"plus Inf", "+Inf"},
		{"minus Inf", "-Inf"},
		{"Infinity text", "Infinity"},
		{"plus Infinity", "+Infinity"},
		{"minus Infinity", "-Infinity"},
		{"unparseable number text", "1e999"},
		{"not a time", "not-a-time"},
		{"boolean true", true},
		{"boolean false", false},
		{"array", []any{1, 2}},
		{"object", map[string]any{"time": 1}},
		{"null", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := timeRangeRecord()
			record["heartbeat_at"] = tc.value
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			if _, found, _ := st.GetInstance("svc", "i-1"); found {
				t.Fatalf("record created for invalid heartbeat %v", tc.value)
			}
		})
	}
}

func TestTimeRangeMissingHeartbeatStillRejected(t *testing.T) {
	router, st := testRouter(t)

	record := timeRangeRecord()
	delete(record, "heartbeat_at")
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatal("record created without heartbeat_at")
	}
}

func TestTimeRangeOffsetTimesJudgedInUTC(t *testing.T) {
	router, st := testRouter(t)

	// A valid offset time is accepted and reported as the same UTC moment.
	record := timeRangeRecord()
	record["heartbeat_at"] = "2026-10-01T14:00:00+02:00"
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["heartbeat_at"] != "2026-10-01T12:00:00Z" {
		t.Fatalf("heartbeat_at = %v, want 2026-10-01T12:00:00Z", instance["heartbeat_at"])
	}

	// Offset times whose UTC instant falls outside the range are rejected.
	for _, value := range []string{
		"0000-01-01T00:30:00+01:00", // UTC: year -1, before the lower bound
		"9999-12-31T23:30:00-01:00", // UTC: year 10000, past the upper bound
	} {
		record := timeRangeRecord()
		record["instance_id"] = "i-2"
		record["heartbeat_at"] = value
		rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found, _ := st.GetInstance("svc", "i-2"); found {
			t.Fatalf("record created for out-of-range offset time %s", value)
		}
	}
}

func TestHeartbeatRenewalAppliesTimeRange(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, timeRangeRecord())

	// A negative fractional Unix second renews to the same UTC moment.
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": -0.25})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["heartbeat_at"] != "1969-12-31T23:59:59.75Z" {
		t.Fatalf("heartbeat_at = %v, want 1969-12-31T23:59:59.75Z", instance["heartbeat_at"])
	}
	got, _, _ := st.GetInstance("svc", "i-1")
	if got.HeartbeatAt.Format(time.RFC3339Nano) != "1969-12-31T23:59:59.75Z" {
		t.Fatalf("stored heartbeat = %v", got.HeartbeatAt)
	}

	// Out-of-range and non-finite renewals are rejected and change nothing.
	for _, value := range []any{upperBoundUnix, "NaN", "-Infinity", lowerBoundUnix - 1, true, nil} {
		rec := doRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat",
			map[string]any{"heartbeat_at": value})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, _, _ := st.GetInstance("svc", "i-1")
		if got.HeartbeatAt.Format(time.RFC3339Nano) != "1969-12-31T23:59:59.75Z" {
			t.Fatalf("heartbeat changed after invalid renewal %v: %v", value, got.HeartbeatAt)
		}
	}
}

func TestRegisterEvaluateAtRangeChecked(t *testing.T) {
	router, st := testRouter(t)

	// A valid evaluate_at keeps the clock-skew guard working.
	record := timeRangeRecord()
	record["evaluate_at"] = at(5)
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register", record)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}

	// An out-of-range evaluate_at rejects the registration entirely.
	record = timeRangeRecord()
	record["instance_id"] = "i-2"
	record["evaluate_at"] = upperBoundUnix
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register", record)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-2"); found {
		t.Fatal("record created with out-of-range evaluate_at")
	}

	// A valid evaluate_at earlier than the heartbeat is still rejected.
	record = timeRangeRecord()
	record["instance_id"] = "i-3"
	record["evaluate_at"] = "2026-10-01T11:00:00Z"
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register", record)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-3"); found {
		t.Fatal("record created with evaluate_at earlier than heartbeat_at")
	}
}

func TestBatchRegisterInvalidTimeRejectsWholeBatch(t *testing.T) {
	router, st := testRouter(t)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "weight": 1, "heartbeat_at": at(0)},
			map[string]any{"instance_id": "i-2", "weight": 1, "heartbeat_at": upperBoundUnix},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register/batch", body)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	for _, id := range []string{"i-1", "i-2"} {
		if _, found, _ := st.GetInstance("svc", id); found {
			t.Fatalf("%s created by a batch with an invalid time", id)
		}
	}

	// The same batch with every time in range succeeds in request order.
	body["instances"] = []any{
		map[string]any{"instance_id": "i-1", "weight": 1, "heartbeat_at": at(0)},
		map[string]any{"instance_id": "i-2", "weight": 1, "heartbeat_at": -0.25},
	}
	rec = doRequest(t, router, http.MethodPost, "/api/v1/register/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	entries := decodeBody(t, rec)["instances"].([]any)
	if len(entries) != 2 ||
		entries[0].(map[string]any)["instance_id"] != "i-1" ||
		entries[1].(map[string]any)["instance_id"] != "i-2" {
		t.Fatalf("batch order not preserved: %v", entries)
	}
	if entries[1].(map[string]any)["heartbeat_at"] != "1969-12-31T23:59:59.75Z" {
		t.Fatalf("i-2 heartbeat = %v", entries[1].(map[string]any)["heartbeat_at"])
	}
}

func TestBatchHeartbeatInvalidTimeRejectsWholeBatch(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, timeRangeRecord())
	second := timeRangeRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	body := map[string]any{
		"service_name": "svc",
		"instances": []any{
			map[string]any{"instance_id": "i-1", "heartbeat_at": at(20)},
			map[string]any{"instance_id": "i-2", "heartbeat_at": "Infinity"},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	for _, id := range []string{"i-1", "i-2"} {
		got, found, _ := st.GetInstance("svc", id)
		if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
			t.Fatalf("%s changed after invalid batch: %+v", id, got)
		}
	}
}

func TestEvaluateEndpointsRejectInvalidTimesWithoutChanges(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, timeRangeRecord())
	other := timeRangeRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)

	invalidValues := []any{upperBoundUnix, "NaN", "+Inf", "not-a-time", true, nil}
	for _, value := range invalidValues {
		// Discovery.
		rec := doRequest(t, router, http.MethodPost, "/api/v1/discover", map[string]any{
			"service_name": "svc", "evaluate_at": value, "heartbeat_timeout": "1m"})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

		// Batch discovery.
		rec = doRequest(t, router, http.MethodPost, "/api/v1/discover/batch", map[string]any{
			"service_names": []any{"svc", "other"}, "evaluate_at": value, "heartbeat_timeout": "1m"})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

		// Standalone cleanup.
		rec = doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
			"evaluate_at": value, "heartbeat_timeout": "1m"})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

		// Service overview.
		rec = doRequest(t, router, http.MethodGet,
			"/api/v1/services?evaluate_at=253402300800&heartbeat_timeout=60", nil)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

		// No record of any service was created, updated or deleted.
		for _, service := range []string{"svc", "other"} {
			got, found, _ := st.GetInstance(service, "i-1")
			if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
				t.Fatalf("%s changed after invalid evaluate_at %v: %+v", service, value, got)
			}
		}
	}
}

// TestTimeRangeValidFlowRegression walks the valid-input path end to end:
// register, full query, renewal, re-query, discovery and cleanup, confirming
// the range check never blocks legitimate input and scoped requests leave
// other services alone.
func TestTimeRangeValidFlowRegression(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, timeRangeRecord())
	other := timeRangeRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)

	// Full query after registration.
	rec := doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["heartbeat_at"] != baseTime || instance["weight"].(float64) != 5 ||
		instance["address"] != "10.0.0.9:9090" || instance["healthy"] != true {
		t.Fatalf("unexpected record after register: %v", instance)
	}

	// Renewal replaces only the heartbeat.
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": at(30)})
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d body %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances/i-1", nil)
	instance = decodeBody(t, rec)["instance"].(map[string]any)
	if instance["heartbeat_at"] != at(30) || instance["weight"].(float64) != 5 {
		t.Fatalf("renewal changed more than heartbeat: %v", instance)
	}

	// Discovery at a valid evaluation point: exactly heartbeat+timeout stays
	// online; one second strictly past cleans the record up.
	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at=2026-10-01T13:00:00Z&heartbeat_timeout=30m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status = %d body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "i-1" {
		t.Fatalf("boundary evaluate ids = %v, want [i-1]", ids)
	}
	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at=2026-10-01T13:00:01Z&heartbeat_timeout=30m", nil)
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 0 {
		t.Fatalf("strictly lost instance still discovered: %v", ids)
	}
	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatal("strictly lost instance not cleaned up")
	}

	// The other service was never touched by the svc-scoped requests.
	got, found, _ := st.GetInstance("other", "i-1")
	if !found || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("other service changed by svc-scoped requests: %+v", got)
	}

	// Cleanup with a valid evaluation point removes the other service's lost
	// record and leaves storage consistent for the overview.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"service_name": "other", "evaluate_at": at(31), "heartbeat_timeout": "1m"})
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d body %s", rec.Code, rec.Body.String())
	}
	if removed := decodeBody(t, rec)["removed"].(float64); removed != 1 {
		t.Fatalf("removed = %v, want 1", removed)
	}
	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/services?evaluate_at="+at(31)+"&heartbeat_timeout=60", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d body %s", rec.Code, rec.Body.String())
	}
	if services := decodeBody(t, rec)["services"].([]any); len(services) != 0 {
		t.Fatalf("overview after cleanup = %v, want empty", services)
	}
}
