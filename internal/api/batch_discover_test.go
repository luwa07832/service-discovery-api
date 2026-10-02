package api

import (
	"net/http"
	"testing"
	"time"
)

// seedBatchDiscoverInstances registers two requested services with lost,
// boundary, unhealthy-fresh and online records, plus one service that stays
// outside every request.
func seedBatchDiscoverInstances(t *testing.T, router http.Handler) {
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
		"weight": 20, "heartbeat_at": at(8),
	})
	// beta/i-1 is strictly lost; beta/i-2 and beta/i-3 share one weight so
	// the instance id decides their order.
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-1", "healthy": true,
		"weight": 7, "heartbeat_at": at(1),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-3", "healthy": true,
		"weight": 9, "heartbeat_at": at(9),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-2", "healthy": true,
		"weight": 9, "heartbeat_at": at(9),
	})
	// delta/i-1 would be lost but the service is never requested.
	registerInstance(t, router, map[string]any{
		"service_name": "delta", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
}

func batchDiscoverRequest(t *testing.T, router http.Handler, body any) (int, map[string]any) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, "/api/v1/discover/batch", body)
	return rec.Code, decodeBody(t, rec)
}

func serviceEntries(t *testing.T, out map[string]any) []any {
	t.Helper()
	rawList, ok := out["services"].([]any)
	if !ok {
		t.Fatalf("services is not a list: %v", out)
	}
	return rawList
}

func entryInstances(t *testing.T, entry any) (string, []string) {
	t.Helper()
	item := entry.(map[string]any)
	rawList, ok := item["instances"].([]any)
	if !ok {
		t.Fatalf("instances is not a list: %v", item)
	}
	ids := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		ids = append(ids, raw.(map[string]any)["instance_id"].(string))
	}
	return item["service_name"].(string), ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestBatchDiscoverSharesEvaluationAndCleansAtomically(t *testing.T) {
	router, st := testRouter(t)
	seedBatchDiscoverInstances(t, router)

	code, out := batchDiscoverRequest(t, router, map[string]any{
		"service_names":     []string{"alpha", "beta", "gamma"},
		"evaluate_at":       at(10),
		"heartbeat_timeout": "5m",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %v", code, out)
	}
	if out["evaluate_at"] != at(10) {
		t.Fatalf("evaluate_at = %v, want %s", out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"] != float64(300) {
		t.Fatalf("heartbeat_timeout = %v, want 300 seconds", out["heartbeat_timeout"])
	}

	entries := serviceEntries(t, out)
	if len(entries) != 3 {
		t.Fatalf("services = %v, want three entries", entries)
	}
	name, ids := entryInstances(t, entries[0])
	if name != "alpha" || !equalStrings(ids, []string{"i-2"}) {
		t.Fatalf("alpha entry = %v %v, want only i-2 on the boundary", name, ids)
	}
	name, ids = entryInstances(t, entries[1])
	if name != "beta" || !equalStrings(ids, []string{"i-2", "i-3"}) {
		t.Fatalf("beta entry = %v %v, want i-2 then i-3 by weight then id", name, ids)
	}
	name, ids = entryInstances(t, entries[2])
	if name != "gamma" || len(ids) != 0 {
		t.Fatalf("gamma entry = %v %v, want an empty instance list", name, ids)
	}

	// Lost records of every requested service are gone; fresh records and
	// services outside the request are untouched.
	for _, key := range [][2]string{{"alpha", "i-1"}, {"beta", "i-1"}} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || found {
			t.Fatalf("%s/%s should be deleted: found=%v err=%v", key[0], key[1], found, err)
		}
	}
	for _, key := range [][2]string{
		{"alpha", "i-2"}, {"alpha", "i-3"}, {"beta", "i-2"}, {"beta", "i-3"}, {"delta", "i-1"},
	} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || !found {
			t.Fatalf("%s/%s should be kept: found=%v err=%v", key[0], key[1], found, err)
		}
	}
}

func TestBatchDiscoverKeepsRequestOrderAndNormalizes(t *testing.T) {
	router, _ := testRouter(t)
	seedBatchDiscoverInstances(t, router)

	evaluateAt := time.Date(2026, 10, 1, 12, 10, 0, 0, time.UTC).Unix()
	code, out := batchDiscoverRequest(t, router, map[string]any{
		"service_names":     []string{" beta ", "alpha"},
		"evaluate_at":       evaluateAt,
		"heartbeat_timeout": 300,
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %v", code, out)
	}
	if out["evaluate_at"] != at(10) {
		t.Fatalf("evaluate_at = %v, want normalized %s", out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"] != float64(300) {
		t.Fatalf("heartbeat_timeout = %v, want 300 seconds", out["heartbeat_timeout"])
	}
	entries := serviceEntries(t, out)
	first, _ := entryInstances(t, entries[0])
	second, _ := entryInstances(t, entries[1])
	if first != "beta" || second != "alpha" {
		t.Fatalf("service order = %v, %v, want request order beta, alpha", first, second)
	}
}

func TestBatchDiscoverRejectsInvalidParameters(t *testing.T) {
	router, st := testRouter(t)
	seedBatchDiscoverInstances(t, router)

	bodies := []any{
		// The body itself is not a single JSON object.
		[]any{map[string]any{"service_names": []string{"alpha"}}},
		// service_names missing, empty, blank, duplicated or not strings.
		map[string]any{"evaluate_at": at(10), "heartbeat_timeout": 300},
		map[string]any{"service_names": []string{}, "evaluate_at": at(10), "heartbeat_timeout": 300},
		map[string]any{"service_names": []string{"alpha", "  "}, "evaluate_at": at(10), "heartbeat_timeout": 300},
		map[string]any{"service_names": []string{"alpha", " alpha "}, "evaluate_at": at(10), "heartbeat_timeout": 300},
		map[string]any{"service_names": "alpha", "evaluate_at": at(10), "heartbeat_timeout": 300},
		map[string]any{"service_names": []any{"alpha", 7}, "evaluate_at": at(10), "heartbeat_timeout": 300},
		// evaluate_at missing or unparsable.
		map[string]any{"service_names": []string{"alpha"}, "heartbeat_timeout": 300},
		map[string]any{"service_names": []string{"alpha"}, "evaluate_at": "soon", "heartbeat_timeout": 300},
		// heartbeat_timeout missing, zero, negative or unparsable.
		map[string]any{"service_names": []string{"alpha"}, "evaluate_at": at(10)},
		map[string]any{"service_names": []string{"alpha"}, "evaluate_at": at(10), "heartbeat_timeout": 0},
		map[string]any{"service_names": []string{"alpha"}, "evaluate_at": at(10), "heartbeat_timeout": "-5s"},
		map[string]any{"service_names": []string{"alpha"}, "evaluate_at": at(10), "heartbeat_timeout": "later"},
		// evaluate_at earlier than a heartbeat of a requested service.
		map[string]any{"service_names": []string{"alpha"}, "evaluate_at": at(4), "heartbeat_timeout": 300},
		map[string]any{"service_names": []string{"gamma", "beta"}, "evaluate_at": at(8), "heartbeat_timeout": 300},
	}
	for index, body := range bodies {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/discover/batch", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("case %d: status = %d, want 400, body %s", index, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		errObj, ok := out["error"].(map[string]any)
		if !ok || errObj["code"] != "invalid_parameter" || len(out) != 1 {
			t.Fatalf("case %d: error body = %v, want a single invalid_parameter error", index, out)
		}
	}

	// No rejected request may create, update or delete any record.
	for _, key := range [][2]string{
		{"alpha", "i-1"}, {"alpha", "i-2"}, {"alpha", "i-3"},
		{"beta", "i-1"}, {"beta", "i-2"}, {"beta", "i-3"}, {"delta", "i-1"},
	} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || !found {
			t.Fatalf("%s/%s should be kept after rejected requests: found=%v err=%v",
				key[0], key[1], found, err)
		}
	}
}

func TestBatchDiscoverEmptyBodyIsRejected(t *testing.T) {
	router, _ := testRouter(t)
	rec := doRequest(t, router, http.MethodPost, "/api/v1/discover/batch", nil)
	expectParameterError(t, rec)
}
