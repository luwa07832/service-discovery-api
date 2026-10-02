package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"

	"github.com/luwa07832/service-discovery-api/internal/store"
	"testing"
)

func doRawRequest(t *testing.T, router http.Handler, method, target, rawBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func weightRecord() map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  "i-1",
		"address":      "10.0.0.8:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       10,
		"heartbeat_at": baseTime,
	}
}

func assertWeightUnchanged(t *testing.T, st *store.Store) {
	t.Helper()
	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.Address != "10.0.0.8:8080" || got.Port != 8080 || !got.Healthy || got.Weight != 10 ||
		!got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("record changed after failed request: %+v", got)
	}
}

func TestUpdateWeightChangesOnlyWeight(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight",
		map[string]any{"weight": 25})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "svc" || instance["instance_id"] != "i-1" ||
		instance["address"] != "10.0.0.8:8080" || instance["port"].(float64) != 8080 ||
		instance["healthy"] != true || instance["weight"].(float64) != 25 {
		t.Fatalf("unexpected response record: %v", instance)
	}
	if instance["heartbeat_at"] != baseTime {
		t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], baseTime)
	}

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Weight != 25 || got.Address != "10.0.0.8:8080" ||
		got.Port != 8080 || !got.Healthy || !got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
		t.Fatalf("stored record mismatch: %+v found=%v", got, found)
	}
}

func TestUpdateWeightAcceptsStringAndQueryParameter(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight",
		map[string]any{"weight": "3.5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("string weight: status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["weight"].(float64) != 3.5 {
		t.Fatalf("weight = %v", instance["weight"])
	}

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight?weight=42", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query weight: status = %d body %s", rec.Code, rec.Body.String())
	}
	instance = decodeBody(t, rec)["instance"].(map[string]any)
	if instance["weight"].(float64) != 42 {
		t.Fatalf("weight = %v", instance["weight"])
	}
}

func TestUpdateWeightJSONBodyWinsOverQuery(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight?weight=42",
		map[string]any{"weight": 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["weight"].(float64) != 7 {
		t.Fatalf("weight = %v, want 7 (body wins)", instance["weight"])
	}
}

func TestUpdateWeightValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())

	cases := []struct {
		name    string
		path    string
		rawBody string
		body    any
		query   bool
	}{
		{name: "missing weight", path: "/api/v1/services/svc/instances/i-1/weight", body: map[string]any{}},
		{name: "null weight", path: "/api/v1/services/svc/instances/i-1/weight", body: map[string]any{"weight": nil}},
		{name: "zero weight", path: "/api/v1/services/svc/instances/i-1/weight", body: map[string]any{"weight": 0}},
		{name: "negative weight", path: "/api/v1/services/svc/instances/i-1/weight", body: map[string]any{"weight": -2}},
		{name: "unparseable weight", path: "/api/v1/services/svc/instances/i-1/weight", body: map[string]any{"weight": "heavy"}},
		{name: "boolean weight", path: "/api/v1/services/svc/instances/i-1/weight", body: map[string]any{"weight": true}},
		{name: "query zero weight", path: "/api/v1/services/svc/instances/i-1/weight?weight=0", query: true},
		{name: "query inf weight", path: "/api/v1/services/svc/instances/i-1/weight?weight=Infinity", query: true},
		{name: "empty body", path: "/api/v1/services/svc/instances/i-1/weight", body: nil},
		{name: "body not an object", path: "/api/v1/services/svc/instances/i-1/weight", rawBody: "[1,2,3]"},
		{name: "body malformed json", path: "/api/v1/services/svc/instances/i-1/weight", rawBody: "{bad"},
		{name: "empty service name", path: "/api/v1/services//instances/i-1/weight", body: map[string]any{"weight": 5}},
		{name: "empty instance id", path: "/api/v1/services/svc/instances//weight", body: map[string]any{"weight": 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			switch {
			case tc.rawBody != "":
				rec = doRawRequest(t, router, http.MethodPut, tc.path, tc.rawBody)
			case tc.query || tc.body != nil:
				rec = doRequest(t, router, http.MethodPut, tc.path, tc.body)
			default:
				rec = doRequest(t, router, http.MethodPut, tc.path, nil)
			}
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertWeightUnchanged(t, st)
		})
	}

}

func TestUpdateWeightMissingInstanceReturns404(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/gone/weight",
		map[string]any{"weight": 5})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
	assertWeightUnchanged(t, st)
}

func TestBatchUpdateWeightAppliesInRequestOrder(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())
	second := weightRecord()
	second["instance_id"] = "i-2"
	second["weight"] = 3
	registerInstance(t, router, second)
	third := weightRecord()
	third["instance_id"] = "i-3"
	third["healthy"] = false
	registerInstance(t, router, third)
	other := weightRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)

	body := map[string]any{
		"updates": []any{
			map[string]any{"instance_id": "i-3", "weight": "9.5"},
			map[string]any{"instance_id": "i-1", "weight": 100},
		},
	}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("updated = %v, want 2", out["updated"])
	}
	entries := out["instances"].([]any)
	if len(entries) != 2 {
		t.Fatalf("instances len = %d", len(entries))
	}
	first := entries[0].(map[string]any)
	secondEntry := entries[1].(map[string]any)
	if first["instance_id"] != "i-3" || secondEntry["instance_id"] != "i-1" {
		t.Fatalf("order not preserved: %v %v", first["instance_id"], secondEntry["instance_id"])
	}
	if first["weight"].(float64) != 9.5 || first["healthy"] != false ||
		first["address"] != "10.0.0.8:8080" || first["port"].(float64) != 8080 {
		t.Fatalf("batch changed non-weight fields: %v", first)
	}
	if first["heartbeat_at"] != baseTime {
		t.Fatalf("i-3 heartbeat changed: %v", first["heartbeat_at"])
	}
	if secondEntry["weight"].(float64) != 100 {
		t.Fatalf("i-1 weight = %v", secondEntry["weight"])
	}

	got2, _, _ := st.GetInstance("svc", "i-2")
	if got2.Weight != 3 {
		t.Fatalf("i-2 weight changed: %+v", got2)
	}
	peer, _, _ := st.GetInstance("other", "i-1")
	if peer.Weight != 10 {
		t.Fatalf("other service record touched: %+v", peer)
	}
}

func TestBatchUpdateWeightValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())
	second := weightRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	cases := []struct {
		name    string
		path    string
		rawBody string
		body    any
	}{
		{name: "missing updates", body: map[string]any{}},
		{name: "empty updates", body: map[string]any{"updates": []any{}}},
		{name: "updates not a list", body: map[string]any{"updates": "i-1"}},
		{name: "entry not an object", body: map[string]any{"updates": []any{"i-1"}}},
		{name: "entry missing instance id", body: map[string]any{"updates": []any{
			map[string]any{"weight": 5}}}},
		{name: "entry blank instance id", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "  ", "weight": 5}}}},
		{name: "duplicate instance id", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 5},
			map[string]any{"instance_id": "i-1", "weight": 6}}}},
		{name: "missing weight", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1"}}}},
		{name: "zero weight", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 0}}}},
		{name: "unparseable weight", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": "nope"}}}},
		{name: "body not an object", rawBody: "[]"},
		{name: "body malformed json", rawBody: "{not json"},
		{name: "empty service name", path: "/api/v1/services//instances/weight", body: map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 5}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/api/v1/services/svc/instances/weight"
			}
			var rec *httptest.ResponseRecorder
			if tc.rawBody != "" {
				rec = doRawRequest(t, router, http.MethodPut, path, tc.rawBody)
			} else {
				rec = doRequest(t, router, http.MethodPut, path, tc.body)
			}
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertWeightUnchanged(t, st)
			got2, found, _ := st.GetInstance("svc", "i-2")
			if !found || got2.Weight != 10 {
				t.Fatalf("i-2 changed after invalid batch: %+v", got2)
			}
		})
	}
}

func TestBatchUpdateWeightMissingInstanceRollsBack(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())
	second := weightRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	body := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-1", "weight": 50},
		map[string]any{"instance_id": "ghost", "weight": 60},
		map[string]any{"instance_id": "i-2", "weight": 70},
	}}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight", body)
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	for _, id := range []string{"i-1", "i-2"} {
		got, found, _ := st.GetInstance("svc", id)
		if !found || got.Weight != 10 {
			t.Fatalf("%s changed after partial miss: %+v", id, got)
		}
	}
}

func TestUpdateWeightStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	registerInstance(t, router, weightRecord())
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight",
		map[string]any{"weight": 5})
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")

	batchBody := map[string]any{"updates": []any{
		map[string]any{"instance_id": "i-1", "weight": 5},
	}}
	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight", batchBody)
	expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
}

func TestWeightUpdateAffectsDiscoveryOrder(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())
	second := weightRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-2/weight",
		map[string]any{"weight": 50})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances", nil))
	ids := instanceIDs(t, out)
	if len(ids) != 2 || ids[0] != "i-2" || ids[1] != "i-1" {
		t.Fatalf("order after weight update = %v, want [i-2 i-1]", ids)
	}

	weightOut := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-2/weight", nil))
	if weightOut["weight"].(float64) != 50 {
		t.Fatalf("weight query = %v", weightOut["weight"])
	}
}
