package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// Bytes that are Unicode whitespace but NOT JSON whitespace: vertical tab
// U+000B, form feed U+000C, no-break space U+00A0, em space U+2003 and
// ideographic space U+3000. JSON parsers reject every one of them outside a
// string, so a body containing only such bytes (possibly mixed with the four
// legal JSON whitespace bytes) must never be treated as an absent body.
var nonJSONWhitespaceBodies = []string{
	"\u000b",
	"\u000c",
	"\u00a0",
	"\u2003",
	"　",
	"\u000b\u000c\u00a0\u2003　",
	" \u00a0\t",
	"\u2003\r\n",
	" \t\r\n\u00a0 \t",
	"\u00a0\u00a0 ",
	"\r\n\t \u000b",
	"\u000c 　",
}

// framingBodies wraps an otherwise valid object with non-JSON whitespace
// before it, after it, or mixed in with legal JSON whitespace. The bytes are
// document framing here, not JSON string content, so every one is a body
// integrity error.
func framingBodies(object string) []string {
	return []string{
		"\u00a0" + object,
		"\u000b " + object,
		" \t\r\n\u00a0 " + object,
		object + "\u3000",
		object + " \r\n\u2003",
		"\u2003 " + object + "\r\n\t\u000c",
	}
}

// TestNonJSONWhitespaceBodiesRejectedOnPublicEntries drives every published
// optional-body entry (registration, queries, deletion, heartbeat, weight
// update, discovery and cleanup) together with all aliases already listed by
// bodyIntegrityEntries. A body made only of non-JSON whitespace must produce
// HTTP 400 invalid_parameter with only the top-level error object, even when
// the target URL carries every valid query parameter, and must create,
// overwrite, renew, modify or delete nothing; the same holds when such bytes
// frame a complete valid object.
func TestNonJSONWhitespaceBodiesRejectedOnPublicEntries(t *testing.T) {
	for _, ep := range bodyIntegrityEntries() {
		ep := ep
		t.Run(ep.name, func(t *testing.T) {
			payloads := append([]string{}, nonJSONWhitespaceBodies...)
			payloads = append(payloads, framingBodies(ep.object)...)

			targets := []bodyIntegrityTarget{
				target(ep.method, ep.pathTarget),
				target(ep.method, ep.altTarget),
			}
			targets = append(targets, ep.extraTargets...)
			for _, tgt := range targets {
				tgt := tgt
				t.Run(tgt.method+" "+tgt.path, func(t *testing.T) {
					router, st := testRouter(t)
					seedBodyIntegrityFixture(t, router)
					before := snapshotRows(t, st)

					for _, raw := range payloads {
						raw := raw
						t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
							rec := doRawRequest(t, router, tgt.method, tgt.path, raw)
							expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")

							if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
								t.Fatalf("records changed after body %q on %s %s:\nbefore %+v\nafter  %+v",
									raw, tgt.method, tgt.path, before, after)
							}
						})
					}
				})
			}
		})
	}
}

// TestNonJSONWhitespaceBodiesRejectedOnStrictBatchEntries covers the
// body-required batch entries that do not appear in bodyIntegrityEntries:
// batch registration (path and body styles), batch weight update, batch
// health update and batch discovery. Empty, pure-JSON-whitespace and {}
// bodies stay rejected as before, query parameters still cannot be merged,
// and non-JSON whitespace alone or framing a valid object is rejected with no
// partial write or lost-record deletion.
func TestNonJSONWhitespaceBodiesRejectedOnStrictBatchEntries(t *testing.T) {
	healthObject := `{"updates":[` +
		`{"instance_id":"i-1","healthy":false},` +
		`{"instance_id":"i-2","healthy":true}]}`
	discoverObject := `{"service_names":["svc","other"],` +
		`"evaluate_at":"2026-10-01T12:10:00Z","heartbeat_timeout":600}`

	type strictEndpoint struct {
		name   string
		method string
		path   string
		object string
		query  string // query string that must never rescue a rejected body
	}
	endpoints := []strictEndpoint{
		{"register-path", http.MethodPost, "/api/v1/services/svc/instances/batch",
			registerTrailerObjectByPath, "?service_name=other"},
		{"register-body", http.MethodPost, "/api/v1/register/batch",
			registerTrailerObjectByBody, "?service_name=other"},
		{"weight", http.MethodPut, "/api/v1/services/svc/instances/weight",
			weightTrailerObject, "?weight=1"},
		{"health", http.MethodPut, "/api/v1/services/svc/instances/health",
			healthObject, "?healthy=false"},
		{"discover", http.MethodPost, "/api/v1/discover/batch",
			discoverObject, "?service_names=svc&evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=600"},
	}
	for _, ep := range endpoints {
		ep := ep
		t.Run(ep.name, func(t *testing.T) {
			payloads := append([]string{}, nonJSONWhitespaceBodies...)
			payloads = append(payloads, framingBodies(ep.object)...)

			router, st := testRouter(t)
			registerInstance(t, router, weightRecord())
			second := weightRecord()
			second["instance_id"] = "i-2"
			second["weight"] = 3
			registerInstance(t, router, second)
			peer := weightRecord()
			peer["service_name"] = "other"
			registerInstance(t, router, peer)

			for _, raw := range payloads {
				raw := raw
				t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
					rec := doRawRequest(t, router, ep.method, ep.path, raw)
					expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
					assertBatchFixtureUnchanged(t, router, st)
				})
			}

			// Query parameters cannot rescue a non-JSON-whitespace body.
			rec := doRawRequest(t, router, ep.method, ep.path+ep.query, " ")
			expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
			assertBatchFixtureUnchanged(t, router, st)

			// The pre-existing strict contract is unchanged: no body, only
			// JSON whitespace, or an empty object are parameter errors and
			// query parameters are not merged into a batch body.
			for _, raw := range []string{"", "   ", "\t\r\n ", "{}"} {
				rec := doRawRequest(t, router, ep.method, ep.path+ep.query, raw)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
				assertBatchFixtureUnchanged(t, router, st)
			}
		})
	}
}

// assertBatchFixtureUnchanged verifies a rejected strict-batch request left
// every fixture record field intact and created no row.
func assertBatchFixtureUnchanged(t *testing.T, router http.Handler, st *store.Store) {
	t.Helper()
	got, found := publicInstance(t, router, "svc", "i-1")
	if !found {
		t.Fatalf("svc/i-1 disappeared after rejected request")
	}
	assertOriginalI1(t, got)
	if i2, found := publicInstance(t, router, "svc", "i-2"); !found || i2["weight"].(float64) != 3 {
		t.Fatalf("svc/i-2 changed after rejected request: %v found=%v", i2, found)
	}
	if i2, found := publicInstance(t, router, "svc", "i-2"); found && i2["healthy"] != true {
		t.Fatalf("svc/i-2 health changed: %v", i2)
	}
	if _, found := publicInstance(t, router, "svc", "i-3"); found {
		t.Fatalf("rejected request created svc/i-3")
	}
	if peer, found := publicInstance(t, router, "other", "i-1"); !found || peer["weight"].(float64) != 10 {
		t.Fatalf("other/i-1 changed after rejected request: %v found=%v", peer, found)
	}
	if all, err := st.ListAllInstances(); err != nil || len(all) != 3 {
		t.Fatalf("store rows = %d (err %v), want 3", len(all), err)
	}
}

// TestNonJSONWhitespaceBodyWinsOverNotFound sends non-JSON whitespace bodies
// at targets that do not exist: the body error must be reported first
// (HTTP 400 invalid_parameter), never 404. A create-capable entry must not
// create the missing instance even with every field supplied as a query
// parameter. A valid body at the same missing targets keeps returning 404
// instance_not_found.
func TestNonJSONWhitespaceBodyWinsOverNotFound(t *testing.T) {
	type ghost struct {
		name   string
		method string
		path   string
		valid  string
	}
	targets := []ghost{
		{"get", http.MethodGet, "/api/v1/services/svc/instances/ghost", `{}`},
		{"get-health", http.MethodGet, "/api/v1/services/svc/instances/ghost/health", `{}`},
		{"get-weight", http.MethodGet, "/api/v1/services/svc/instances/ghost/weight", `{}`},
		{"get-heartbeat", http.MethodGet, "/api/v1/services/svc/instances/ghost/heartbeat", `{}`},
		{"delete", http.MethodDelete, "/api/v1/services/svc/instances/ghost", `{}`},
		{"delete-post", http.MethodPost, "/api/v1/services/svc/instances/ghost/delete", `{}`},
		{"heartbeat", http.MethodPost, "/api/v1/services/svc/instances/ghost/heartbeat",
			`{"heartbeat_at":"` + integrityTime + `"}`},
		{"weight", http.MethodPut, "/api/v1/services/svc/instances/ghost/weight", `{"weight":20}`},
		{"health", http.MethodPut, "/api/v1/services/svc/instances/ghost/health", `{"healthy":true}`},
	}
	bodies := append([]string{}, nonJSONWhitespaceBodies...)
	bodies = append(bodies,
		"\u00a0"+`{"heartbeat_at":"`+integrityTime+`"}`,
		`{"weight":20}`+"　",
	)
	for _, tc := range targets {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			router, _ := testRouter(t)
			seedBodyIntegrityFixture(t, router)

			for _, raw := range bodies {
				rec := doRawRequest(t, router, tc.method, tc.path, raw)
				expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
			}
			if _, found := publicInstance(t, router, "svc", "ghost"); found {
				t.Fatalf("rejected body created/left svc/ghost")
			}

			rec := doRawRequest(t, router, tc.method, tc.path, tc.valid)
			expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
		})
	}

	// Registration with all valid query fields still cannot create a row when
	// the body is non-JSON whitespace.
	router, _ := testRouter(t)
	for _, raw := range nonJSONWhitespaceBodies {
		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?service_name=svc&instance_id=i-never&weight=10&heartbeat_at="+integrityTime,
			raw)
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-never"); found {
			t.Fatalf("non-JSON whitespace body registered i-never: %q", raw)
		}
	}
}

// TestNonJSONWhitespaceBodyWinsOverStorageUnavailable closes the store and
// sends non-JSON whitespace bodies with sufficient query parameters: the
// body error wins (400 invalid_parameter), never 503. Valid bodies on the
// closed store still report 503 storage_unavailable.
func TestNonJSONWhitespaceBodyWinsOverStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	seedBodyIntegrityFixture(t, router)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/register?service_name=svc&instance_id=i-x&weight=10&heartbeat_at=" + integrityTime},
		{http.MethodDelete, "/api/v1/services/svc/instances/i-1"},
		{http.MethodPost, "/api/v1/services/svc/instances/i-1/delete"},
		{http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at=" + integrityTime},
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=20"},
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/health?healthy=false"},
		{http.MethodGet, "/api/v1/services/svc/instances/i-1"},
		{http.MethodGet, "/api/v1/services?evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=600"},
		{http.MethodPost, "/api/v1/discover?service_name=svc&evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=600"},
		{http.MethodPost, "/api/v1/cleanup?evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=600"},
	}
	for _, tc := range cases {
		for _, raw := range []string{" ", " \t　\n", "\u2003", "\u000b"} {
			rec := doRawRequest(t, router, tc.method, tc.path, raw)
			expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		}
	}

	validCases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/register",
			`{"service_name":"svc","instance_id":"i-x","weight":10,"heartbeat_at":"` + integrityTime + `"}`},
		{http.MethodDelete, "/api/v1/services/svc/instances/i-1", `{}`},
		{http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat",
			`{"heartbeat_at":"` + integrityTime + `"}`},
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/weight", `{"weight":20}`},
		{http.MethodGet, "/api/v1/services/svc/instances/i-1", `{}`},
		{http.MethodPost, "/api/v1/cleanup", `{"evaluate_at":"2026-10-01T12:10:00Z","heartbeat_timeout":600}`},
	}
	for _, tc := range validCases {
		rec := doRawRequest(t, router, tc.method, tc.path, tc.body)
		expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	}
}

// TestNonJSONWhitespaceBodyBlocksDiscoverAndCleanupDeletion seeds an instance
// that is strictly lost at the request evaluation point. Every discovery and
// cleanup request carrying non-JSON whitespace (alone, mixed with JSON
// whitespace, or framing a valid object) must fail with 400
// invalid_parameter and leave the lost record stored; the fresh peer in
// another service must stay stored as well.
func TestNonJSONWhitespaceBodyBlocksDiscoverAndCleanupDeletion(t *testing.T) {
	eval := "2026-10-01T12:10:00Z"
	setup := func() http.Handler {
		router, _ := testRouter(t)
		gone := weightRecord()
		gone["instance_id"] = "i-gone"
		registerInstance(t, router, gone)
		fresh := weightRecord()
		fresh["instance_id"] = "i-fresh"
		fresh["heartbeat_at"] = "2026-10-01T12:09:00Z"
		registerInstance(t, router, fresh)
		peer := weightRecord()
		peer["service_name"] = "other"
		peer["instance_id"] = "i-gone"
		peer["heartbeat_at"] = "2026-10-01T12:09:00Z"
		registerInstance(t, router, peer)
		return router
	}
	object := `{"evaluate_at":"` + eval + `","heartbeat_timeout":300}`
	requests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/services/svc/discover?evaluate_at=" + eval + "&heartbeat_timeout=300"},
		{http.MethodPost, "/api/v1/services/svc/discover?evaluate_at=" + eval + "&heartbeat_timeout=300"},
		{http.MethodGet, "/api/v1/discover?service_name=svc&evaluate_at=" + eval + "&heartbeat_timeout=300"},
		{http.MethodPost, "/api/v1/discover?service_name=svc&evaluate_at=" + eval + "&heartbeat_timeout=300"},
		{http.MethodPost, "/api/v1/cleanup?evaluate_at=" + eval + "&heartbeat_timeout=300"},
	}
	bodies := append([]string{}, nonJSONWhitespaceBodies...)
	bodies = append(bodies, framingBodies(object)...)
	for _, rc := range requests {
		rc := rc
		t.Run(rc.method+" "+rc.path, func(t *testing.T) {
			for _, raw := range bodies {
				raw := raw
				t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
					router := setup()
					rec := doRawRequest(t, router, rc.method, rc.path, raw)
					expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
					if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
						t.Fatalf("lost record deleted by rejected %s body %q", rc.path, raw)
					}
					if _, found := publicInstance(t, router, "other", "i-gone"); !found {
						t.Fatalf("peer record deleted by rejected %s body %q", rc.path, raw)
					}
				})
			}
		})
	}

	// Pure JSON whitespace still means "no body": discovery and cleanup run
	// with the query parameters and the lost record is removed.
	router := setup()
	rec := doRawRequest(t, router, http.MethodPost,
		"/api/v1/cleanup?evaluate_at="+eval+"&heartbeat_timeout=300", " \t\r\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON-whitespace cleanup: %d %s", rec.Code, rec.Body.String())
	}
	if _, found := publicInstance(t, router, "svc", "i-gone"); found {
		t.Fatalf("lost record kept after a legal JSON-whitespace cleanup body")
	}
	router = setup()
	rec = doRawRequest(t, router, http.MethodPost,
		"/api/v1/discover?service_name=svc&evaluate_at="+eval+"&heartbeat_timeout=300", "  \n")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON-whitespace discover: %d %s", rec.Code, rec.Body.String())
	}
	if _, found := publicInstance(t, router, "svc", "i-gone"); found {
		t.Fatalf("lost record kept after a legal JSON-whitespace discover body")
	}
}

// TestJSONWhitespaceAndMergingRulesUnchanged proves the fix narrows only the
// whitespace rule: empty bodies, pure JSON whitespace and {} objects keep
// using query parameters on optional entries, JSON whitespace still pads a
// complete object, body fields still win over same-named query parameters,
// path locators still win over body fields, and valid requests at missing or
// unavailable stores keep 404 and 503.
func TestJSONWhitespaceAndMergingRulesUnchanged(t *testing.T) {
	t.Run("register", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())

		query := "/api/v1/register?service_name=svc&instance_id=i-q&weight=10&heartbeat_at=" + integrityTime
		for label, raw := range map[string]string{
			"empty":         "",
			"json-space":    "  \t\r\n ",
			"empty-object":  "{}",
			"padded-object": "\r\n\t " + `{"weight":10}` + " \n",
		} {
			rec := doRawRequest(t, router, http.MethodPost, query, raw)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s register: %d %s", label, rec.Code, rec.Body.String())
			}
		}
		if _, found := publicInstance(t, router, "svc", "i-q"); !found {
			t.Fatalf("query-parameter registration did not happen")
		}

		// Body weight wins over the query weight.
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register?weight=99",
			`{"service_name":"svc","instance_id":"i-bw","weight":42,"heartbeat_at":"`+integrityTime+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins register: %d %s", rec.Code, rec.Body.String())
		}
		if got, _ := publicInstance(t, router, "svc", "i-bw"); got["weight"].(float64) != 42 {
			t.Fatalf("body weight did not win: %v", got)
		}
	})

	t.Run("heartbeat and weight", func(t *testing.T) {
		router, st := testRouter(t)
		registerInstance(t, router, weightRecord())

		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at=2026-10-01T12:01:00Z",
			" \t\r\n ")
		if rec.Code != http.StatusOK {
			t.Fatalf("whitespace heartbeat: %d %s", rec.Code, rec.Body.String())
		}
		got, _, _ := st.GetInstance("svc", "i-1")
		if got.HeartbeatAt.Format("2006-01-02T15:04:05Z") != "2026-10-01T12:01:00Z" {
			t.Fatalf("query heartbeat not applied: %+v", got)
		}
		rec = doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=99",
			"\n "+`{"weight":33}`+"\r\n")
		if rec.Code != http.StatusOK {
			t.Fatalf("padded weight: %d %s", rec.Code, rec.Body.String())
		}
		if got, _, _ := st.GetInstance("svc", "i-1"); got.Weight != 33 {
			t.Fatalf("body weight did not win over query: %+v", got)
		}
	})

	t.Run("health", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())

		// Pure JSON whitespace keeps the query fallback; a JSON-wrapped object
		// is accepted; a body boolean wins over the query value.
		for _, tc := range []struct {
			body  string
			query string
			want  bool
		}{
			{" \t\r\n", "?healthy=false", false},
			{"\n " + `{"healthy":false}` + " \n", "", false},
			{`{"healthy":false}`, "?healthy=true", false},
			{`{}`, "?healthy=false", false},
		} {
			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/i-1/health"+tc.query, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("health body %q query %q: %d %s", tc.body, tc.query, rec.Code, rec.Body.String())
			}
			if got, _ := publicInstance(t, router, "svc", "i-1"); got["healthy"] != tc.want {
				t.Fatalf("healthy = %v, want %v", got["healthy"], tc.want)
			}
		}
	})

	t.Run("read-only queries", func(t *testing.T) {
		router, _ := testRouter(t)
		seedBodyIntegrityFixture(t, router)

		// Empty body on a query-style GET.
		if rec := doRawRequest(t, router, http.MethodGet,
			"/api/v1/instances?service_name=svc&instance_id=i-1", "  \n"); rec.Code != http.StatusOK {
			t.Fatalf("whitespace GET: %d %s", rec.Code, rec.Body.String())
		}
		// Body locator wins over the query locator when no path segment exists.
		rec := doRawRequest(t, router, http.MethodGet,
			"/api/v1/instances?service_name=other&instance_id=i-2",
			`{"service_name":"svc","instance_id":"i-1"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins GET: %d %s", rec.Code, rec.Body.String())
		}
		if inst := decodeBody(t, rec)["instance"].(map[string]any); inst["service_name"] != "svc" || inst["instance_id"] != "i-1" {
			t.Fatalf("body locator did not win: %v", inst)
		}
		// A path locator wins over any body/query value.
		rec = doRawRequest(t, router, http.MethodGet,
			"/api/v1/services/other/instances/i-1",
			`{"service_name":"svc","instance_id":"i-2"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("path-wins GET: %d %s", rec.Code, rec.Body.String())
		}
		if inst := decodeBody(t, rec)["instance"].(map[string]any); inst["service_name"] != "other" || inst["instance_id"] != "i-1" {
			t.Fatalf("path locator lost to body: %v", inst)
		}
	})
}

// TestSpecialWhitespaceInsideJSONStringsPreserved ensures the document-level
// whitespace rule never reaches JSON string content: the non-JSON whitespace
// characters (escaped or raw) stay legal inside string values, an address is
// stored byte-for-byte including ordinary spaces, and locator fields still
// trim leading and trailing whitespace from their decoded string values.
func TestSpecialWhitespaceInsideJSONStringsPreserved(t *testing.T) {
	router, _ := testRouter(t)

	// U+000B and U+000C must appear as \uXXXX escapes inside a JSON string;
	// the others are raw UTF-8. Every character must come back verbatim.
	wantAddress := "a\u000b\u000c  　b"
	rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register",
		`{"service_name":"svc","instance_id":"i-special",`+
			`"address":"a\u000b\u000c  　b","weight":10,`+
			`"heartbeat_at":"`+integrityTime+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("special-whitespace address register: %d %s", rec.Code, rec.Body.String())
	}
	got, found := publicInstance(t, router, "svc", "i-special")
	if !found {
		t.Fatalf("i-special not registered")
	}
	if got["address"] != wantAddress {
		t.Fatalf("address mismatch: got %q want %q", got["address"], wantAddress)
	}

	// Ordinary leading/trailing spaces inside the address string are stored
	// verbatim: only locator fields trim, the address does not.
	rec = doRawRequest(t, router, http.MethodPost, "/api/v1/register",
		`{"service_name":"svc","instance_id":"i-spaced","address":"  10.0.0.9:9009  ",`+
			`"weight":10,"heartbeat_at":"`+integrityTime+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("spaced address register: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := publicInstance(t, router, "svc", "i-spaced"); got["address"] != "  10.0.0.9:9009  " {
		t.Fatalf("address was trimmed: %q", got["address"])
	}

	// Locator fields keep trimming decoded string values; escaped tabs inside
	// the JSON string are ordinary string content until that trim runs.
	rec = doRawRequest(t, router, http.MethodPost, "/api/v1/register",
		`{"service_name":" svc ","instance_id":"\ti-1\t","weight":11,`+
			`"heartbeat_at":"`+integrityTime+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("trimmed locator register: %d %s", rec.Code, rec.Body.String())
	}
	if existing, _ := publicInstance(t, router, "svc", "i-1"); existing["weight"].(float64) != 11 {
		t.Fatalf("trimmed locator did not resolve to svc/i-1: %v", existing)
	}
}

// TestSingleHealthEntryRejectsNonJSONWhitespace explicitly pins the
// readHealthyInput gate: a non-JSON whitespace body must not fall through to
// the healthy query parameter, and must not modify the stored health state.
func TestSingleHealthEntryRejectsNonJSONWhitespace(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())

	for _, raw := range nonJSONWhitespaceBodies {
		rec := doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/health?healthy=false", raw)
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if got, _ := publicInstance(t, router, "svc", "i-1"); got["healthy"] != true {
			t.Fatalf("health changed after body %q: %v", raw, got["healthy"])
		}
	}

	// A non-JSON byte framing a valid object is rejected too.
	rec := doRawRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health?healthy=false",
		"\u00a0"+`{"healthy":false}`)
	expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
	if got, _ := publicInstance(t, router, "svc", "i-1"); got["healthy"] != true {
		t.Fatalf("health changed after prefixed object: %v", got["healthy"])
	}

	// Missing healthy in both body and query is still a parameter error after
	// a JSON-whitespace-only body.
	rec = doRawRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health", " \t\r\n")
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
}
