package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// Bodies made only of whitespace that Go's bytes.TrimSpace strips but JSON
// does not accept outside a string (plus mixes with the four legal JSON
// whitespace bytes). Before the fix these were mistaken for an absent body,
// so an entry with sufficient query parameters would execute the request.
var nonJSONWhitespaceOnlyBodies = []struct {
	name string
	body string
}{
	{"vertical-tab", "\u000b"},
	{"form-feed", "\u000c"},
	{"no-break-space", "\u00a0"},
	{"em-space", " "},
	{"ideographic-space", "　"},
	{"vt-mixed-with-json-ws", " \t\r\n\u000b"},
	{"ff-mixed-with-json-ws", "\u000c\n\t "},
	{"nbsp-mixed-with-json-ws", "\u00a0\r\n "},
	{"em-mixed-with-json-ws", " \t "},
	{"all-special-mixed", "　\u00a0\u000b\u000c "},
}

// illegalPaddingCases wraps an otherwise valid object with non-JSON
// whitespace before, after or on both sides. Such padding must never be
// ignored: the request is a body error even though the object itself is
// complete and valid.
func illegalPaddingCases(object string) []struct {
	name string
	body string
} {
	return []struct {
		name string
		body string
	}{
		{"leading-vertical-tab", "\u000b" + object},
		{"trailing-form-feed", object + "\u000c"},
		{"leading-no-break-space-among-json-ws", " \t\r\n\u00a0" + object},
		{"trailing-no-break-space-among-json-ws", object + "\u00a0 \r\n"},
		{"leading-em-space", " " + object},
		{"trailing-ideographic-space", object + "　"},
		{"special-on-both-sides", "　" + object + " "},
	}
}

const (
	whitespaceEval  = "2026-10-01T12:10:00Z"
	whitespaceFresh = "2026-10-01T12:09:00Z"
)

// seedWhitespaceFixture stores svc/i-1 (12:00), svc/i-2, other/i-1, a lost
// svc/i-gone (12:00, which discovery/cleanup at 12:10/300s would delete) and
// a fresh other/i-gone peer. A rejected request must leave every row
// byte-identical, including the lost one.
func seedWhitespaceFixture(t *testing.T, router http.Handler) {
	t.Helper()
	seedBodyIntegrityFixture(t, router)
	registerInstance(t, router, map[string]any{
		"service_name": "svc",
		"instance_id":  "i-gone",
		"address":      "10.0.0.7:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       4,
		"heartbeat_at": baseTime,
	})
	registerInstance(t, router, map[string]any{
		"service_name": "other",
		"instance_id":  "i-gone",
		"address":      "10.0.0.9:8080",
		"port":         8080,
		"healthy":      true,
		"weight":       6,
		"heartbeat_at": whitespaceFresh,
	})
}

// Every public entry that reads a JSON body and its aliases. target carries
// query parameters sufficient to complete the request on its own, so a body
// wrongly treated as absent would make the request succeed. object is a
// complete valid body used by the padding cases.
func nonJSONWhitespaceEntries() []struct {
	name   string
	method string
	target string
	object string
} {
	registerQuery := "?service_name=svc&instance_id=i-new&weight=10&heartbeat_at=" + baseTime
	locatorQuery := "?service_name=svc&instance_id=i-1"
	overviewQuery := "?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=600"
	discoverQuery := "?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"
	fullDiscoverQuery := "?service_name=svc&evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"
	registerObject := `{"service_name":"svc","instance_id":"i-pad","address":"10.0.0.8:8080",` +
		`"port":8080,"healthy":true,"weight":10,"heartbeat_at":"` + baseTime + `"}`
	overwriteObject := `{"service_name":"svc","instance_id":"i-1","weight":10,` +
		`"heartbeat_at":"` + whitespaceFresh + `"}`
	locatorObject := `{"service_name":"svc","instance_id":"i-1"}`
	timeObject := `{"evaluate_at":"` + whitespaceEval + `","heartbeat_timeout":300}`

	return []struct {
		name   string
		method string
		target string
		object string
	}{
		{"register-collection", http.MethodPost, "/api/v1/services/svc/instances?instance_id=i-new&weight=10&heartbeat_at=" + baseTime, registerObject},
		{"register-put-overwrite", http.MethodPut, "/api/v1/services/svc/instances/i-1?weight=10&heartbeat_at=" + whitespaceFresh, overwriteObject},
		{"register", http.MethodPost, "/api/v1/register" + registerQuery, registerObject},
		{"instances-post", http.MethodPost, "/api/v1/instances" + registerQuery, registerObject},
		{"instances-put", http.MethodPut, "/api/v1/instances" + registerQuery, registerObject},

		{"get-instance", http.MethodGet, "/api/v1/services/svc/instances/i-1", locatorObject},
		{"get-instance-query", http.MethodGet, "/api/v1/instances" + locatorQuery, locatorObject},
		{"get-health", http.MethodGet, "/api/v1/services/svc/instances/i-1/health", locatorObject},
		{"get-health-query", http.MethodGet, "/api/v1/instances/health" + locatorQuery, locatorObject},
		{"get-weight", http.MethodGet, "/api/v1/services/svc/instances/i-1/weight", locatorObject},
		{"get-weight-query", http.MethodGet, "/api/v1/instances/weight" + locatorQuery, locatorObject},
		{"get-heartbeat", http.MethodGet, "/api/v1/services/svc/instances/i-1/heartbeat", locatorObject},
		{"get-heartbeat-query", http.MethodGet, "/api/v1/instances/heartbeat" + locatorQuery, locatorObject},
		{"list-instances", http.MethodGet, "/api/v1/services/svc/instances", `{"service_name":"svc"}`},
		{"list-instances-query", http.MethodGet, "/api/v1/list?service_name=svc", `{"service_name":"svc"}`},
		{"services-overview", http.MethodGet, "/api/v1/services" + overviewQuery, timeObject},

		{"delete", http.MethodDelete, "/api/v1/services/svc/instances/i-1", locatorObject},
		{"delete-post-alias", http.MethodPost, "/api/v1/services/svc/instances/i-1/delete", locatorObject},
		{"delete-query", http.MethodDelete, "/api/v1/instances" + locatorQuery, locatorObject},
		{"deregister", http.MethodPost, "/api/v1/deregister" + locatorQuery, locatorObject},

		{"heartbeat", http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at=" + whitespaceFresh,
			`{"heartbeat_at":"` + whitespaceFresh + `"}`},
		{"update-weight", http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=41", `{"weight":41}`},
		{"update-health", http.MethodPut, "/api/v1/services/svc/instances/i-1/health?healthy=false", `{"healthy":false}`},

		{"discover-get", http.MethodGet, "/api/v1/services/svc/discover" + discoverQuery, timeObject},
		{"discover-post", http.MethodPost, "/api/v1/services/svc/discover" + discoverQuery, timeObject},
		{"discover-query-get", http.MethodGet, "/api/v1/discover" + fullDiscoverQuery, timeObject},
		{"discover-query-post", http.MethodPost, "/api/v1/discover" + fullDiscoverQuery, timeObject},
		{"cleanup", http.MethodPost, "/api/v1/cleanup" + overviewQuery, timeObject},
		{"cleanup-scoped", http.MethodPost, "/api/v1/cleanup?service_name=svc&evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300",
			`{"service_name":"svc","evaluate_at":"` + whitespaceEval + `","heartbeat_timeout":300}`},
	}
}

// The body-required batch entries. Every object is complete and valid and
// would mutate (or, for batch discover, delete svc/i-gone) on its own.
func nonJSONWhitespaceBatches() []struct {
	name   string
	method string
	target string
	object string
} {
	healthObject := `{"updates":[{"instance_id":"i-1","healthy":false},{"instance_id":"i-2","healthy":true}]}`
	heartbeatObject := `{"service_name":"svc","instances":[` +
		`{"instance_id":"i-1","heartbeat_at":"` + whitespaceFresh + `"},` +
		`{"instance_id":"i-2","heartbeat_at":"2026-10-01T12:08:00Z"}]}`
	discoverObject := `{"service_names":["svc"],"evaluate_at":"` + whitespaceEval + `","heartbeat_timeout":300}`
	return []struct {
		name   string
		method string
		target string
		object string
	}{
		{"batch-register-path", http.MethodPost, "/api/v1/services/svc/instances/batch", registerTrailerObjectByPath},
		{"batch-register-body", http.MethodPost, "/api/v1/register/batch?service_name=svc", registerTrailerObjectByBody},
		{"batch-weight", http.MethodPut, "/api/v1/services/svc/instances/weight", weightTrailerObject},
		{"batch-health", http.MethodPut, "/api/v1/services/svc/instances/health", healthObject},
		{"batch-heartbeat", http.MethodPost, "/api/v1/heartbeat?service_name=svc", heartbeatObject},
		{"batch-discover", http.MethodPost, "/api/v1/discover/batch", discoverObject},
	}
}

// TestNonJSONWhitespaceOnlyBodiesRejected drives every body-reading public
// entry (path and query-parameter styles, every alias, and every body
// -required batch entry) with a body consisting solely of non-JSON
// whitespace. Even though the query parameters complete each request (and,
// for discovery/cleanup, would delete the lost record), every response must
// be exactly the 400 invalid_parameter error object with no row created,
// overwritten, renewed, modified or deleted.
func TestNonJSONWhitespaceOnlyBodiesRejected(t *testing.T) {
	type reqCase struct {
		name   string
		method string
		target string
	}
	var cases []reqCase
	for _, ep := range nonJSONWhitespaceEntries() {
		cases = append(cases, reqCase{ep.name, ep.method, ep.target})
	}
	for _, ep := range nonJSONWhitespaceBatches() {
		cases = append(cases, reqCase{ep.name, ep.method, ep.target})
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			seedWhitespaceFixture(t, router)
			before := snapshotRows(t, st)

			for _, payload := range nonJSONWhitespaceOnlyBodies {
				payload := payload
				t.Run(payload.name, func(t *testing.T) {
					rec := doRawRequest(t, router, tc.method, tc.target, payload.body)
					expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")

					after := snapshotRows(t, st)
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("records changed after body %q on %s %s:\nbefore %+v\nafter  %+v",
							payload.body, tc.method, tc.target, before, after)
					}
				})
			}

			// The records the mutating variants would have touched are
			// independently re-checked field by field.
			got, found := publicInstance(t, router, "svc", "i-1")
			if !found {
				t.Fatalf("svc/i-1 disappeared")
			}
			assertOriginalI1(t, got)
			if i2, found := publicInstance(t, router, "svc", "i-2"); !found || i2["weight"].(float64) != 3 {
				t.Fatalf("svc/i-2 changed: %v found=%v", i2, found)
			}
			if _, found := publicInstance(t, router, "svc", "i-new"); found {
				t.Fatalf("register entry created svc/i-new from query parameters")
			}
			if _, found := publicInstance(t, router, "svc", "i-pad"); found {
				t.Fatalf("register entry created svc/i-pad")
			}
			if _, found := publicInstance(t, router, "svc", "i-3"); found {
				t.Fatalf("batch register created svc/i-3")
			}
			if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
				t.Fatalf("discovery/cleanup deleted svc/i-gone")
			}
			if peer, found := publicInstance(t, router, "other", "i-gone"); !found ||
				peer["heartbeat_at"] != whitespaceFresh {
				t.Fatalf("other-service peer changed: %v found=%v", peer, found)
			}
		})
	}
}

// TestNonJSONWhitespaceAroundValidObjectsRejected proves non-JSON whitespace
// immediately before or after an otherwise complete object is not ignored:
// representative create/update/renew/delete entries, the read-only query
// entries and every batch entry all reject with 400 invalid_parameter and
// leave the stored rows unchanged. Plain JSON whitespace wrapping the same
// objects stays acceptable.
func TestNonJSONWhitespaceAroundValidObjectsRejected(t *testing.T) {
	type objCase struct {
		name   string
		method string
		target string
		object string
	}
	var cases []objCase
	for _, ep := range nonJSONWhitespaceEntries() {
		cases = append(cases, objCase{ep.name, ep.method, ep.target, ep.object})
	}
	for _, ep := range nonJSONWhitespaceBatches() {
		cases = append(cases, objCase{ep.name, ep.method, ep.target, ep.object})
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for _, payload := range illegalPaddingCases(tc.object) {
				payload := payload
				t.Run(payload.name, func(t *testing.T) {
					router, st := testRouter(t)
					seedWhitespaceFixture(t, router)
					before := snapshotRows(t, st)

					rec := doRawRequest(t, router, tc.method, tc.target, payload.body)
					expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")

					if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
						t.Fatalf("records changed after padded body %q:\nbefore %+v\nafter  %+v",
							payload.body, before, after)
					}
				})
			}

			// The positive control: only JSON whitespace around the same
			// complete object is accepted and the request executes normally.
			router, _ := testRouter(t)
			seedWhitespaceFixture(t, router)
			rec := doRawRequest(t, router, tc.method, tc.target, "\r\n\t "+tc.object+" \n")
			if rec.Code != http.StatusOK {
				t.Fatalf("JSON-whitespace-wrapped object rejected on %s %s: %d %s",
					tc.method, tc.target, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestNonJSONWhitespaceInsideJSONStringsAllowed verifies the special
// characters remain legal inside JSON string values: an address containing
// them is stored byte-for-byte, the same works through batch registration,
// and a locator value keeps its existing trim-leading/trailing-whitespace
// semantics. A scoped cleanup whose service name merely contains such a
// character matches no service and deletes nothing.
func TestNonJSONWhitespaceInsideJSONStringsAllowed(t *testing.T) {
	wantAddress := "10.0.0.8:80\u000bx\u000cy\u00a0z q　r"

	t.Run("single register preserves address", func(t *testing.T) {
		router, _ := testRouter(t)
		body := `{"service_name":"svc","instance_id":"i-sp","address":"10.0.0.8:80\u000bx\u000cy\u00a0z q　r",` +
			`"port":8080,"healthy":true,"weight":10,"heartbeat_at":"` + baseTime + `"}`
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("register with special whitespace in address: %d %s", rec.Code, rec.Body.String())
		}
		got, found := publicInstance(t, router, "svc", "i-sp")
		if !found {
			t.Fatalf("i-sp not registered")
		}
		if got["address"] != wantAddress {
			t.Fatalf("address not preserved verbatim: %q want %q", got["address"], wantAddress)
		}
	})

	t.Run("batch register preserves address", func(t *testing.T) {
		router, _ := testRouter(t)
		entry := `{"instance_id":"i-spb","address":"10.0.0.8:80\u000bx\u000cy\u00a0z q　r",` +
			`"port":8080,"healthy":true,"weight":10,"heartbeat_at":"` + baseTime + `"}`
		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/batch", `{"instances":[`+entry+`]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch register with special whitespace in address: %d %s", rec.Code, rec.Body.String())
		}
		got, found := publicInstance(t, router, "svc", "i-spb")
		if !found {
			t.Fatalf("i-spb not registered")
		}
		if got["address"] != wantAddress {
			t.Fatalf("address not preserved verbatim: %q want %q", got["address"], wantAddress)
		}
	})

	t.Run("locator trimming semantics unchanged", func(t *testing.T) {
		router, _ := testRouter(t)
		// Leading/trailing whitespace (including U+00A0 and U+3000 and a
		// tab inside the string) is trimmed; the resulting id is "i-trim".
		body := `{"service_name":"svc","instance_id":"\u00a0 i-trim \t　","weight":10,` +
			`"heartbeat_at":"` + baseTime + `"}`
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("register with padded locator: %d %s", rec.Code, rec.Body.String())
		}
		if _, found := publicInstance(t, router, "svc", "i-trim"); !found {
			t.Fatalf("instance not stored under its trimmed id")
		}
	})

	t.Run("special character inside scoped service name matches nothing", func(t *testing.T) {
		router, _ := testRouter(t)
		seedWhitespaceFixture(t, router)
		// A special character embedded inside the name (not at an edge, so it
		// is not trimmed) is ordinary string content: it must match no
		// service and the cleanup deletes nothing.
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/cleanup",
			`{"service_name":"sv\u000bc","evaluate_at":"`+whitespaceEval+`","heartbeat_timeout":300}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("scoped cleanup: %d %s", rec.Code, rec.Body.String())
		}
		if removed := decodeBody(t, rec)["removed"].(float64); removed != 0 {
			t.Fatalf("removed = %v, want 0", removed)
		}
		if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
			t.Fatalf("cleanup removed records despite unmatched scope")
		}
	})
}

// TestNonJSONWhitespaceBodyPrecedesNotFound sends the illegal bodies at
// targets that do not exist, with query parameters sufficient on their own.
// Body validation must win: 400 invalid_parameter, never 404. The same
// targets with an absent body keep returning 404 instance_not_found.
func TestNonJSONWhitespaceBodyPrecedesNotFound(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
	}{
		{"get", http.MethodGet, "/api/v1/services/svc/instances/ghost"},
		{"get-health", http.MethodGet, "/api/v1/services/svc/instances/ghost/health"},
		{"get-weight", http.MethodGet, "/api/v1/services/svc/instances/ghost/weight"},
		{"get-heartbeat", http.MethodGet, "/api/v1/services/svc/instances/ghost/heartbeat"},
		{"delete", http.MethodDelete, "/api/v1/services/svc/instances/ghost"},
		{"delete-post", http.MethodPost, "/api/v1/services/svc/instances/ghost/delete"},
		{"heartbeat", http.MethodPost, "/api/v1/services/svc/instances/ghost/heartbeat?heartbeat_at=" + whitespaceFresh},
		{"weight", http.MethodPut, "/api/v1/services/svc/instances/ghost/weight?weight=5"},
		{"health", http.MethodPut, "/api/v1/services/svc/instances/ghost/health?healthy=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			seedWhitespaceFixture(t, router)
			before := snapshotRows(t, st)

			for _, payload := range nonJSONWhitespaceOnlyBodies {
				rec := doRawRequest(t, router, tc.method, tc.target, payload.body)
				expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
			}
			if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
				t.Fatalf("records changed:\nbefore %+v\nafter  %+v", before, after)
			}

			// An absent body at the same missing target keeps the 404.
			rec := doRawRequest(t, router, tc.method, tc.target, "")
			expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
		})
	}
}

// TestNonJSONWhitespaceBodyPrecedesStorageUnavailable closes the store and
// sends illegal-whitespace bodies with complete query parameters. The body
// error wins over storage access (400, never 503); valid bodies at the same
// closed store still report 503 storage_unavailable.
func TestNonJSONWhitespaceBodyPrecedesStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	seedWhitespaceFixture(t, router)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	illegalCases := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/api/v1/register?service_name=svc&instance_id=i-new&weight=10&heartbeat_at=" + baseTime},
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=5"},
		{http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at=" + whitespaceFresh},
		{http.MethodDelete, "/api/v1/services/svc/instances/i-1"},
		{http.MethodGet, "/api/v1/services?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=600"},
		{http.MethodPost, "/api/v1/discover?service_name=svc&evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"},
		{http.MethodPost, "/api/v1/cleanup?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"},
		{http.MethodPost, "/api/v1/services/svc/instances/batch"},
		{http.MethodPut, "/api/v1/services/svc/instances/weight"},
		{http.MethodPost, "/api/v1/discover/batch"},
	}
	for _, tc := range illegalCases {
		for _, payload := range nonJSONWhitespaceOnlyBodies {
			rec := doRawRequest(t, router, tc.method, tc.target, payload.body)
			expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		}
	}

	// Valid bodies still reach the store and yield storage_unavailable.
	validCases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPost, "/api/v1/register",
			`{"service_name":"svc","instance_id":"i-1","weight":10,"heartbeat_at":"` + baseTime + `"}`},
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/weight", `{"weight":20}`},
		{http.MethodPost, "/api/v1/discover",
			`{"service_name":"svc","evaluate_at":"` + whitespaceEval + `","heartbeat_timeout":300}`},
		{http.MethodPost, "/api/v1/cleanup",
			`{"evaluate_at":"` + whitespaceEval + `","heartbeat_timeout":300}`},
		{http.MethodPost, "/api/v1/services/svc/instances/batch",
			`{"instances":[{"instance_id":"i-1","weight":10,"heartbeat_at":"` + baseTime + `"}]}`},
	}
	for _, tc := range validCases {
		rec := doRawRequest(t, router, tc.method, tc.target, tc.body)
		expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	}
}

// TestOptionalBodyRulesAndPrioritiesUnchanged restates the existing body
// contract after the fix: absent, JSON-whitespace-only and empty-object
// bodies keep the query-parameter rules, JSON whitespace wrapping an object
// is accepted, body fields win over query parameters, path locators win
// over body fields, and body-required batch entries reject absent or
// whitespace-only bodies without merging query parameters.
func TestOptionalBodyRulesAndPrioritiesUnchanged(t *testing.T) {
	t.Run("optional-body entries fall back to query parameters", func(t *testing.T) {
		router, _ := testRouter(t)
		seedWhitespaceFixture(t, router)

		ok := func(label, method, target, body string) {
			t.Helper()
			rec := doRawRequest(t, router, method, target, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s body=%q: %d %s", label, body, rec.Code, rec.Body.String())
			}
		}
		for _, body := range []string{"", " \t\r\n", "{}"} {
			ok("register", http.MethodPost,
				"/api/v1/register?service_name=svc&instance_id=i-fb-"+fmt.Sprintf("%d", len(body))+
					"&weight=10&heartbeat_at="+baseTime, body)
			ok("get", http.MethodGet, "/api/v1/services/svc/instances/i-1", body)
			ok("weight", http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=33", body)
			ok("heartbeat", http.MethodPost,
				"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+whitespaceFresh, body)
		}
		// Delete and the discovery/cleanup entries follow the same fallback.
		for _, style := range []struct {
			method string
			path   string
		}{
			{http.MethodDelete, "/api/v1/services/svc/instances/i-2"},
			{http.MethodPost, "/api/v1/services/svc/instances/i-2/delete"},
		} {
			for _, body := range []string{"", "\r\n ", "{}"} {
				router2, _ := testRouter(t)
				registerInstance(t, router2, weightRecord())
				second := weightRecord()
				second["instance_id"] = "i-2"
				second["weight"] = 3
				registerInstance(t, router2, second)
				rec := doRawRequest(t, router2, style.method, style.path, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("delete body=%q: %d %s", body, rec.Code, rec.Body.String())
				}
				if _, found := publicInstance(t, router2, "svc", "i-2"); found {
					t.Fatalf("delete body=%q did not remove i-2", body)
				}
			}
		}
		discoverQuery := "/api/v1/services/svc/discover?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"
		cleanupQuery := "/api/v1/cleanup?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"
		for _, tc := range []struct {
			method string
			target string
		}{
			{http.MethodGet, discoverQuery},
			{http.MethodPost, discoverQuery},
			{http.MethodPost, cleanupQuery},
		} {
			for _, body := range []string{"", "  \t\r\n", "{}"} {
				router2, _ := testRouter(t)
				registerInstance(t, router2, map[string]any{
					"service_name": "svc", "instance_id": "i-gone", "weight": 4,
					"healthy": true, "heartbeat_at": baseTime,
				})
				rec := doRawRequest(t, router2, tc.method, tc.target, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("fallback %s %s body=%q: %d %s",
						tc.method, tc.target, body, rec.Code, rec.Body.String())
				}
				if _, found := publicInstance(t, router2, "svc", "i-gone"); found {
					t.Fatalf("lost record not cleaned up for fallback body=%q", body)
				}
			}
		}
	})

	t.Run("json field wins over query parameter", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())

		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register?weight=99",
			`{"service_name":"svc","instance_id":"i-bw","weight":10,"heartbeat_at":"`+baseTime+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
		}
		if got, _ := publicInstance(t, router, "svc", "i-bw"); got["weight"].(float64) != 10 {
			t.Fatalf("query weight overrode body weight: %v", got)
		}
		rec = doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=99", `{"weight":31}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("weight update: %d %s", rec.Code, rec.Body.String())
		}
		if got, _ := publicInstance(t, router, "svc", "i-1"); got["weight"].(float64) != 31 {
			t.Fatalf("query weight overrode body weight: %v", got)
		}
	})

	t.Run("path locator wins over body fields", func(t *testing.T) {
		router, _ := testRouter(t)
		seedWhitespaceFixture(t, router)
		rec := doRawRequest(t, router, http.MethodGet,
			"/api/v1/services/other/instances/i-gone",
			`{"service_name":"svc","instance_id":"i-1"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("path-located get: %d %s", rec.Code, rec.Body.String())
		}
		inst := decodeBody(t, rec)["instance"].(map[string]any)
		if inst["service_name"] != "other" || inst["instance_id"] != "i-gone" {
			t.Fatalf("body fields overrode path locator: %v", inst)
		}
	})

	t.Run("batch entries never merge query parameters", func(t *testing.T) {
		router, st := testRouter(t)
		seedWhitespaceFixture(t, router)
		before := snapshotRows(t, st)

		cases := []struct {
			method string
			target string
		}{
			{http.MethodPost, "/api/v1/register/batch?service_name=svc"},
			{http.MethodPost, "/api/v1/services/svc/instances/batch"},
			{http.MethodPut, "/api/v1/services/svc/instances/weight?weight=5"},
			{http.MethodPut, "/api/v1/services/svc/instances/health?healthy=false"},
			{http.MethodPost, "/api/v1/heartbeat?service_name=svc"},
			{http.MethodPost, "/api/v1/discover/batch?evaluate_at=" + whitespaceEval + "&heartbeat_timeout=300"},
		}
		for _, tc := range cases {
			for _, body := range []string{"", " \t\r\n"} {
				rec := doRawRequest(t, router, tc.method, tc.target, body)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			}
		}
		if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
			t.Fatalf("batch fallback changed records:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}
