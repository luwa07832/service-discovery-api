package api

import (
	"net/http"
	"testing"
)

func TestHeartbeatRefreshesKnownInstance(t *testing.T) {
	router, _ := lifecycleRouter(t)
	register(t, router, goodRegistration())

	body := map[string]any{
		"service_name": "svc", "instance_id": "i-1",
		"healthy": false, "weight": 3,
		"heartbeat_at": lifecycleAt(5), "request_at": lifecycleAt(5),
	}
	for _, target := range []string{
		"/api/v1/services/svc/instances/i-1/heartbeat",
		"/api/v1/heartbeat",
	} {
		rec := lifecycleRequest(t, router, http.MethodPost, target, body)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		out := decodeLifecycleBody(t, rec)
		if out["updated"] != true {
			t.Fatalf("%s updated flag: %v", target, out)
		}
		instance := out["instance"].(map[string]any)
		if instance["healthy"] != false || instance["weight"].(float64) != 3 || instance["heartbeat_at"] != lifecycleAt(5) {
			t.Fatalf("%s refreshed record mismatch: %v", target, instance)
		}
	}

	rec := lifecycleRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances/i-1", nil)
	instance := decodeLifecycleBody(t, rec)["instance"].(map[string]any)
	if instance["healthy"] != false || instance["weight"].(float64) != 3 || instance["heartbeat_at"] != lifecycleAt(5) {
		t.Fatalf("query after heartbeat mismatch: %v", instance)
	}
}

func TestHeartbeatKeepsUnspecifiedHealthAndWeight(t *testing.T) {
	router, _ := lifecycleRouter(t)
	register(t, router, goodRegistration())
	body := map[string]any{
		"service_name": "svc", "instance_id": "i-1",
		"heartbeat_at": lifecycleAt(6), "request_at": lifecycleAt(6),
	}
	out := decodeLifecycleBody(t, lifecycleRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body))
	instance := out["instance"].(map[string]any)
	if instance["healthy"] != true || instance["weight"].(float64) != 7 {
		t.Fatalf("omitted fields should keep the current values: %v", instance)
	}
	if instance["heartbeat_at"] != lifecycleAt(6) {
		t.Fatalf("heartbeat time should refresh: %v", instance)
	}
}

func TestHeartbeatMissingAtDefaultsToRequestTime(t *testing.T) {
	router, _ := lifecycleRouter(t)
	register(t, router, goodRegistration())
	out := decodeLifecycleBody(t, lifecycleRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "svc", "instance_id": "i-1",
		"healthy": true, "weight": 7, "request_at": lifecycleAt(8),
	}))
	instance := out["instance"].(map[string]any)
	if instance["heartbeat_at"] != lifecycleAt(8) {
		t.Fatalf("heartbeat should default to request time: %v", instance)
	}
}

func TestHeartbeatUnknownInstanceIsFalseAndCreatesNothing(t *testing.T) {
	router, st := lifecycleRouter(t)
	body := map[string]any{
		"service_name": "svc", "instance_id": "ghost",
		"healthy": true, "weight": 1,
		"heartbeat_at": lifecycleAt(1), "request_at": lifecycleAt(1),
	}
	rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	if rec.Code != 200 {
		t.Fatalf("unknown heartbeat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeLifecycleBody(t, rec)
	if out["updated"] != false {
		t.Fatalf("unknown heartbeat should report updated=false: %v", out)
	}
	if _, exists := out["instance"]; exists {
		t.Fatalf("unknown heartbeat must not return a record: %v", out)
	}
	if list, _ := st.ListInstances("svc"); len(list) != 0 {
		t.Fatalf("unknown heartbeat created a record: %d rows", len(list))
	}
}

func TestHeartbeatValidationCreatesNothing(t *testing.T) {
	router, st := lifecycleRouter(t)
	register(t, router, goodRegistration())
	base := map[string]any{
		"service_name": "svc", "instance_id": "i-1",
		"healthy": true, "weight": 1,
		"heartbeat_at": lifecycleAt(1), "request_at": lifecycleAt(1),
	}
	cases := []map[string]any{
		without(base, "service_name"),
		without(base, "instance_id"),
		without(base, "request_at"),
		{"service_name": "svc", "instance_id": "i-1", "weight": 1, "request_at": lifecycleAt(1), "healthy": "maybe"},
		{"service_name": "svc", "instance_id": "i-1", "weight": -2, "request_at": lifecycleAt(1)},
		{"service_name": "svc", "instance_id": "i-1", "weight": "nope", "request_at": lifecycleAt(1)},
		{"service_name": "svc", "instance_id": "i-1", "weight": 1, "request_at": lifecycleAt(1), "heartbeat_at": "later"},
		{"service_name": "svc", "instance_id": "i-1", "weight": 1,
			"request_at": lifecycleAt(1), "heartbeat_at": lifecycleAt(2)},
	}
	for index, body := range cases {
		rec := lifecycleRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
		expectInvalidParameter(t, rec)
		if list, _ := st.ListInstances("svc"); len(list) != 1 {
			t.Fatalf("case %d changed storage: %d rows", index, len(list))
		}
	}
	got, _, _ := st.GetInstance("svc", "i-1")
	if !got.Healthy || got.Weight != 7 || got.HeartbeatAt.Format("2006-01-02T15:04:05Z") != lifecycleBaseTime {
		t.Fatalf("invalid heartbeats changed the record: %+v", got)
	}
}

func TestHeartbeatIdenticalRepeatIsDeterministic(t *testing.T) {
	router, st := lifecycleRouter(t)
	register(t, router, goodRegistration())
	body := map[string]any{
		"service_name": "svc", "instance_id": "i-1",
		"healthy": false, "weight": 4,
		"heartbeat_at": lifecycleAt(3), "request_at": lifecycleAt(3),
	}
	lifecycleRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	lifecycleRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
	list, _ := st.ListInstances("svc")
	if len(list) != 1 {
		t.Fatalf("identical repeat created records: %d rows", len(list))
	}
	if list[0].Healthy || list[0].Weight != 4 || list[0].HeartbeatAt.Format("2006-01-02T15:04:05Z") != lifecycleAt(3) {
		t.Fatalf("identical repeat changed content: %+v", list[0])
	}
}
