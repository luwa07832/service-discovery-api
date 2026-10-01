package api

import (
	"net/http"
	"testing"
	"time"
)

func TestRenewHeartbeatUpdatesOnlyTimestamp(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.1:9000",
		"port": 9000, "healthy": true, "weight": 7, "heartbeat_at": at(0),
	})

	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": at(30)})
	if rec.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	instance := out["instance"].(map[string]any)
	if instance["heartbeat_at"] != at(30) {
		t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], at(30))
	}
	if instance["address"] != "10.0.0.1:9000" || instance["port"].(float64) != 9000 ||
		instance["healthy"] != true || instance["weight"].(float64) != 7 {
		t.Fatalf("renew changed other fields: %v", instance)
	}

	stored, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("record missing: found=%v err=%v", found, err)
	}
	wantHeartbeat, _ := time.Parse(time.RFC3339, at(30))
	if !stored.HeartbeatAt.Equal(wantHeartbeat) || stored.Address != "10.0.0.1:9000" ||
		stored.Port != 9000 || !stored.Healthy || stored.Weight != 7 {
		t.Fatalf("stored record mismatch: %+v", stored)
	}
}

func TestRenewHeartbeatAcceptsUnixSeconds(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	seconds := int64(1759312800)
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": seconds})
	if rec.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	instance := out["instance"].(map[string]any)
	want := time.Unix(seconds, 0).UTC().Format(time.RFC3339Nano)
	if instance["heartbeat_at"] != want {
		t.Fatalf("heartbeat_at = %v, want %s", instance["heartbeat_at"], want)
	}
}

func TestRenewHeartbeatRejectsBadInput(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	// Missing or unparseable heartbeat_at is a parameter error.
	for _, body := range []map[string]any{
		{},
		{"heartbeat_at": ""},
		{"heartbeat_at": "not-a-time"},
	} {
		rec := doRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat", body)
		expectParameterError(t, rec)
	}

	// Unknown instances are a not-found error.
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/ghost/heartbeat",
		map[string]any{"heartbeat_at": at(5)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing instance status = %d, want 404", rec.Code)
	}
	out := decodeBody(t, rec)
	if out["error"].(map[string]any)["code"] != "instance_not_found" {
		t.Fatalf("code = %v, want instance_not_found", out["error"])
	}

	// Rejected requests changed nothing.
	stored, found, err := st.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("record missing: found=%v err=%v", found, err)
	}
	wantHeartbeat, _ := time.Parse(time.RFC3339, at(0))
	if !stored.HeartbeatAt.Equal(wantHeartbeat) {
		t.Fatalf("rejected renewals changed record: %+v", stored)
	}
}

func TestBatchRenewHeartbeatsUpdatesInRequestOrder(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "a",
		"port": 1, "healthy": true, "weight": 5, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-2", "address": "b",
		"port": 2, "healthy": false, "weight": 9, "heartbeat_at": at(1),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "other", "instance_id": "i-9", "healthy": true,
		"weight": 3, "heartbeat_at": at(2),
	})

	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "svc",
		"instances": []map[string]any{
			{"instance_id": "i-2", "heartbeat_at": at(20)},
			{"instance_id": "i-1", "heartbeat_at": at(25)},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch renew: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["updated"].(float64) != 2 {
		t.Fatalf("updated = %v, want 2", out["updated"])
	}
	items := out["instances"].([]any)
	if len(items) != 2 {
		t.Fatalf("instances len = %d, want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["instance_id"] != "i-2" || first["heartbeat_at"] != at(20) {
		t.Fatalf("first item = %v", first)
	}
	if first["healthy"] != false || first["weight"].(float64) != 9 ||
		first["address"] != "b" || first["port"].(float64) != 2 {
		t.Fatalf("batch renew changed other fields: %v", first)
	}
	second := items[1].(map[string]any)
	if second["instance_id"] != "i-1" || second["heartbeat_at"] != at(25) {
		t.Fatalf("second item = %v", second)
	}

	// Stored records carry the new heartbeat and nothing else changed.
	stored, _, _ := st.GetInstance("svc", "i-2")
	wantHeartbeat, _ := time.Parse(time.RFC3339, at(20))
	if !stored.HeartbeatAt.Equal(wantHeartbeat) || stored.Healthy || stored.Weight != 9 {
		t.Fatalf("stored i-2 mismatch: %+v", stored)
	}
	// Instances of other services are untouched.
	peer, found, _ := st.GetInstance("other", "i-9")
	if !found {
		t.Fatalf("other service instance missing")
	}
	wantPeer, _ := time.Parse(time.RFC3339, at(2))
	if !peer.HeartbeatAt.Equal(wantPeer) {
		t.Fatalf("other service record changed: %+v", peer)
	}
}

func TestBatchRenewValidationChangesNothing(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	cases := []map[string]any{
		{},
		{"service_name": "svc"},
		{"service_name": "svc", "instances": []any{}},
		{"service_name": "  ", "instances": []map[string]any{
			{"instance_id": "i-1", "heartbeat_at": at(5)}}},
		{"service_name": "svc", "instances": []map[string]any{
			{"heartbeat_at": at(5)}}},
		{"service_name": "svc", "instances": []map[string]any{
			{"instance_id": " ", "heartbeat_at": at(5)}}},
		{"service_name": "svc", "instances": []map[string]any{
			{"instance_id": "i-1"}}},
		{"service_name": "svc", "instances": []map[string]any{
			{"instance_id": "i-1", "heartbeat_at": "junk"}}},
		{"service_name": "svc", "instances": []map[string]any{
			{"instance_id": "i-1", "heartbeat_at": at(5)},
			{"instance_id": "i-1", "heartbeat_at": at(6)}}},
		{"service_name": "svc", "instances": []any{"not-an-object"}},
	}
	for index, body := range cases {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", body)
		expectParameterError(t, rec)
		stored, found, err := st.GetInstance("svc", "i-1")
		if err != nil || !found {
			t.Fatalf("case %d: record missing: found=%v err=%v", index, found, err)
		}
		wantHeartbeat, _ := time.Parse(time.RFC3339, at(0))
		if !stored.HeartbeatAt.Equal(wantHeartbeat) {
			t.Fatalf("case %d changed record: %+v", index, stored)
		}
	}
}

func TestBatchRenewMissingInstanceIsAtomic(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-2", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})

	rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", map[string]any{
		"service_name": "svc",
		"instances": []map[string]any{
			{"instance_id": "i-1", "heartbeat_at": at(5)},
			{"instance_id": "ghost", "heartbeat_at": at(6)},
		},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["error"].(map[string]any)["code"] != "instance_not_found" {
		t.Fatalf("code = %v, want instance_not_found", out["error"])
	}

	// The valid entry in the same batch was not applied either.
	for _, id := range []string{"i-1", "i-2"} {
		stored, found, err := st.GetInstance("svc", id)
		if err != nil || !found {
			t.Fatalf("%s missing: found=%v err=%v", id, found, err)
		}
		wantHeartbeat, _ := time.Parse(time.RFC3339, at(0))
		if !stored.HeartbeatAt.Equal(wantHeartbeat) {
			t.Fatalf("partial update leaked into %s: %+v", id, stored)
		}
	}
}

func TestRenewHeartbeatKeepsHealthAndDiscoverySemantics(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "sick", "healthy": false,
		"weight": 1, "heartbeat_at": at(0),
	})

	// Renewing the heartbeat does not flip the stored health state, so the
	// unhealthy instance stays out of discovery hits and is not cleaned up.
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/sick/heartbeat",
		map[string]any{"heartbeat_at": at(9)})
	if rec.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=5m", nil))
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("unhealthy instance discovered after renew: %v", ids)
	}
	stored, found, _ := st.GetInstance("svc", "sick")
	if !found || stored.Healthy {
		t.Fatalf("renew changed health state: %+v", stored)
	}

	// A renewed healthy instance is discoverable again at its new heartbeat.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "fit", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/fit/heartbeat",
		map[string]any{"heartbeat_at": at(9)})
	if rec.Code != http.StatusOK {
		t.Fatalf("renew fit: %d %s", rec.Code, rec.Body.String())
	}
	out = decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=5m", nil))
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "fit" {
		t.Fatalf("ids = %v, want [fit]", ids)
	}
}
