package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// malformedObjectBodies lists every non-empty body that is not exactly one
// complete JSON object surrounded by JSON whitespace: a JSON null, array or
// other bare value, a truncated document, and any trailing content (stray
// brackets, a second value or other text) glued to the object or separated by
// whitespace. Each one must be rejected with HTTP 400 invalid_parameter
// before any business rule, lookup or storage call runs.
var malformedObjectBodies = []string{
	"null", "[]", "[{}]", "123", "true", `"text"`,
	"{", "}", "]", "{bad", `{"a":`, `{"a":1`,
	`{"weight":5}}`, `{"weight":5}]`, `{"weight":5}{}`, `{"weight":5} null`,
	`{"weight":5} 1`, `{"weight":5}"x"`, `{"weight":5},`, "{\"weight\":5} \t}\n",
	" \r\n null", "\t[]\r\n",
}

// seedBodyIntegrityFixture creates one healthy fresh target instance, one
// strictly lost instance of the same service (a valid discover/cleanup
// request would delete it) and one instance of another service. Rejected
// requests must leave all three records exactly as seeded and must never
// create the i-new instance the query strings below point at.
func seedBodyIntegrityFixture(t *testing.T, router http.Handler) {
	t.Helper()
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-1", "address": "10.0.0.8:8080",
		"port": 8080, "healthy": true, "weight": 10, "heartbeat_at": at(0),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "i-lost", "address": "old",
		"healthy": true, "weight": 1, "heartbeat_at": at(-10),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "other", "instance_id": "peer", "address": "peer",
		"healthy": true, "weight": 4, "heartbeat_at": at(0),
	})
}

func snapshotAllInstances(t *testing.T, st *store.Store) map[string]store.Instance {
	t.Helper()
	all, err := st.ListAllInstances()
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	snapshot := make(map[string]store.Instance, len(all))
	for _, instance := range all {
		snapshot[instance.ServiceName+"/"+instance.InstanceID] = instance
	}
	return snapshot
}

func assertSnapshotUnchanged(t *testing.T, before map[string]store.Instance, st *store.Store) {
	t.Helper()
	after := snapshotAllInstances(t, st)
	if len(after) != len(before) {
		t.Fatalf("record count changed: before %d after %d", len(before), len(after))
	}
	for key, wanted := range before {
		got, ok := after[key]
		if !ok {
			t.Fatalf("record %s disappeared after rejected request", key)
		}
		if got != wanted {
			t.Fatalf("record %s changed after rejected request:\nbefore %+v\nafter  %+v", key, wanted, got)
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			t.Fatalf("record %s appeared after rejected request", key)
		}
	}
}

// bodyEntry is one public entry point with a target whose query string alone
// is sufficient to complete the operation. A malformed non-empty body must
// still be rejected, so the query parameters can never rescue it.
type bodyEntry struct {
	name   string
	method string
	target string
}

func bodyIntegrityEntries() []bodyEntry {
	validTime := at(10)
	// 15m keeps the fresh i-1 (heartbeat 12:00) online at 12:10 while the
	// strictly lost i-lost (heartbeat 11:50) is exactly five minutes past.
	validDiscover := "?evaluate_at=" + validTime + "&heartbeat_timeout=15m"
	return []bodyEntry{
		// Registration and full overwrite, path style and parameter style.
		{"register-collection", http.MethodPost, "/api/v1/services/svc/instances?instance_id=i-new&weight=5&heartbeat_at=" + validTime},
		{"upsert-put-path", http.MethodPut, "/api/v1/services/svc/instances/i-1?weight=5&heartbeat_at=" + validTime},
		{"register-param", http.MethodPost, "/api/v1/register?service_name=svc&instance_id=i-new&weight=5&heartbeat_at=" + validTime},
		{"instances-post-param", http.MethodPost, "/api/v1/instances?service_name=svc&instance_id=i-new&weight=5&heartbeat_at=" + validTime},
		{"instances-put-param", http.MethodPut, "/api/v1/instances?service_name=svc&instance_id=i-new&weight=5&heartbeat_at=" + validTime},

		// Instance and attribute queries, path style and parameter style.
		{"get-instance-path", http.MethodGet, "/api/v1/services/svc/instances/i-1"},
		{"get-instance-param", http.MethodGet, "/api/v1/instances?service_name=svc&instance_id=i-1"},
		{"get-health-path", http.MethodGet, "/api/v1/services/svc/instances/i-1/health"},
		{"get-health-param", http.MethodGet, "/api/v1/instances/health?service_name=svc&instance_id=i-1"},
		{"get-weight-path", http.MethodGet, "/api/v1/services/svc/instances/i-1/weight"},
		{"get-weight-param", http.MethodGet, "/api/v1/instances/weight?service_name=svc&instance_id=i-1"},
		{"get-heartbeat-path", http.MethodGet, "/api/v1/services/svc/instances/i-1/heartbeat"},
		{"get-heartbeat-param", http.MethodGet, "/api/v1/instances/heartbeat?service_name=svc&instance_id=i-1"},

		// Instance list, path style and parameter style.
		{"list-path", http.MethodGet, "/api/v1/services/svc/instances"},
		{"list-param", http.MethodGet, "/api/v1/list?service_name=svc"},

		// Read-only service overview.
		{"overview", http.MethodGet, "/api/v1/services" + validDiscover},

		// Deletion, path style, POST alias and parameter style.
		{"delete-path", http.MethodDelete, "/api/v1/services/svc/instances/i-1"},
		{"delete-post-alias", http.MethodPost, "/api/v1/services/svc/instances/i-1/delete"},
		{"delete-param", http.MethodDelete, "/api/v1/instances?service_name=svc&instance_id=i-1"},
		{"deregister-param", http.MethodPost, "/api/v1/deregister?service_name=svc&instance_id=i-1"},

		// Single-instance heartbeat renewal and batch heartbeat.
		{"heartbeat-single", http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at=" + validTime},
		{"heartbeat-batch", http.MethodPost, "/api/v1/heartbeat?service_name=svc"},

		// Single-instance weight update.
		{"weight-update", http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=9"},

		// Single-service discovery, GET/POST and path/parameter styles.
		{"discover-get-path", http.MethodGet, "/api/v1/services/svc/discover" + validDiscover},
		{"discover-get-param", http.MethodGet, "/api/v1/discover" + validDiscover},
		{"discover-post-path", http.MethodPost, "/api/v1/services/svc/discover" + validDiscover},
		{"discover-post-param", http.MethodPost, "/api/v1/discover" + validDiscover},

		// Standalone lost-instance cleanup.
		{"cleanup", http.MethodPost, "/api/v1/cleanup" + validDiscover},
	}
}

// TestSingleObjectEntriesRejectMalformedBodies is the regression: every
// public entry that merges a JSON body with query parameters must reject any
// non-empty body that is not exactly one complete JSON object, even when the
// query string alone is sufficient, the target is missing, or storage is
// unavailable. Rejections create, update and delete nothing, in the target
// service and every other service.
func TestSingleObjectEntriesRejectMalformedBodies(t *testing.T) {
	for _, entry := range bodyIntegrityEntries() {
		router, st := testRouter(t)
		seedBodyIntegrityFixture(t, router)
		before := snapshotAllInstances(t, st)

		for _, raw := range malformedObjectBodies {
			t.Run(entry.name+"+"+fmt.Sprintf("%q", raw), func(t *testing.T) {
				rec := doRawRequest(t, router, entry.method, entry.target, raw)
				expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
				assertSnapshotUnchanged(t, before, st)
			})
		}
	}
}

// TestSingleObjectEntriesBodyErrorsTakePrecedence pins the ordering: a
// malformed body is reported as invalid_parameter instead of a missing
// target's 404 or an unavailable store's 503, and it performs no write.
func TestSingleObjectEntriesBodyErrorsTakePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"weight-missing-target", http.MethodPut,
			"/api/v1/services/ghost/instances/i-gone/weight?weight=5", "null"},
		{"weight-trailer-missing-target", http.MethodPut,
			"/api/v1/services/ghost/instances/i-gone/weight?weight=5", `{"weight":5}]`},
		{"heartbeat-missing-target", http.MethodPost,
			"/api/v1/services/ghost/instances/i-gone/heartbeat?heartbeat_at=" + at(10), "[]"},
		{"delete-missing-target", http.MethodDelete,
			"/api/v1/instances?service_name=ghost&instance_id=i-gone", "null"},
		{"deregister-missing-target", http.MethodPost,
			"/api/v1/deregister?service_name=ghost&instance_id=i-gone", `{}}`},
		{"get-missing-target", http.MethodGet,
			"/api/v1/instances?service_name=ghost&instance_id=i-gone", "null"},
		{"discover-empty-service", http.MethodPost,
			"/api/v1/discover?service_name=ghost&evaluate_at=" + at(10) + "&heartbeat_timeout=5m", "null"},
		{"cleanup-with-valid-query", http.MethodPost,
			"/api/v1/cleanup?evaluate_at=" + at(10) + "&heartbeat_timeout=5m", "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			seedBodyIntegrityFixture(t, router)
			before := snapshotAllInstances(t, st)

			rec := doRawRequest(t, router, tc.method, tc.target, tc.body)
			expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertSnapshotUnchanged(t, before, st)
		})
	}

	// Storage unavailable: the body error still wins over the 503 and the
	// store is never called.
	t.Run("storage-unavailable", func(t *testing.T) {
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
		for _, raw := range []string{"null", `{"weight":5}]`} {
			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/i-1/weight?weight=9", raw)
			expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
		}
	})
}

// TestSingleObjectEntriesAcceptBodyQueryAndMerged proves the legitimate
// paths keep working: no body or a whitespace-only body follows the query
// rules, an empty object is filled from the query string, a JSON object
// merges with same-named body fields winning, unknown fields and brackets or
// escapes inside string values are never mistaken for body errors, and the
// large-port exact round trip is preserved across the public path styles.
func TestSingleObjectEntriesAcceptBodyQueryAndMerged(t *testing.T) {
	// Registration: query only, whitespace body, empty object and a full JSON
	// body each create the requested record.
	t.Run("register query and body styles", func(t *testing.T) {
		router, _ := testRouter(t)

		queryOnly := "/api/v1/services/svc/instances?instance_id=i-q&weight=5&heartbeat_at=" + at(10)
		if rec := doRawRequest(t, router, http.MethodPost, queryOnly, ""); rec.Code != http.StatusOK {
			t.Fatalf("query-only register: status %d body %s", rec.Code, rec.Body.String())
		}
		if rec := doRawRequest(t, router, http.MethodPost, queryOnly, " \t\r\n "); rec.Code != http.StatusOK {
			t.Fatalf("whitespace-only register: status %d body %s", rec.Code, rec.Body.String())
		}
		emptyObject := "/api/v1/register?service_name=svc&instance_id=i-empty&weight=5&heartbeat_at=" + at(10)
		if rec := doRawRequest(t, router, http.MethodPost, emptyObject, "{}"); rec.Code != http.StatusOK {
			t.Fatalf("empty-object register: status %d body %s", rec.Code, rec.Body.String())
		}
		for _, id := range []string{"i-q", "i-empty"} {
			if _, found := publicInstance(t, router, "svc", id); !found {
				t.Fatalf("%s not created", id)
			}
		}

		// Body fields win over same-named query parameters: the record is
		// svc/i-body with weight 7, never svc/i-query or weight 42.
		merged := "/api/v1/services/svc/instances?service_name=other&instance_id=i-query&weight=42&heartbeat_at=" + at(9)
		rec := doRawRequest(t, router, http.MethodPost, merged,
			` {"instance_id":"i-body","weight":7,"heartbeat_at":"`+at(10)+`"} `)
		if rec.Code != http.StatusOK {
			t.Fatalf("merged register: status %d body %s", rec.Code, rec.Body.String())
		}
		got, found := publicInstance(t, router, "svc", "i-body")
		if !found {
			t.Fatalf("svc/i-body not created")
		}
		if got["weight"].(float64) != 7 || got["heartbeat_at"] != at(10) {
			t.Fatalf("body fields did not win: %v", got)
		}
		if _, found := publicInstance(t, router, "other", "i-query"); found {
			t.Fatalf("query values must not move the target")
		}

		// Unknown fields and brackets/escapes inside strings stay valid.
		raw := `{"service_name":"svc","instance_id":"i-esc","weight":10,` +
			`"heartbeat_at":"` + at(10) + `","address":"a}b]c\"d\\e",` +
			`"note":"x} ] { \""}`
		if rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register", raw); rec.Code != http.StatusOK {
			t.Fatalf("escaped-string register: status %d body %s", rec.Code, rec.Body.String())
		}
		escaped, found := publicInstance(t, router, "svc", "i-esc")
		if !found || escaped["address"] != `a}b]c"d\e` {
			t.Fatalf("address stored as %v found=%v", escaped["address"], found)
		}

		// PUT path style keeps whole-record overwrite semantics and returns
		// ports above 2^53 exactly.
		rec = doRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1",
			map[string]any{"weight": 8, "heartbeat_at": at(10),
				"port": json.Number("9007199254740993")})
		if rec.Code != http.StatusOK {
			t.Fatalf("put overwrite: status %d body %s", rec.Code, rec.Body.String())
		}
		overwritten, found := publicInstance(t, router, "svc", "i-1")
		if !found {
			t.Fatalf("i-1 missing after overwrite")
		}
		if overwritten["address"] != "" || overwritten["port"].(float64) != 9007199254740993 ||
			overwritten["healthy"] != false || overwritten["weight"].(float64) != 8 {
			t.Fatalf("overwrite did not replace the whole record: %v", overwritten)
		}
	})

	// Weight update keeps the JSON-body-wins, query-only and empty-object
	// rules, including whitespace wrapping and unknown fields.
	t.Run("weight update styles", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())

		if rec := doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=42", ""); rec.Code != http.StatusOK {
			t.Fatalf("query-only weight: status %d body %s", rec.Code, rec.Body.String())
		}
		if rec := doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=42", "{}"); rec.Code != http.StatusOK {
			t.Fatalf("empty-object weight: status %d body %s", rec.Code, rec.Body.String())
		}
		rec := doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=42",
			"\t {\"weight\":7,\"unknown\":\"x}]\"} \n")
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins weight: status %d body %s", rec.Code, rec.Body.String())
		}
		instance := decodeBody(t, rec)["instance"].(map[string]any)
		if instance["weight"].(float64) != 7 {
			t.Fatalf("weight = %v, want body value 7", instance["weight"])
		}

		// An illegal business field in the body is not repaired by the query.
		for _, raw := range []string{`{"weight":-5}`, `{"weight":"heavy"}`} {
			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/i-1/weight?weight=9", raw)
			expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
		}
		got, found := publicInstance(t, router, "svc", "i-1")
		if !found || got["weight"].(float64) != 7 {
			t.Fatalf("invalid body weight changed the record: %v", got)
		}
	})

	// Heartbeat renewal keeps the same merge rules and replaces only
	// heartbeat_at.
	t.Run("heartbeat styles", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())

		if rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+at(9), ""); rec.Code != http.StatusOK {
			t.Fatalf("query-only heartbeat: status %d body %s", rec.Code, rec.Body.String())
		}
		if rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+at(9), "{}"); rec.Code != http.StatusOK {
			t.Fatalf("empty-object heartbeat: status %d body %s", rec.Code, rec.Body.String())
		}
		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+at(9),
			`{"heartbeat_at":"`+at(10)+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins heartbeat: status %d body %s", rec.Code, rec.Body.String())
		}
		instance := decodeBody(t, rec)["instance"].(map[string]any)
		if instance["heartbeat_at"] != at(10) || instance["weight"].(float64) != 10 {
			t.Fatalf("heartbeat merge changed the wrong fields: %v", instance)
		}

		// A bad body timestamp does not fall back to the valid query value.
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+at(10),
			`{"heartbeat_at":"nope"}`)
		expectSingleErrorObject(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, found := publicInstance(t, router, "svc", "i-1")
		if !found || got["heartbeat_at"] != at(10) {
			t.Fatalf("invalid body heartbeat changed the record: %v", got)
		}

		// Batch heartbeat accepts one object and returns full records.
		registerInstance(t, router, map[string]any{
			"service_name": "svc", "instance_id": "i-2", "weight": 3, "heartbeat_at": at(0),
		})
		batch := map[string]any{
			"service_name": "svc",
			"instances": []any{
				map[string]any{"instance_id": "i-1", "heartbeat_at": at(11)},
				map[string]any{"instance_id": "i-2", "heartbeat_at": at(11)},
			},
		}
		if rec := doRequest(t, router, http.MethodPost, "/api/v1/heartbeat", batch); rec.Code != http.StatusOK {
			t.Fatalf("batch heartbeat: status %d body %s", rec.Code, rec.Body.String())
		}
	})

	// Read entries keep working with no body, with a JSON object body (body
	// fields are honored on GET too) and with unknown body fields.
	t.Run("queries accept object bodies", func(t *testing.T) {
		router, _ := testRouter(t)
		seedBodyIntegrityFixture(t, router)

		get := func(method, target, raw string) {
			t.Helper()
			rec := doRawRequest(t, router, method, target, raw)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s body %q: status %d body %s", method, target, raw, rec.Code, rec.Body.String())
			}
		}
		get(http.MethodGet, "/api/v1/services/svc/instances/i-1", "")
		get(http.MethodGet, "/api/v1/instances?service_name=svc&instance_id=i-1", `{"unknown":1}`)
		get(http.MethodGet, "/api/v1/instances/health?service_name=svc&instance_id=i-1", "{}")
		get(http.MethodGet, "/api/v1/instances/weight?service_name=svc&instance_id=i-1", "  ")
		get(http.MethodGet, "/api/v1/services/svc/instances?healthy=true", `{}`)
		get(http.MethodGet, "/api/v1/list?service_name=svc", `{"service_name":"ignored-by-nothing"}`)
		// The object body alone supplies evaluate_at and heartbeat_timeout.
		get(http.MethodGet, "/api/v1/services",
			`{"evaluate_at":"`+at(10)+`","heartbeat_timeout":"5m"}`)
	})

	// Delete still works through query-only and object-body parameter styles.
	t.Run("delete styles", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, map[string]any{
			"service_name": "svc", "instance_id": "i-q", "weight": 1, "heartbeat_at": at(0),
		})
		registerInstance(t, router, map[string]any{
			"service_name": "svc", "instance_id": "i-b", "weight": 1, "heartbeat_at": at(0),
		})

		if rec := doRawRequest(t, router, http.MethodDelete,
			"/api/v1/instances?service_name=svc&instance_id=i-q", ""); rec.Code != http.StatusOK {
			t.Fatalf("query-only delete: status %d body %s", rec.Code, rec.Body.String())
		}
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/deregister",
			`{"service_name":"svc","instance_id":"i-b"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("object-body deregister: status %d body %s", rec.Code, rec.Body.String())
		}
		if _, found := publicInstance(t, router, "svc", "i-q"); found {
			t.Fatalf("i-q not deleted")
		}
		if _, found := publicInstance(t, router, "svc", "i-b"); found {
			t.Fatalf("i-b not deleted")
		}
	})

	// Discovery keeps query-only, object-body and merged styles, the strict
	// timeout boundary and its synchronous lost cleanup.
	t.Run("discover styles and cleanup", func(t *testing.T) {
		router, st := testRouter(t)
		seedBodyIntegrityFixture(t, router)

		// Query-only GET returns the fresh instance; the strictly lost record
		// is removed by the request cleanup.
		rec := doRawRequest(t, router, http.MethodGet,
			"/api/v1/services/svc/discover?evaluate_at="+at(10)+"&heartbeat_timeout=15m", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("query-only discover: status %d body %s", rec.Code, rec.Body.String())
		}
		if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "i-1" {
			t.Fatalf("discover hits = %v, want [i-1]", ids)
		}
		if _, found, _ := st.GetInstance("svc", "i-lost"); found {
			t.Fatalf("lost instance not removed by valid discover")
		}

		// POST parameter style takes all parameters from one JSON object.
		registerInstance(t, router, map[string]any{
			"service_name": "alpha", "instance_id": "i-1", "healthy": true,
			"weight": 1, "heartbeat_at": at(0),
		})
		rec = doRawRequest(t, router, http.MethodPost, "/api/v1/discover",
			` {"service_name":"alpha","evaluate_at":"`+at(10)+`","heartbeat_timeout":"15m","extra":1} `)
		if rec.Code != http.StatusOK {
			t.Fatalf("object-body discover: status %d body %s", rec.Code, rec.Body.String())
		}
		if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "i-1" {
			t.Fatalf("body discover hits = %v", ids)
		}

		// A merged request lets the body's evaluate_at win over the query one.
		registerInstance(t, router, map[string]any{
			"service_name": "beta", "instance_id": "i-1", "healthy": true,
			"weight": 1, "heartbeat_at": at(0),
		})
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/beta/discover?evaluate_at="+at(2)+"&heartbeat_timeout=15m",
			`{"evaluate_at":"`+at(10)+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("merged discover: status %d body %s", rec.Code, rec.Body.String())
		}
		if out := decodeBody(t, rec); out["evaluate_at"] != at(10) {
			t.Fatalf("body evaluate_at did not win: %v", out["evaluate_at"])
		}
	})

	// Cleanup keeps query-only, object-body and empty-object-plus-query styles
	// and its scoped atomic deletion.
	t.Run("cleanup styles", func(t *testing.T) {
		router, st := testRouter(t)
		seedBodyIntegrityFixture(t, router)
		registerInstance(t, router, map[string]any{
			"service_name": "other", "instance_id": "i-lost", "healthy": true,
			"weight": 1, "heartbeat_at": at(-10),
		})

		// Query-only no body: scoped to svc through the query string so the
		// lost record of the other service survives.
		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/cleanup?service_name=svc&evaluate_at="+at(10)+"&heartbeat_timeout=15m", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("query-only cleanup: status %d body %s", rec.Code, rec.Body.String())
		}
		if out := decodeBody(t, rec); out["removed"] != float64(1) {
			t.Fatalf("removed = %v, want 1", out["removed"])
		}
		if _, found, _ := st.GetInstance("svc", "i-lost"); found {
			t.Fatalf("svc/i-lost not cleaned")
		}
		if _, found, _ := st.GetInstance("other", "i-lost"); !found {
			t.Fatalf("other/i-lost must survive scoped cleanup")
		}

		// Empty object is filled from the query string.
		registerInstance(t, router, map[string]any{
			"service_name": "svc", "instance_id": "i-lost2", "healthy": true,
			"weight": 1, "heartbeat_at": at(-10),
		})
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/cleanup?evaluate_at="+at(10)+"&heartbeat_timeout=15m", "{}")
		if rec.Code != http.StatusOK {
			t.Fatalf("empty-object cleanup: status %d body %s", rec.Code, rec.Body.String())
		}
		if out := decodeBody(t, rec); out["removed"] != float64(2) {
			t.Fatalf("removed = %v, want 2 (svc/i-lost2 and other/i-lost)", out["removed"])
		}

		// A JSON object body supplies everything on its own.
		registerInstance(t, router, map[string]any{
			"service_name": "gamma", "instance_id": "i-lost", "healthy": true,
			"weight": 1, "heartbeat_at": at(-10),
		})
		rec = doRawRequest(t, router, http.MethodPost, "/api/v1/cleanup",
			`{"service_name":"gamma","evaluate_at":"`+at(10)+`","heartbeat_timeout":"15m"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("object-body cleanup: status %d body %s", rec.Code, rec.Body.String())
		}
		if out := decodeBody(t, rec); out["removed"] != float64(1) {
			t.Fatalf("removed = %v, want 1", out["removed"])
		}
	})
}

// TestSingleObjectEntriesWhitespaceOnlyBodiesStayQueryBased confirms the
// explicit exception on every entry style: a missing or whitespace-only body
// never becomes a body error and the query parameters drive the request.
func TestSingleObjectEntriesWhitespaceOnlyBodiesStayQueryBased(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"get", http.MethodGet, "/api/v1/instances?service_name=svc&instance_id=i-1"},
		{"list", http.MethodGet, "/api/v1/list?service_name=svc"},
		{"overview", http.MethodGet, "/api/v1/services?evaluate_at=" + at(10) + "&heartbeat_timeout=5m"},
		{"weight", http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=3"},
		{"heartbeat", http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at=" + at(10)},
		{"discover", http.MethodPost, "/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=15m"},
	}
	for _, tc := range cases {
		for _, raw := range []string{"", "   ", "\t\r\n "} {
			t.Run(tc.name+"+"+fmt.Sprintf("%q", raw), func(t *testing.T) {
				rec := doRawRequest(t, router, tc.method, tc.target, raw)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), `"error"`) {
					t.Fatalf("whitespace body produced an error response: %s", rec.Body.String())
				}
			})
		}
	}
}
