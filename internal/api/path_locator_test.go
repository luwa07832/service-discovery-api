package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// seedTwoServices registers one healthy fresh instance in each of two
// services plus one strictly lost instance in each service. Path locator
// tests prove that conflicting body/query fields can never move an operation
// from the path target (alpha) to another existing target (beta).
func seedTwoServices(t *testing.T, router http.Handler) {
	t.Helper()
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "i-1", "address": "alpha-1",
		"port": 8001, "healthy": true, "weight": 10, "heartbeat_at": at(12),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "alpha", "instance_id": "alpha-lost", "address": "alpha-old",
		"healthy": true, "weight": 4, "heartbeat_at": at(-10),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "i-2", "address": "beta-2",
		"port": 8002, "healthy": true, "weight": 20, "heartbeat_at": at(12),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "beta", "instance_id": "beta-lost", "address": "beta-old",
		"healthy": true, "weight": 3, "heartbeat_at": at(-10),
	})
}

func seedTwoServicesStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	router, st := testRouter(t)
	seedTwoServices(t, router)
	return router, st
}

// conflictQuery carries a locator that points at beta through every accepted
// synonym, so path handlers must ignore all of them.
const conflictQuery = "?service_name=beta&serviceName=beta&service=beta" +
	"&instance_id=i-2&instanceId=i-2&instance=i-2&id=i-2"

func conflictBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"service_name": "beta", "serviceName": "beta", "service": "beta",
		"instance_id": "i-2", "instanceId": "i-2", "instance": "i-2", "id": "i-2",
	}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func expectSingleErrorObject(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	expectErrorShape(t, rec, status, code)
	if out := decodeBody(t, rec); len(out) != 1 {
		t.Fatalf("response must be one error object only: %v", out)
	}
}

// assertFixtureIntact verifies a rejected request created, updated or
// deleted nothing: all four seeded records remain with their seeded values.
func assertFixtureIntact(t *testing.T, st *store.Store) {
	t.Helper()
	if all, err := st.ListAllInstances(); err != nil || len(all) != 4 {
		t.Fatalf("storage changed after failed request: %d records err=%v", len(all), err)
	}
	alpha, found, err := st.GetInstance("alpha", "i-1")
	if err != nil || !found {
		t.Fatalf("alpha/i-1 missing after failed request: found=%v err=%v", found, err)
	}
	if alpha.Address != "alpha-1" || alpha.Port != 8001 || !alpha.Healthy ||
		alpha.Weight != 10 || !alpha.HeartbeatAt.Equal(mustParseTime(at(12))) {
		t.Fatalf("alpha/i-1 changed after failed request: %+v", alpha)
	}
	for _, key := range []struct {
		service, instance string
	}{
		{"alpha", "alpha-lost"},
		{"beta", "i-2"},
		{"beta", "beta-lost"},
	} {
		if _, found, err := st.GetInstance(key.service, key.instance); err != nil || !found {
			t.Fatalf("%s/%s missing after failed request: found=%v err=%v",
				key.service, key.instance, found, err)
		}
	}
}

// TestPathLocatorWinsOnInstanceReads pins the headline contract: with
// instances in both services, GET alpha/i-1 with conflicting query locators
// still reads alpha's i-1 across all single-instance read entries.
func TestPathLocatorWinsOnInstanceReads(t *testing.T) {
	router, st := seedTwoServicesStore(t)

	targets := []string{
		"/api/v1/services/alpha/instances/i-1" + conflictQuery,
		"/api/v1/services/alpha/instances/i-1/health" + conflictQuery,
		"/api/v1/services/alpha/instances/i-1/weight" + conflictQuery,
		"/api/v1/services/alpha/instances/i-1/heartbeat" + conflictQuery,
	}
	for _, target := range targets {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d %s", target, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		obj := out["instance"]
		if obj == nil {
			obj = out // health/weight/heartbeat entries answer at top level
		}
		got := obj.(map[string]any)
		if got["service_name"] != "alpha" || got["instance_id"] != "i-1" {
			t.Fatalf("GET %s retargeted to %v", target, got)
		}
	}

	// Path reads must not have touched beta at all.
	beta, found, err := st.GetInstance("beta", "i-2")
	if err != nil || !found || beta.Address != "beta-2" || beta.Weight != 20 {
		t.Fatalf("beta record changed after path reads: %+v found=%v err=%v", beta, found, err)
	}
}

// TestParamStyleEntriesStillResolveSynonyms keeps the non-path lookup rules:
// without a path segment the synonym fields resolve the instance.
func TestParamStyleEntriesStillResolveSynonyms(t *testing.T) {
	router, _ := seedTwoServicesStore(t)

	cases := []struct {
		target       string
		wantService  string
		wantInstance string
	}{
		{"/api/v1/instances?service_name=alpha&instance_id=i-1", "alpha", "i-1"},
		{"/api/v1/instances?service=beta&id=i-2", "beta", "i-2"},
		{"/api/v1/instances/health?serviceName=beta&instanceId=i-2", "beta", "i-2"},
		{"/api/v1/instances/weight?service=alpha&instance=i-1", "alpha", "i-1"},
		{"/api/v1/instances/heartbeat?service_name=beta&instance_id=i-2", "beta", "i-2"},
	}
	for _, tc := range cases {
		rec := doRequest(t, router, http.MethodGet, tc.target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d %s", tc.target, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		obj := out["instance"]
		if obj == nil {
			obj = out
		}
		got := obj.(map[string]any)
		if got["service_name"] != tc.wantService || got["instance_id"] != tc.wantInstance {
			t.Fatalf("GET %s resolved %v, want %s/%s", tc.target, got, tc.wantService, tc.wantInstance)
		}
	}
}

// TestPathLocatorWinsOnSingleItemWrites proves overwrite, heartbeat, weight
// and delete operate on the path target only; the conflicting existing
// instance keeps every original value. Collection registration keeps taking
// instance_id from the body while the path supplies only the service.
func TestPathLocatorWinsOnSingleItemWrites(t *testing.T) {
	t.Run("heartbeat", func(t *testing.T) {
		router, st := seedTwoServicesStore(t)
		rec := doRequest(t, router, http.MethodPost,
			"/api/v1/services/alpha/instances/i-1/heartbeat"+conflictQuery,
			conflictBody(map[string]any{"heartbeat_at": at(25)}))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
		}
		updated := decodeBody(t, rec)["instance"].(map[string]any)
		if updated["service_name"] != "alpha" || updated["instance_id"] != "i-1" {
			t.Fatalf("heartbeat retargeted: %v", updated)
		}
		alpha, _, _ := st.GetInstance("alpha", "i-1")
		if !alpha.HeartbeatAt.Equal(mustParseTime(at(25))) {
			t.Fatalf("alpha heartbeat = %v", alpha.HeartbeatAt)
		}
		beta, found, _ := st.GetInstance("beta", "i-2")
		if !found || !beta.HeartbeatAt.Equal(mustParseTime(at(12))) {
			t.Fatalf("beta heartbeat changed: %+v", beta)
		}
	})

	t.Run("weight", func(t *testing.T) {
		router, st := seedTwoServicesStore(t)
		rec := doRequest(t, router, http.MethodPut,
			"/api/v1/services/alpha/instances/i-1/weight"+conflictQuery,
			conflictBody(map[string]any{"weight": 42}))
		if rec.Code != http.StatusOK {
			t.Fatalf("weight: %d %s", rec.Code, rec.Body.String())
		}
		updated := decodeBody(t, rec)["instance"].(map[string]any)
		if updated["service_name"] != "alpha" || updated["instance_id"] != "i-1" ||
			updated["weight"].(float64) != 42 {
			t.Fatalf("weight update retargeted: %v", updated)
		}
		beta, found, _ := st.GetInstance("beta", "i-2")
		if !found || beta.Weight != 20 {
			t.Fatalf("beta weight changed: %+v", beta)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		router, st := seedTwoServicesStore(t)
		rec := doRequest(t, router, http.MethodPut,
			"/api/v1/services/alpha/instances/i-1"+conflictQuery,
			conflictBody(map[string]any{
				"address": "alpha-replaced", "weight": 7, "heartbeat_at": at(25),
			}))
		if rec.Code != http.StatusOK {
			t.Fatalf("overwrite: %d %s", rec.Code, rec.Body.String())
		}
		updated := decodeBody(t, rec)["instance"].(map[string]any)
		if updated["service_name"] != "alpha" || updated["instance_id"] != "i-1" ||
			updated["address"] != "alpha-replaced" {
			t.Fatalf("overwrite retargeted: %v", updated)
		}
		alpha, _, _ := st.GetInstance("alpha", "i-1")
		if alpha.Address != "alpha-replaced" || alpha.Weight != 7 {
			t.Fatalf("alpha not overwritten: %+v", alpha)
		}
		beta, found, _ := st.GetInstance("beta", "i-2")
		if !found || beta.Address != "beta-2" || beta.Weight != 20 {
			t.Fatalf("beta changed by overwrite: %+v", beta)
		}
	})

	t.Run("collection register keeps body instance id", func(t *testing.T) {
		router, st := seedTwoServicesStore(t)
		// The path names only the service; instance_id must still come from
		// the existing body/query rules, and the body's service_name is ignored.
		rec := doRequest(t, router, http.MethodPost,
			"/api/v1/services/alpha/instances?service_name=beta",
			map[string]any{
				"service_name": "beta", "instance_id": "i-9",
				"weight": 7, "heartbeat_at": baseTime,
			})
		if rec.Code != http.StatusOK {
			t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
		}
		created := decodeBody(t, rec)["instance"].(map[string]any)
		if created["service_name"] != "alpha" || created["instance_id"] != "i-9" {
			t.Fatalf("register target = %v, want alpha/i-9", created)
		}
		if _, found, _ := st.GetInstance("alpha", "i-9"); !found {
			t.Fatalf("alpha/i-9 not stored")
		}
		if list, _ := st.ListInstances("beta"); len(list) != 2 {
			t.Fatalf("beta changed by collection register: %d records", len(list))
		}
	})

	t.Run("delete", func(t *testing.T) {
		router, st := seedTwoServicesStore(t)
		rec := doRequest(t, router, http.MethodDelete,
			"/api/v1/services/alpha/instances/i-1"+conflictQuery, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		if out["service_name"] != "alpha" || out["instance_id"] != "i-1" || out["deleted"] != true {
			t.Fatalf("delete response retargeted: %v", out)
		}
		if _, found, _ := st.GetInstance("alpha", "i-1"); found {
			t.Fatalf("alpha/i-1 was not deleted")
		}
		if _, found, _ := st.GetInstance("beta", "i-2"); !found {
			t.Fatalf("beta/i-2 deleted by conflicting path request")
		}
	})
}

// TestPathLocatorMissingTargetReturns404 unifies single-instance read,
// heartbeat, weight and delete: a missing path target is 404 even when the
// conflicting parameters point at an existing instance, and no record moves.
func TestPathLocatorMissingTargetReturns404(t *testing.T) {
	entries := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"read", http.MethodGet, "/api/v1/services/alpha/instances/ghost", nil},
		{"read health", http.MethodGet, "/api/v1/services/alpha/instances/ghost/health", nil},
		{"read weight", http.MethodGet, "/api/v1/services/alpha/instances/ghost/weight", nil},
		{"read heartbeat", http.MethodGet, "/api/v1/services/alpha/instances/ghost/heartbeat", nil},
		{"heartbeat", http.MethodPost,
			"/api/v1/services/alpha/instances/ghost/heartbeat", conflictBody(map[string]any{"heartbeat_at": at(25)})},
		{"weight", http.MethodPut,
			"/api/v1/services/alpha/instances/ghost/weight", conflictBody(map[string]any{"weight": 42})},
		{"delete", http.MethodDelete, "/api/v1/services/alpha/instances/ghost", nil},
		{"post delete", http.MethodPost,
			"/api/v1/services/alpha/instances/ghost/delete", conflictBody(nil)},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			router, st := seedTwoServicesStore(t)
			rec := doRequest(t, router, entry.method, entry.path+conflictQuery, entry.body)
			expectSingleErrorObject(t, rec, http.StatusNotFound, "instance_not_found")

			beta, found, err := st.GetInstance("beta", "i-2")
			if err != nil || !found {
				t.Fatalf("beta lookup after 404: found=%v err=%v", found, err)
			}
			if beta.Weight != 20 || !beta.HeartbeatAt.Equal(mustParseTime(at(12))) {
				t.Fatalf("beta record changed after 404: %+v", beta)
			}
			if alpha, found, _ := st.GetInstance("alpha", "i-1"); !found ||
				alpha.Weight != 10 || !alpha.HeartbeatAt.Equal(mustParseTime(at(12))) {
				t.Fatalf("alpha record changed after 404: %+v found=%v", alpha, found)
			}
		})
	}
}

// TestPathLocatorScopesInstanceList keeps list requests on the path service,
// including conflicting locators, and answers unknown services with HTTP 200
// and an empty array. Listing never removes records.
func TestPathLocatorScopesInstanceList(t *testing.T) {
	router, st := seedTwoServicesStore(t)

	for _, target := range []string{
		"/api/v1/services/alpha/instances" + conflictQuery,
		"/api/v1/services/alpha/instances?service=beta",
		"/api/v1/list?service_name=alpha", // parameter-style entry unchanged
	} {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d %s", target, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		if out["service_name"] != "alpha" {
			t.Fatalf("GET %s listed %v, want alpha", target, out["service_name"])
		}
		ids := instanceIDs(t, out)
		want := map[string]bool{"i-1": true, "alpha-lost": true}
		if len(ids) != len(want) {
			t.Fatalf("GET %s ids = %v, want alpha records only", target, ids)
		}
		for _, id := range ids {
			if !want[id] {
				t.Fatalf("GET %s returned non-alpha record %s", target, id)
			}
		}
	}

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/missing/instances?service_name=beta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown service list: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "missing" {
		t.Fatalf("unknown service list name = %v", out["service_name"])
	}
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("unknown service list = %v, want empty", ids)
	}

	// Listing never removes anything: both lost records are still stored.
	if _, found, _ := st.GetInstance("alpha", "alpha-lost"); !found {
		t.Fatalf("list deleted alpha lost record")
	}
	if _, found, _ := st.GetInstance("beta", "beta-lost"); !found {
		t.Fatalf("list deleted beta lost record")
	}
}

// TestPathLocatorScopesDiscovery: path discovery returns only healthy,
// non-lost instances of the path service, deletes only that service's
// strictly-lost records, and never widens the scope through other sources.
func TestPathLocatorScopesDiscovery(t *testing.T) {
	for _, entry := range []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"get conflicting query", http.MethodGet,
			"/api/v1/services/alpha/discover?service_name=beta&evaluate_at=" + at(20) + "&heartbeat_timeout=10m", nil},
		{"post conflicting body", http.MethodPost, "/api/v1/services/alpha/discover",
			map[string]any{"service_name": "beta", "evaluate_at": at(20), "heartbeat_timeout": "10m"}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			router, st := seedTwoServicesStore(t)
			rec := doRequest(t, router, entry.method, entry.target, entry.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("discover: %d %s", rec.Code, rec.Body.String())
			}
			out := decodeBody(t, rec)
			if out["service_name"] != "alpha" {
				t.Fatalf("discover service = %v, want alpha", out["service_name"])
			}
			if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "i-1" {
				t.Fatalf("discover ids = %v, want [i-1]", ids)
			}
			if _, found, _ := st.GetInstance("alpha", "alpha-lost"); found {
				t.Fatalf("alpha lost record not cleaned up")
			}
			if _, found, _ := st.GetInstance("beta", "beta-lost"); !found {
				t.Fatalf("beta lost record cleaned up by alpha discovery")
			}
			if beta, found, _ := st.GetInstance("beta", "i-2"); !found || beta.Weight != 20 {
				t.Fatalf("beta online record touched: %+v", beta)
			}
		})
	}
}

// TestPathDiscoverUnknownServiceReturnsEmpty: a service without records is
// HTTP 200 with an empty instance array and deletes nothing elsewhere.
func TestPathDiscoverUnknownServiceReturnsEmpty(t *testing.T) {
	router, st := seedTwoServicesStore(t)
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/missing/discover?service_name=beta&evaluate_at="+at(20)+"&heartbeat_timeout=5m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown service discover: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "missing" {
		t.Fatalf("service_name = %v, want missing", out["service_name"])
	}
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("instances = %v, want empty", ids)
	}
	if list, _ := st.ListAllInstances(); len(list) != 4 {
		t.Fatalf("unknown-service discovery changed storage: %d records", len(list))
	}
}

// TestPathLocatorBlankSegmentRejectsAndCannotBeFilled covers both the empty
// segments the router already recognizes and whitespace-only path values:
// both are parameter errors even when query parameters or the body carry a
// valid locator, and no record is created, updated or deleted.
func TestPathLocatorBlankSegmentRejectsAndCannotBeFilled(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"empty service read", http.MethodGet,
			"/api/v1/services//instances/i-1?service_name=alpha", nil},
		{"whitespace service read", http.MethodGet,
			"/api/v1/services/%20%20/instances/i-1?service_name=alpha", nil},
		{"whitespace instance read", http.MethodGet,
			"/api/v1/services/alpha/instances/%09?instance_id=i-1", nil},
		{"empty service health read", http.MethodGet,
			"/api/v1/services//instances/i-1/health?service=alpha", nil},
		{"empty service list", http.MethodGet,
			"/api/v1/services//instances?service_name=alpha", nil},
		{"empty service discover", http.MethodGet,
			"/api/v1/services//discover?service_name=alpha&evaluate_at=" + at(20) + "&heartbeat_timeout=10m", nil},
		{"empty service register", http.MethodPost,
			"/api/v1/services//instances?service_name=alpha",
			map[string]any{"instance_id": "i-1", "weight": 1, "heartbeat_at": baseTime}},
		{"empty service overwrite", http.MethodPut,
			"/api/v1/services//instances/i-1?service_name=alpha",
			map[string]any{"weight": 1, "heartbeat_at": baseTime}},
		{"empty service heartbeat", http.MethodPost,
			"/api/v1/services//instances/i-1/heartbeat?service_name=alpha",
			map[string]any{"heartbeat_at": at(25)}},
		{"empty instance heartbeat", http.MethodPost,
			"/api/v1/services/alpha/instances//heartbeat?instance_id=i-1",
			map[string]any{"heartbeat_at": at(25)}},
		{"empty service weight", http.MethodPut,
			"/api/v1/services//instances/i-1/weight?service_name=alpha",
			map[string]any{"weight": 42}},
		{"empty instance weight", http.MethodPut,
			"/api/v1/services/alpha/instances//weight?instance_id=i-1",
			map[string]any{"weight": 42}},
		{"empty service delete", http.MethodDelete,
			"/api/v1/services//instances/i-1?service_name=alpha&instance_id=i-1", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := seedTwoServicesStore(t)
			rec := doRequest(t, router, tc.method, tc.path, tc.body)
			expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertFixtureIntact(t, st)
		})
	}
}

// TestPathLocatorMalformedJSONRejected: an unparseable JSON body on a path
// entry is the same parameter error and leaves every record untouched.
func TestPathLocatorMalformedJSONRejected(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"collection register", http.MethodPost, "/api/v1/services/alpha/instances"},
		{"overwrite", http.MethodPut, "/api/v1/services/alpha/instances/i-1"},
		{"heartbeat", http.MethodPost, "/api/v1/services/alpha/instances/i-1/heartbeat"},
		{"weight", http.MethodPut, "/api/v1/services/alpha/instances/i-1/weight"},
		{"discover", http.MethodPost, "/api/v1/services/alpha/discover"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := seedTwoServicesStore(t)
			rec := doRawRequest(t, router, tc.method, tc.path, "{not json")
			expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertFixtureIntact(t, st)
		})
	}
}
