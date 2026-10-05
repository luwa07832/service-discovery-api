package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// This file pins the path-locator authority: on path-style entries the
// service name and instance id taken from the URL path always win over any
// conflicting, empty, non-string or synonymous field carried by the JSON body
// or query string, and every assertion checks both the response target and
// the actual storage change.

// pathFixture seeds two services with two instances each and returns the
// store for direct storage assertions.
func pathFixture(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	router, st := testRouter(t)
	records := []map[string]any{
		{"service_name": "alpha", "instance_id": "i-1", "address": "a1",
			"port": 8001, "healthy": true, "weight": 10, "heartbeat_at": at(0)},
		{"service_name": "alpha", "instance_id": "i-2", "address": "a2",
			"port": 8002, "healthy": true, "weight": 20, "heartbeat_at": at(2)},
		{"service_name": "beta", "instance_id": "i-1", "address": "b1",
			"port": 9001, "healthy": true, "weight": 30, "heartbeat_at": at(0)},
		{"service_name": "beta", "instance_id": "i-2", "address": "b2",
			"port": 9002, "healthy": true, "weight": 40, "heartbeat_at": at(2)},
	}
	for _, record := range records {
		registerInstance(t, router, record)
	}
	return router, st
}

func expectStored(t *testing.T, st *store.Store, service, id string, wantWeight float64, heartbeatMinute int, found bool) {
	t.Helper()
	got, ok, err := st.GetInstance(service, id)
	if err != nil {
		t.Fatalf("get %s/%s: %v", service, id, err)
	}
	if ok != found {
		t.Fatalf("%s/%s found = %v, want %v", service, id, ok, found)
	}
	if !found {
		return
	}
	if got.Weight != wantWeight {
		t.Fatalf("%s/%s weight = %v, want %v", service, id, got.Weight, wantWeight)
	}
	if want := mustParseTime(at(heartbeatMinute)); !got.HeartbeatAt.Equal(want) {
		t.Fatalf("%s/%s heartbeat = %v, want %v", service, id, got.HeartbeatAt, want)
	}
}

func assertFixtureIntact(t *testing.T, st *store.Store) {
	t.Helper()
	expectStored(t, st, "alpha", "i-1", 10, 0, true)
	expectStored(t, st, "alpha", "i-2", 20, 2, true)
	expectStored(t, st, "beta", "i-1", 30, 0, true)
	expectStored(t, st, "beta", "i-2", 40, 2, true)
}

func TestPathLocatorWinsOnSingleInstanceQueries(t *testing.T) {
	router, st := pathFixture(t)

	targets := []string{
		"/api/v1/services/alpha/instances/i-1?service_name=beta&instance_id=i-2",
		"/api/v1/services/alpha/instances/i-1?service=beta&id=i-2",
		"/api/v1/services/alpha/instances/i-1?serviceName=beta&instanceId=i-2",
	}
	for _, target := range targets {
		// Query-string conflict.
		rec := doRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		instance := decodeBody(t, rec)["instance"].(map[string]any)
		if instance["service_name"] != "alpha" || instance["instance_id"] != "i-1" ||
			instance["address"] != "a1" || instance["weight"].(float64) != 10 {
			t.Fatalf("%s returned %v, want alpha/i-1", target, instance)
		}

		// The same conflict through a JSON body, including empty and
		// non-string values, must not move the target either.
		for _, body := range []map[string]any{
			{"service_name": "beta", "instance_id": "i-2"},
			{"service_name": "", "instance_id": ""},
			{"service_name": 123, "instance_id": true},
			{"service_name": nil, "instance_id": nil},
			{"service": "beta", "id": "i-2"},
		} {
			rec := doRequest(t, router, http.MethodGet,
				"/api/v1/services/alpha/instances/i-1?service_name=beta&instance_id=i-2", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("body %v: %d %s", body, rec.Code, rec.Body.String())
			}
			instance := decodeBody(t, rec)["instance"].(map[string]any)
			if instance["service_name"] != "alpha" || instance["instance_id"] != "i-1" {
				t.Fatalf("body %v returned %v, want alpha/i-1", body, instance)
			}
		}
	}

	// Property queries carry the path identity in the response too.
	for _, suffix := range []string{"health", "weight", "heartbeat"} {
		rec := doRequest(t, router, http.MethodGet,
			"/api/v1/services/alpha/instances/i-1/"+suffix+"?service_name=beta&instance_id=i-2", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", suffix, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		if out["service_name"] != "alpha" || out["instance_id"] != "i-1" {
			t.Fatalf("%s response identity = %v/%v, want alpha/i-1", suffix, out["service_name"], out["instance_id"])
		}
	}
	assertFixtureIntact(t, st)
}

func TestPathLocatorWinsOnInstanceOverwrite(t *testing.T) {
	router, st := pathFixture(t)

	// The whole-record overwrite lands on the path target even though the
	// body names another service and instance.
	body := map[string]any{
		"service_name": "beta", "instance_id": "i-2",
		"address": "overwritten", "port": 7007, "healthy": false,
		"weight": 99, "heartbeat_at": at(5),
	}
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/alpha/instances/i-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("overwrite: %d %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "alpha" || instance["instance_id"] != "i-1" ||
		instance["weight"].(float64) != 99 || instance["address"] != "overwritten" {
		t.Fatalf("overwrite response = %v, want path target alpha/i-1", instance)
	}
	expectStored(t, st, "alpha", "i-1", 99, 5, true)
	// Nothing else changed: the body-named record keeps its original values.
	expectStored(t, st, "alpha", "i-2", 20, 2, true)
	expectStored(t, st, "beta", "i-1", 30, 0, true)
	expectStored(t, st, "beta", "i-2", 40, 2, true)
}

func TestPathLocatorWinsOnHeartbeat(t *testing.T) {
	router, st := pathFixture(t)

	body := map[string]any{"service_name": "beta", "instance_id": "i-2", "heartbeat_at": at(8)}
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/alpha/instances/i-1/heartbeat", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "alpha" || instance["instance_id"] != "i-1" {
		t.Fatalf("heartbeat response = %v/%v, want alpha/i-1", instance["service_name"], instance["instance_id"])
	}
	if instance["heartbeat_at"] != at(8) || instance["weight"].(float64) != 10 {
		t.Fatalf("heartbeat changed other fields: %v", instance)
	}
	expectStored(t, st, "alpha", "i-1", 10, 8, true)
	expectStored(t, st, "alpha", "i-2", 20, 2, true)
	expectStored(t, st, "beta", "i-1", 30, 0, true)
	expectStored(t, st, "beta", "i-2", 40, 2, true)
}

func TestPathLocatorWinsOnWeightChange(t *testing.T) {
	router, st := pathFixture(t)

	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/alpha/instances/i-1/weight?service_name=beta&instance_id=i-2",
		map[string]any{"weight": 77})
	if rec.Code != http.StatusOK {
		t.Fatalf("weight: %d %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "alpha" || instance["instance_id"] != "i-1" ||
		instance["weight"].(float64) != 77 {
		t.Fatalf("weight response = %v, want alpha/i-1 at 77", instance)
	}
	expectStored(t, st, "alpha", "i-1", 77, 0, true)
	expectStored(t, st, "alpha", "i-2", 20, 2, true)
	expectStored(t, st, "beta", "i-1", 30, 0, true)
	expectStored(t, st, "beta", "i-2", 40, 2, true)
}

func TestPathLocatorWinsOnDelete(t *testing.T) {
	for _, target := range []string{
		"/api/v1/services/alpha/instances/i-1?service_name=beta&instance_id=i-2",
		"/api/v1/services/alpha/instances/i-1/delete?service_name=beta&instance_id=i-2",
	} {
		t.Run(target, func(t *testing.T) {
			router, st := pathFixture(t)
			method := http.MethodDelete
			if strings.HasSuffix(strings.SplitN(target, "?", 2)[0], "/delete") {
				method = http.MethodPost
			}
			rec := doRequest(t, router, method, target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
			out := decodeBody(t, rec)
			if out["service_name"] != "alpha" || out["instance_id"] != "i-1" || out["deleted"] != true {
				t.Fatalf("delete response = %v, want alpha/i-1 deleted", out)
			}
			expectStored(t, st, "alpha", "i-1", 0, 0, false)
			expectStored(t, st, "alpha", "i-2", 20, 2, true)
			expectStored(t, st, "beta", "i-1", 30, 0, true)
			expectStored(t, st, "beta", "i-2", 40, 2, true)
		})
	}
}

func TestPathLocatorWinsOnList(t *testing.T) {
	router, st := pathFixture(t)

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances?service_name=beta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "alpha" {
		t.Fatalf("list service_name = %v, want alpha", out["service_name"])
	}
	ids := instanceIDs(t, out)
	if len(ids) != 2 {
		t.Fatalf("list ids = %v, want two alpha instances", ids)
	}
	for _, id := range ids {
		if id != "i-1" && id != "i-2" {
			t.Fatalf("list leaked a beta instance: %v", ids)
		}
	}
	assertFixtureIntact(t, st)
}

func TestPathLocatorScopesDiscoverResultAndCleanup(t *testing.T) {
	router, st := testRouter(t)
	records := []map[string]any{
		{"service_name": "alpha", "instance_id": "a-fresh", "healthy": true,
			"weight": 10, "heartbeat_at": at(9)},
		{"service_name": "alpha", "instance_id": "a-lost", "healthy": true,
			"weight": 1, "heartbeat_at": at(0)},
		{"service_name": "beta", "instance_id": "b-fresh", "healthy": true,
			"weight": 10, "heartbeat_at": at(9)},
		{"service_name": "beta", "instance_id": "b-lost", "healthy": true,
			"weight": 1, "heartbeat_at": at(0)},
	}
	for _, record := range records {
		registerInstance(t, router, record)
	}

	// Query-string conflict on the GET entry.
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/discover?service_name=beta&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get discover: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "alpha" {
		t.Fatalf("get discover service_name = %v, want alpha", out["service_name"])
	}
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "a-fresh" {
		t.Fatalf("get discover ids = %v, want [a-fresh]", ids)
	}

	// Re-seed the alpha lost record and try again through the POST entry with
	// a conflicting body value.
	registerInstance(t, router, records[1])
	rec = doRequest(t, router, http.MethodPost, "/api/v1/services/alpha/discover", map[string]any{
		"service_name": "beta", "evaluate_at": at(10), "heartbeat_timeout": 60,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("post discover: %d %s", rec.Code, rec.Body.String())
	}
	out = decodeBody(t, rec)
	if out["service_name"] != "alpha" {
		t.Fatalf("post discover service_name = %v, want alpha", out["service_name"])
	}
	if ids := instanceIDs(t, out); len(ids) != 1 || ids[0] != "a-fresh" {
		t.Fatalf("post discover ids = %v, want [a-fresh]", ids)
	}

	// Cleanup is strictly scoped to the path service: alpha's lost record is
	// gone, beta's equally-lost record is untouched.
	if _, found, _ := st.GetInstance("alpha", "a-lost"); found {
		t.Fatalf("alpha/a-lost should have been cleaned up")
	}
	if _, found, _ := st.GetInstance("beta", "b-lost"); !found {
		t.Fatalf("beta/b-lost must not be cleaned up by an alpha-scoped discovery")
	}
	expectStored(t, st, "alpha", "a-fresh", 10, 9, true)
	expectStored(t, st, "beta", "b-fresh", 10, 9, true)
}

func TestMissingPathTargetIs404EvenWhenConflictExists(t *testing.T) {
	router, st := pathFixture(t)

	cases := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"get instance", http.MethodGet,
			"/api/v1/services/alpha/instances/gone?service_name=beta&instance_id=i-1", nil},
		{"get health", http.MethodGet,
			"/api/v1/services/alpha/instances/gone/health?service_name=beta&instance_id=i-1", nil},
		{"get weight", http.MethodGet,
			"/api/v1/services/alpha/instances/gone/weight?service_name=beta&instance_id=i-1", nil},
		{"get heartbeat", http.MethodGet,
			"/api/v1/services/alpha/instances/gone/heartbeat?service_name=beta&instance_id=i-1", nil},
		{"heartbeat renewal", http.MethodPost,
			"/api/v1/services/alpha/instances/gone/heartbeat?service_name=beta&instance_id=i-1",
			map[string]any{"heartbeat_at": at(8)}},
		{"weight change", http.MethodPut,
			"/api/v1/services/alpha/instances/gone/weight?service_name=beta&instance_id=i-1",
			map[string]any{"weight": 77}},
		{"delete", http.MethodDelete,
			"/api/v1/services/alpha/instances/gone?service_name=beta&instance_id=i-1", nil},
		{"post delete", http.MethodPost,
			"/api/v1/services/alpha/instances/gone/delete?service_name=beta&instance_id=i-1", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, tc.method, tc.target, tc.body)
			expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
			// No fallback to the existing conflict target, no new record.
			assertFixtureIntact(t, st)
			if _, found, _ := st.GetInstance("alpha", "gone"); found {
				t.Fatalf("missing path target was created")
			}
		})
	}
}

func TestEmptyPathSegmentsAreParameterErrors(t *testing.T) {
	router, st := pathFixture(t)

	conflict := "?service_name=beta&instance_id=i-2"
	cases := []struct {
		method string
		target string
		body   any
	}{
		// Slash-only service segment.
		{http.MethodGet, "/api/v1/services/", nil},
		{http.MethodGet, "/api/v1/services//?service_name=beta", nil},
		{http.MethodDelete, "/api/v1/services//instances?service_name=beta", nil},
		{http.MethodGet, "/api/v1/services//instances", nil},
		{http.MethodPost, "/api/v1/services//instances" + conflict,
			map[string]any{"instance_id": "i-9", "weight": 1, "heartbeat_at": at(8)}},
		{http.MethodPost, "/api/v1/services//instances/batch",
			map[string]any{"service_name": "beta", "instances": []any{batchEntry("i-1")}}},
		{http.MethodGet, "/api/v1/services//instances/i-1" + conflict, nil},
		{http.MethodGet, "/api/v1/services//instances/i-1/health" + conflict, nil},
		{http.MethodGet, "/api/v1/services//instances/i-1/weight" + conflict, nil},
		{http.MethodGet, "/api/v1/services//instances/i-1/heartbeat" + conflict, nil},
		{http.MethodPut, "/api/v1/services//instances/i-1" + conflict,
			map[string]any{"weight": 5, "heartbeat_at": at(8)}},
		{http.MethodPost, "/api/v1/services//instances/i-1/heartbeat" + conflict,
			map[string]any{"heartbeat_at": at(8)}},
		{http.MethodPut, "/api/v1/services//instances/i-1/weight" + conflict,
			map[string]any{"weight": 5}},
		{http.MethodDelete, "/api/v1/services//instances/i-1" + conflict, nil},
		{http.MethodPost, "/api/v1/services//instances/i-1/delete" + conflict, nil},
		{http.MethodPut, "/api/v1/services//instances/weight",
			map[string]any{"updates": []any{map[string]any{"instance_id": "i-1", "weight": 5}}}},
		{http.MethodPut, "/api/v1/services//instances/health",
			map[string]any{"updates": []any{map[string]any{"instance_id": "i-1", "healthy": false}}}},
		{http.MethodGet, "/api/v1/services//discover?service_name=beta&evaluate_at=" + at(10) + "&heartbeat_timeout=1m", nil},
		{http.MethodPost, "/api/v1/services//discover",
			map[string]any{"service_name": "beta", "evaluate_at": at(10), "heartbeat_timeout": 60}},
		// Empty instance segment: only the doubled slash is an empty segment;
		// a single trailing slash names the collection and is asserted
		// separately below.
		{http.MethodGet, "/api/v1/services/alpha/instances//" + conflict, nil},
		{http.MethodPut, "/api/v1/services/alpha/instances//" + conflict,
			map[string]any{"weight": 5, "heartbeat_at": at(8)}},
		{http.MethodDelete, "/api/v1/services/alpha/instances//" + conflict, nil},
		{http.MethodGet, "/api/v1/services/alpha/instances//health" + conflict, nil},
		{http.MethodGet, "/api/v1/services/alpha/instances//weight" + conflict, nil},
		{http.MethodGet, "/api/v1/services/alpha/instances//heartbeat" + conflict, nil},
		{http.MethodPost, "/api/v1/services/alpha/instances//heartbeat" + conflict,
			map[string]any{"heartbeat_at": at(8)}},
		{http.MethodPut, "/api/v1/services/alpha/instances//weight" + conflict,
			map[string]any{"weight": 5}},
		{http.MethodPut, "/api/v1/services/alpha/instances//health" + conflict,
			map[string]any{"healthy": false}},
		{http.MethodPost, "/api/v1/services/alpha/instances//delete" + conflict, nil},
	}
	for _, tc := range cases {
		rec := doRequest(t, router, tc.method, tc.target, tc.body)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	}
	assertFixtureIntact(t, st)
	if all, err := st.ListAllInstances(); err != nil || len(all) != 4 {
		t.Fatalf("empty-segment requests changed storage: %d rows err=%v", len(all), err)
	}
}

func TestWhitespacePathSegmentsAreParameterErrors(t *testing.T) {
	router, st := pathFixture(t)

	cases := []struct {
		method string
		target string
		body   any
	}{
		{http.MethodGet, "/api/v1/services/%20/instances/i-1?service_name=beta", nil},
		{http.MethodGet, "/api/v1/services/%09/instances/i-1?service_name=beta", nil},
		{http.MethodPost, "/api/v1/services/%20/instances?instance_id=i-9",
			map[string]any{"instance_id": "i-9", "weight": 1, "heartbeat_at": at(8)}},
		{http.MethodGet, "/api/v1/services/alpha/instances/%20?instance_id=i-1", nil},
		{http.MethodGet, "/api/v1/services/alpha/instances/%20/health?instance_id=i-1", nil},
		{http.MethodGet, "/api/v1/services/alpha/instances/%20/weight?instance_id=i-1", nil},
		{http.MethodGet, "/api/v1/services/alpha/instances/%20/heartbeat?instance_id=i-1", nil},
		{http.MethodPut, "/api/v1/services/alpha/instances/%20?instance_id=i-1",
			map[string]any{"weight": 5, "heartbeat_at": at(8)}},
		{http.MethodPost, "/api/v1/services/alpha/instances/%20/heartbeat?instance_id=i-1",
			map[string]any{"heartbeat_at": at(8)}},
		{http.MethodPut, "/api/v1/services/alpha/instances/%20/weight?instance_id=i-1",
			map[string]any{"weight": 5}},
		{http.MethodDelete, "/api/v1/services/alpha/instances/%20?instance_id=i-1", nil},
		{http.MethodPost, "/api/v1/services/alpha/instances/%20/delete?instance_id=i-1", nil},
		{http.MethodGet, "/api/v1/services/%20/discover?service_name=beta&evaluate_at=" + at(10) + "&heartbeat_timeout=1m", nil},
		{http.MethodPost, "/api/v1/services/%20/discover",
			map[string]any{"service_name": "beta", "evaluate_at": at(10), "heartbeat_timeout": 60}},
	}
	for _, tc := range cases {
		rec := doRequest(t, router, tc.method, tc.target, tc.body)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	}
	assertFixtureIntact(t, st)
}

func TestCollectionURLWithSingleTrailingSlashStillServed(t *testing.T) {
	router, st := pathFixture(t)

	// One trailing slash is the collection URL itself (no empty instance
	// segment); it keeps working without redirecting.
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances/?service_name=beta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get collection with slash: %d %s", rec.Code, rec.Body.String())
	}
	if out := decodeBody(t, rec); out["service_name"] != "alpha" {
		t.Fatalf("collection with slash targeted %v, want alpha", out["service_name"])
	}
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/alpha/instances/", map[string]any{
			"instance_id": "i-9", "weight": 1, "heartbeat_at": at(8),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("post collection with slash: %d %s", rec.Code, rec.Body.String())
	}
	expectStored(t, st, "alpha", "i-9", 1, 8, true)
}

func TestCollectionRegistrationTakesInstanceFromParameters(t *testing.T) {
	router, st := pathFixture(t)

	// Instance id from the JSON body; the body's conflicting service name is
	// ignored because the collection path names only the service.
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/services/alpha/instances", map[string]any{
			"service_name": "beta", "instance_id": "i-9",
			"weight": 1, "heartbeat_at": at(8),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("body instance: %d %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "alpha" || instance["instance_id"] != "i-9" {
		t.Fatalf("body instance response = %v/%v, want alpha/i-9", instance["service_name"], instance["instance_id"])
	}
	expectStored(t, st, "alpha", "i-9", 1, 8, true)

	// Instance id from the query string when the body omits it.
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/alpha/instances?instance_id=i-10", map[string]any{
			"weight": 1, "heartbeat_at": at(8),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("query instance: %d %s", rec.Code, rec.Body.String())
	}
	instance = decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "alpha" || instance["instance_id"] != "i-10" {
		t.Fatalf("query instance response = %v/%v, want alpha/i-10", instance["service_name"], instance["instance_id"])
	}
	expectStored(t, st, "alpha", "i-10", 1, 8, true)

	// The fixture records are untouched, and nothing landed under beta.
	expectStored(t, st, "beta", "i-2", 40, 2, true)
	if _, found, _ := st.GetInstance("beta", "i-9"); found {
		t.Fatalf("body service name leaked into storage")
	}
}

func TestUnknownServicePathReturnsEmpty200(t *testing.T) {
	router, st := pathFixture(t)

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/nope/instances?service_name=beta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["service_name"] != "nope" {
		t.Fatalf("list service_name = %v, want nope", out["service_name"])
	}
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("list ids = %v, want empty", ids)
	}

	// Path discovery of an unknown service returns an empty list and must not
	// read or clean the conflicting service named in the query string.
	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/services/nope/discover?service_name=beta&evaluate_at="+at(10)+"&heartbeat_timeout=1m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover: %d %s", rec.Code, rec.Body.String())
	}
	out = decodeBody(t, rec)
	if out["service_name"] != "nope" {
		t.Fatalf("discover service_name = %v, want nope", out["service_name"])
	}
	if ids := instanceIDs(t, out); len(ids) != 0 {
		t.Fatalf("discover ids = %v, want empty", ids)
	}
	assertFixtureIntact(t, st)
}

func TestMalformedJSONOnPathEntriesChangesNothing(t *testing.T) {
	router, st := pathFixture(t)

	cases := []struct {
		method string
		target string
		raw    string
	}{
		{http.MethodGet, "/api/v1/services/alpha/instances/i-1?service_name=beta", "{bad"},
		{http.MethodPut, "/api/v1/services/alpha/instances/i-1/weight?service_name=beta", "{bad"},
		{http.MethodPost, "/api/v1/services/alpha/instances/i-1/heartbeat?service_name=beta", "{bad"},
		{http.MethodDelete, "/api/v1/services/alpha/instances/i-1?service_name=beta", "{bad"},
		{http.MethodPost, "/api/v1/services/alpha/discover?service_name=beta", "{bad"},
	}
	for _, tc := range cases {
		rec := doRawRequest(t, router, tc.method, tc.target, tc.raw)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	}
	assertFixtureIntact(t, st)
	if all, err := st.ListAllInstances(); err != nil || len(all) != 4 {
		t.Fatalf("malformed requests changed storage: %d rows err=%v", len(all), err)
	}
}

func TestPathLocatorStillHonorsConflictOnParameterEntries(t *testing.T) {
	// Parameter-style entries have no path locator: their lookup rules are
	// unchanged and aliases still resolve normally.
	router, st := pathFixture(t)

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/instances?service=beta&id=i-2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("parameter entry: %d %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "beta" || instance["instance_id"] != "i-2" {
		t.Fatalf("parameter entry returned %v/%v, want beta/i-2", instance["service_name"], instance["instance_id"])
	}
	assertFixtureIntact(t, st)
}

func TestPathLocatorIgnoresNonStringConflictsOnWrites(t *testing.T) {
	router, st := pathFixture(t)

	// Non-string and nil locator values in the body must be ignored rather
	// than either retargeting the request or turning it into a parameter
	// error, because the path locator is authoritative.
	rec := doRequest(t, router, http.MethodPut,
		"/api/v1/services/alpha/instances/i-1",
		map[string]any{"service_name": 123, "instance_id": true, "weight": 5, "heartbeat_at": at(8)})
	if rec.Code != http.StatusOK {
		t.Fatalf("overwrite: %d %s", rec.Code, rec.Body.String())
	}
	expectStored(t, st, "alpha", "i-1", 5, 8, true)

	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/alpha/instances/i-1/heartbeat",
		map[string]any{"service_name": nil, "instance": 4, "heartbeat_at": at(9)})
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	expectStored(t, st, "alpha", "i-1", 5, 9, true)

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/alpha/instances/i-1/weight",
		map[string]any{"service_name": 9, "instance_id": 1.5, "weight": 66})
	if rec.Code != http.StatusOK {
		t.Fatalf("weight: %d %s", rec.Code, rec.Body.String())
	}
	expectStored(t, st, "alpha", "i-1", 66, 9, true)

	expectStored(t, st, "beta", "i-1", 30, 0, true)
	expectStored(t, st, "beta", "i-2", 40, 2, true)
}

func TestPathLocatorTrimsPathValueBeforeLookup(t *testing.T) {
	// A concrete path segment is trimmed for lookup the same way parameter
	// values are, so padded segments address the trimmed instance rather than
	// failing or falling back.
	router, _ := pathFixture(t)

	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances/%20i-1%20?service_name=beta&instance_id=i-2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("padded path: %d %s", rec.Code, rec.Body.String())
	}
	instance := decodeBody(t, rec)["instance"].(map[string]any)
	if instance["service_name"] != "alpha" || instance["instance_id"] != "i-1" {
		t.Fatalf("padded path returned %v/%v, want alpha/i-1", instance["service_name"], instance["instance_id"])
	}
}
