package api

import (
	"net/http"
	"testing"
)

func weightRecord(instanceID string, weight any) map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  instanceID,
		"address":      "10.0.0.8:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       weight,
		"heartbeat_at": baseTime,
	}
}

func TestUpdateWeightChangesOnlyWeight(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight",
		map[string]any{"weight": 25.5})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "svc" || instance["instance_id"] != "i-1" ||
		instance["address"] != "10.0.0.8:8080" || instance["port"].(float64) != 8080 ||
		instance["healthy"] != true || instance["weight"].(float64) != 25.5 ||
		instance["heartbeat_at"] != baseTime {
		t.Fatalf("fields changed by weight update: %v", instance)
	}

	got, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.Weight != 25.5 || got.Address != "10.0.0.8:8080" || got.Port != 8080 ||
		!got.Healthy || got.HeartbeatAt.Format("2006-01-02T15:04:05Z") != baseTime {
		t.Fatalf("stored record changed outside weight: %+v", got)
	}
}

func TestUpdateWeightAcceptsNumericStringAndQueryParameter(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight?weight=3",
		map[string]any{"weight": "12.5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["weight"].(float64) != 12.5 {
		t.Fatalf("weight = %v, JSON body must win over query parameter", instance["weight"])
	}

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight?weight=4", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	instance = decodeBody(t, rec)["instance"].(map[string]any)
	if instance["weight"].(float64) != 4 {
		t.Fatalf("weight = %v, want query parameter 4", instance["weight"])
	}
}

func TestUpdateWeightMissingInstanceReturns404(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/gone/weight",
		map[string]any{"weight": 2})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Weight != 10 {
		t.Fatalf("existing record changed after 404: %+v", got)
	}
}

func TestUpdateWeightValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))

	cases := []struct {
		name   string
		target string
		body   any
	}{
		{"missing weight", "/api/v1/services/svc/instances/i-1/weight", map[string]any{}},
		{"zero weight", "/api/v1/services/svc/instances/i-1/weight", map[string]any{"weight": 0}},
		{"negative weight", "/api/v1/services/svc/instances/i-1/weight", map[string]any{"weight": -1}},
		{"unparseable weight", "/api/v1/services/svc/instances/i-1/weight", map[string]any{"weight": "heavy"}},
		{"nan weight", "/api/v1/services/svc/instances/i-1/weight", map[string]any{"weight": "NaN"}},
		{"empty service", "/api/v1/services//instances/i-1/weight", map[string]any{"weight": 1}},
		{"empty instance", "/api/v1/services/svc/instances//weight", map[string]any{"weight": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPut, tc.target, tc.body)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			got, found, _ := st.GetInstance("svc", "i-1")
			if !found || got.Weight != 10 {
				t.Fatalf("record changed after rejected request: %+v", got)
			}
		})
	}
}

func TestBatchUpdateWeightAppliesInRequestOrder(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))
	registerInstance(t, router, weightRecord("i-2", 3))

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight",
		map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-2", "weight": "7.5"},
			map[string]any{"instance_id": "i-1", "weight": 1},
		}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("updated = %v, want 2", out["updated"])
	}
	instances := out["instances"].([]any)
	first := instances[0].(map[string]any)
	second := instances[1].(map[string]any)
	if first["instance_id"] != "i-2" || first["weight"].(float64) != 7.5 ||
		first["address"] != "10.0.0.8:8080" || first["port"].(float64) != 8080 ||
		first["healthy"] != true || first["heartbeat_at"] != baseTime {
		t.Fatalf("first record wrong: %v", first)
	}
	if second["instance_id"] != "i-1" || second["weight"].(float64) != 1 {
		t.Fatalf("second record wrong: %v", second)
	}

	got, _, _ := st.GetInstance("svc", "i-2")
	if got.Weight != 7.5 || got.Address != "10.0.0.8:8080" || got.Port != 8080 ||
		!got.Healthy || got.HeartbeatAt.Format("2006-01-02T15:04:05Z") != baseTime {
		t.Fatalf("non-weight fields changed: %+v", got)
	}
}

func TestBatchUpdateWeightValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))
	registerInstance(t, router, weightRecord("i-2", 3))

	cases := []struct {
		name string
		body any
	}{
		{"updates missing", map[string]any{}},
		{"updates empty", map[string]any{"updates": []any{}}},
		{"updates not a list", map[string]any{"updates": "i-1"}},
		{"entry not an object", map[string]any{"updates": []any{"i-1"}}},
		{"missing instance_id", map[string]any{"updates": []any{
			map[string]any{"weight": 1}}}},
		{"empty instance_id", map[string]any{"updates": []any{
			map[string]any{"instance_id": "  ", "weight": 1}}}},
		{"duplicate instance_id", map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 2},
			map[string]any{"instance_id": "i-1", "weight": 3}}}},
		{"missing weight", map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1"}}}},
		{"non-positive weight", map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 0}}}},
		{"non-finite weight", map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": "Infinity"}}}},
		{"body is a list", []any{map[string]any{"instance_id": "i-1", "weight": 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/weight", tc.body)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			got, _, _ := st.GetInstance("svc", "i-1")
			if got.Weight != 10 {
				t.Fatalf("i-1 changed after rejected batch: %+v", got)
			}
			if peer, _, _ := st.GetInstance("svc", "i-2"); peer.Weight != 3 {
				t.Fatalf("i-2 changed after rejected batch: %+v", peer)
			}
		})
	}
}

func TestBatchUpdateWeightMissingInstanceIsAtomic404(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight",
		map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 2},
			map[string]any{"instance_id": "missing", "weight": 4},
		}})
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")

	got, found, _ := st.GetInstance("svc", "i-1")
	if !found || got.Weight != 10 {
		t.Fatalf("batch left a partial update: %+v", got)
	}
}

func TestBatchUpdateWeightEmptyServiceReturns400(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord("i-1", 10))

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services//instances/weight",
		map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 2}}})
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
}

func TestWeightUpdateRoutesDoNotShadowExistingGetOrPut(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord("weight", 10))

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/weight/weight", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET weight query for instance 'weight': status %d body %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight",
		map[string]any{"updates": []any{
			map[string]any{"instance_id": "weight", "weight": 6}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch weight endpoint status = %d body %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["updated"].(float64) != 1 {
		t.Fatalf("batch endpoint did not handle PUT to /instances/weight: %s", rec.Body.String())
	}
}
