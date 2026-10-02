package api

import (
	"net/http"
	"testing"
)

func TestWeightEmptySegmentsReturnParameterError(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, "/api/v1/services//instances/weight", map[string]any{"updates": []any{
			map[string]any{"instance_id": "i-1", "weight": 5}}}},
		{http.MethodPut, "/api/v1/services//instances/i-1/weight", map[string]any{"weight": 5}},
		{http.MethodPut, "/api/v1/services/svc/instances//weight", map[string]any{"weight": 5}},
		{http.MethodGet, "/api/v1/services//instances/i-1/weight", nil},
		{http.MethodGet, "/api/v1/services/svc/instances//weight", nil},
	}
	for _, tc := range cases {
		rec := doRequest(t, router, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s -> %d body %s, want 400", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
