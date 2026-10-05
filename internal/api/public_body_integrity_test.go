package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// errTestBodyRead is returned by a request body whose read always fails, so
// the "body cannot be read" branch can be exercised over real HTTP entries.
var errTestBodyRead = errors.New("simulated body read failure")

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errTestBodyRead }

// expectOnlyTopLevelError asserts the published error contract: exactly one
// top-level key ("error"), whose object carries exactly the two string
// fields code and message, with a non-empty message that leaks no SQL,
// stack frame or file path.
func expectOnlyTopLevelError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	expectErrorShape(t, rec, wantStatus, wantCode)
	out := decodeBody(t, rec)
	if len(out) != 1 {
		t.Fatalf("response has %d top-level keys, want only error: %s", len(out), rec.Body.String())
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("error is not an object: %s", rec.Body.String())
	}
	if len(errObj) != 2 {
		t.Fatalf("error object has %d keys, want code and message only: %s", len(errObj), rec.Body.String())
	}
	code, _ := errObj["code"].(string)
	message, _ := errObj["message"].(string)
	if code != wantCode || message == "" {
		t.Fatalf("unexpected error fields: %v", errObj)
	}
}

func seedBodyIntegrityFixture(t *testing.T, router http.Handler) {
	t.Helper()
	registerInstance(t, router, weightRecord())
	second := weightRecord()
	second["instance_id"] = "i-2"
	second["weight"] = 3
	registerInstance(t, router, second)
	other := weightRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)
}

func snapshotRows(t *testing.T, st *store.Store) []store.Instance {
	t.Helper()
	rows, err := st.ListAllInstances()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ServiceName != rows[j].ServiceName {
			return rows[i].ServiceName < rows[j].ServiceName
		}
		return rows[i].InstanceID < rows[j].InstanceID
	})
	return rows
}

// bodyIntegrityEntry describes one public entry through both public styles:
// pathTarget addresses the target through path locators, altTarget through
// the query-parameter style (or the same path entry with the remaining
// fields in the query string). object is a complete, otherwise-valid body.
type bodyIntegrityEntry struct {
	name       string
	method     string
	pathTarget string
	altTarget  string
	// extraTargets lists additional published aliases (possibly with another
	// HTTP method) that must honor the same body contract.
	extraTargets []bodyIntegrityTarget
	object       string
}

type bodyIntegrityTarget struct {
	method string
	path   string
}

func target(method, path string) bodyIntegrityTarget {
	return bodyIntegrityTarget{method, path}
}

const integrityTime = "2026-10-01T12:00:00Z"

func integrityQueryTarget(base string, params url.Values) string {
	if len(params) == 0 {
		return base
	}
	return base + "?" + params.Encode()
}

func locatorParams() url.Values {
	return url.Values{
		"service_name": {"svc"},
		"instance_id":  {"i-1"},
	}
}

func bodyIntegrityEntries() []bodyIntegrityEntry {
	registerObject := `{"service_name":"svc","instance_id":"i-1",` +
		`"address":"10.0.0.8:8080","port":8080,"healthy":true,` +
		`"weight":10,"heartbeat_at":"` + integrityTime + `"}`
	registerQuery := url.Values{
		"service_name": {"svc"},
		"instance_id":  {"i-1"},
		"weight":       {"10"},
		"heartbeat_at": {integrityTime},
	}
	locatorObject := `{"service_name":"svc","instance_id":"i-1"}`
	timeObject := `{"evaluate_at":"2026-10-01T12:10:00Z","heartbeat_timeout":600}`
	overviewQuery := url.Values{
		"evaluate_at":       {"2026-10-01T12:10:00Z"},
		"heartbeat_timeout": {"600"},
	}
	discoverQuery := url.Values{
		"service_name":      {"svc"},
		"evaluate_at":       {"2026-10-01T12:10:00Z"},
		"heartbeat_timeout": {"600"},
	}
	pathDiscoverQuery := url.Values{
		"evaluate_at":       {"2026-10-01T12:10:00Z"},
		"heartbeat_timeout": {"600"},
	}
	heartbeatQuery := url.Values{"heartbeat_at": {integrityTime}}
	weightQuery := url.Values{"weight": {"20"}}
	batchHeartbeatObject := `{"service_name":"svc","instances":[` +
		`{"instance_id":"i-1","heartbeat_at":"` + integrityTime + `"}]}`

	return []bodyIntegrityEntry{
		{
			name:       "register-collection",
			method:     http.MethodPost,
			pathTarget: "/api/v1/services/svc/instances",
			altTarget:  integrityQueryTarget("/api/v1/register", registerQuery),
			extraTargets: []bodyIntegrityTarget{
				target(http.MethodPost, integrityQueryTarget("/api/v1/instances", registerQuery)),
			},
			object: registerObject,
		},
		{
			name:       "register-put",
			method:     http.MethodPut,
			pathTarget: "/api/v1/services/svc/instances/i-1",
			altTarget:  integrityQueryTarget("/api/v1/instances", registerQuery),
			object:     registerObject,
		},
		{
			name:       "get-instance",
			method:     http.MethodGet,
			pathTarget: "/api/v1/services/svc/instances/i-1",
			altTarget:  integrityQueryTarget("/api/v1/instances", locatorParams()),
			object:     locatorObject,
		},
		{
			name:       "get-health",
			method:     http.MethodGet,
			pathTarget: "/api/v1/services/svc/instances/i-1/health",
			altTarget:  integrityQueryTarget("/api/v1/instances/health", locatorParams()),
			object:     locatorObject,
		},
		{
			name:       "get-weight",
			method:     http.MethodGet,
			pathTarget: "/api/v1/services/svc/instances/i-1/weight",
			altTarget:  integrityQueryTarget("/api/v1/instances/weight", locatorParams()),
			object:     locatorObject,
		},
		{
			name:       "get-heartbeat",
			method:     http.MethodGet,
			pathTarget: "/api/v1/services/svc/instances/i-1/heartbeat",
			altTarget:  integrityQueryTarget("/api/v1/instances/heartbeat", locatorParams()),
			object:     locatorObject,
		},
		{
			name:       "list-instances",
			method:     http.MethodGet,
			pathTarget: "/api/v1/services/svc/instances",
			altTarget:  integrityQueryTarget("/api/v1/list", url.Values{"service_name": {"svc"}}),
			object:     `{"service_name":"svc"}`,
		},
		{
			name:       "services-overview",
			method:     http.MethodGet,
			pathTarget: integrityQueryTarget("/api/v1/services", overviewQuery),
			altTarget:  integrityQueryTarget("/api/v1/services", overviewQuery),
			object:     timeObject,
		},
		{
			name:       "delete",
			method:     http.MethodDelete,
			pathTarget: "/api/v1/services/svc/instances/i-1",
			altTarget:  integrityQueryTarget("/api/v1/instances", locatorParams()),
			extraTargets: []bodyIntegrityTarget{
				target(http.MethodPost, "/api/v1/services/svc/instances/i-1/delete"),
				target(http.MethodPost, integrityQueryTarget("/api/v1/deregister", locatorParams())),
			},
			object: locatorObject,
		},
		{
			name:       "heartbeat",
			method:     http.MethodPost,
			pathTarget: integrityQueryTarget("/api/v1/services/svc/instances/i-1/heartbeat", heartbeatQuery),
			altTarget:  integrityQueryTarget("/api/v1/services/svc/instances/i-1/heartbeat", heartbeatQuery),
			object:     `{"heartbeat_at":"` + integrityTime + `"}`,
		},
		{
			name:       "batch-heartbeat",
			method:     http.MethodPost,
			pathTarget: "/api/v1/heartbeat",
			altTarget:  "/api/v1/heartbeat",
			object:     batchHeartbeatObject,
		},
		{
			name:       "update-weight",
			method:     http.MethodPut,
			pathTarget: integrityQueryTarget("/api/v1/services/svc/instances/i-1/weight", weightQuery),
			altTarget:  integrityQueryTarget("/api/v1/services/svc/instances/i-1/weight", weightQuery),
			object:     `{"weight":20}`,
		},
		{
			name:       "discover",
			method:     http.MethodPost,
			pathTarget: integrityQueryTarget("/api/v1/services/svc/discover", pathDiscoverQuery),
			altTarget:  integrityQueryTarget("/api/v1/discover", discoverQuery),
			extraTargets: []bodyIntegrityTarget{
				target(http.MethodGet, integrityQueryTarget("/api/v1/services/svc/discover", pathDiscoverQuery)),
				target(http.MethodGet, integrityQueryTarget("/api/v1/discover", discoverQuery)),
			},
			object: timeObject,
		},
		{
			name:       "cleanup",
			method:     http.MethodPost,
			pathTarget: integrityQueryTarget("/api/v1/cleanup", overviewQuery),
			altTarget:  integrityQueryTarget("/api/v1/cleanup", overviewQuery),
			object:     timeObject,
		},
	}
}

// malformedBodies lists payloads that are not one complete JSON object:
// null, arrays and other bare values, truncated objects and plain garbage.
var malformedBodies = []string{
	"null", "[]", "[{}]", "123", "8.5", "true", "false", `"text"`,
	"{", "}", "]", `{"weight"`, `{"weight":`, "{bad", "junk", "nul",
	`{"a":}`, `{"a":"b"x`,
}

// trailerSuffixes follow an otherwise complete valid object: a stray
// closing bracket, a second JSON value or other non-whitespace content,
// immediately or separated by JSON whitespace.
var trailerSuffixes = []string{
	"}", "]", " {}", " null", " 1", " []", `"x"`, "\t}\n", " \r\n]\r\n ", ",", " ,x",
}

// TestPublicEntriesRejectMalformedBodies drives every non-batch public
// entry (both path and query-parameter styles) with null, array, bare
// value, truncated and trailing-data bodies. Each request must return the
// single top-level error object with HTTP 400 invalid_parameter, and no
// request may create, update or delete any instance, even though the query
// parameters of the target are sufficient on their own.
func TestPublicEntriesRejectMalformedBodies(t *testing.T) {
	for _, ep := range bodyIntegrityEntries() {
		ep := ep
		t.Run(ep.name, func(t *testing.T) {
			payloads := append([]string{}, malformedBodies...)
			for _, suffix := range trailerSuffixes {
				payloads = append(payloads, ep.object+suffix)
			}
			targets := []bodyIntegrityTarget{
				target(ep.method, ep.pathTarget),
				target(ep.method, ep.altTarget),
			}
			targets = append(targets, ep.extraTargets...)
			for _, tgt := range targets {
				tgt := tgt
				t.Run(tgt.method+" "+tgt.path, func(t *testing.T) {
					// One fixture per entry/style pair: every rejected body
					// must leave the rows byte-identical to this snapshot, so
					// the fixture can be shared across all payloads.
					router, st := testRouter(t)
					seedBodyIntegrityFixture(t, router)
					before := snapshotRows(t, st)

					for _, raw := range payloads {
						raw := raw
						t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
							rec := doRawRequest(t, router, tgt.method, tgt.path, raw)
							expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")

							after := snapshotRows(t, st)
							if !reflect.DeepEqual(before, after) {
								t.Fatalf("records changed after rejected body %q on %s %s:\nbefore %+v\nafter  %+v",
									raw, tgt.method, tgt.path, before, after)
							}
						})
					}
				})
			}
		})
	}
}

// TestPublicEntriesMalformedBodyWinsOverMissingParameters proves body
// validation runs before parameter validation: an entry with no query
// parameters at all (which would fail business validation on a valid body)
// still reports the same invalid_parameter rejection for a malformed body,
// and a discovery/cleanup request in that shape must not reach the stored
// records or their lost-instance cleanup.
func TestPublicEntriesMalformedBodyWinsOverMissingParameters(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"discover-no-params", http.MethodPost, "/api/v1/discover", "null"},
		{"discover-path-no-params", http.MethodPost, "/api/v1/services/svc/discover", `{"evaluate_at":"x"}]`},
		{"cleanup-no-params", http.MethodPost, "/api/v1/cleanup", "null"},
		{"overview-no-params", http.MethodGet, "/api/v1/services", "null"},
		{"register-no-params", http.MethodPost, "/api/v1/register", "{bad"},
		{"weight-no-params", http.MethodPut, "/api/v1/services/svc/instances/i-1/weight", "null"},
		{"heartbeat-no-params", http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat", "}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			seedBodyIntegrityFixture(t, router)
			before := snapshotRows(t, st)

			rec := doRawRequest(t, router, tc.method, tc.target, tc.body)
			expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")

			if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
				t.Fatalf("records changed:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// TestPublicEntriesBodyErrorPrecedesNotFound sends malformed bodies at
// targets that do not exist. The body error is reported first: the response
// is 400 invalid_parameter, never 404 instance_not_found. A valid body at
// the same missing targets keeps returning 404.
func TestPublicEntriesBodyErrorPrecedesNotFound(t *testing.T) {
	invalid := []string{"null", `{"weight":20}]`, `{"heartbeat_at":"` + integrityTime + `"} x`}
	type target struct {
		name   string
		method string
		path   string
		valid  string
	}
	targets := []target{
		{"get", http.MethodGet, "/api/v1/services/svc/instances/ghost", `{}`},
		{"get-health", http.MethodGet, "/api/v1/services/svc/instances/ghost/health", `{}`},
		{"get-weight", http.MethodGet, "/api/v1/services/svc/instances/ghost/weight", `{}`},
		{"get-heartbeat", http.MethodGet, "/api/v1/services/svc/instances/ghost/heartbeat", `{}`},
		{"delete", http.MethodDelete, "/api/v1/services/svc/instances/ghost", `{}`},
		{"delete-post", http.MethodPost, "/api/v1/services/svc/instances/ghost/delete", `{}`},
		{"heartbeat", http.MethodPost, "/api/v1/services/svc/instances/ghost/heartbeat",
			`{"heartbeat_at":"` + integrityTime + `"}`},
		{"weight", http.MethodPut, "/api/v1/services/svc/instances/ghost/weight", `{"weight":20}`},
	}
	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			seedBodyIntegrityFixture(t, router)
			before := snapshotRows(t, st)

			for _, raw := range invalid {
				rec := doRawRequest(t, router, tc.method, tc.path, raw)
				expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
			}
			if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
				t.Fatalf("records changed after body-first rejections:\n%+v\n%+v", before, after)
			}

			rec := doRawRequest(t, router, tc.method, tc.path, tc.valid)
			expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
		})
	}
}

// TestPublicEntriesBodyErrorPrecedesStorageUnavailable closes the store and
// sends malformed bodies with query parameters sufficient on their own. The
// body error must win: 400 invalid_parameter, never 503. A valid body on the
// same closed store keeps yielding 503 storage_unavailable.
func TestPublicEntriesBodyErrorPrecedesStorageUnavailable(t *testing.T) {
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

	for _, ep := range bodyIntegrityEntries() {
		for _, target := range []string{ep.pathTarget, ep.altTarget} {
			for _, raw := range []string{"null", ep.object + "}", ep.object + " []"} {
				rec := doRawRequest(t, router, ep.method, target, raw)
				expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
			}
		}
	}

	// Valid bodies still reach the store and report storage_unavailable.
	validCases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/register", bodyIntegrityEntries()[0].object},
		{http.MethodDelete, "/api/v1/services/svc/instances/i-1", `{}`},
		{http.MethodPost, "/api/v1/services/svc/instances/i-1/heartbeat", `{"heartbeat_at":"` + integrityTime + `"}`},
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/weight", `{"weight":20}`},
		{http.MethodGet, "/api/v1/services/svc/instances/i-1", `{}`},
		{http.MethodGet, "/api/v1/services", `{"evaluate_at":"2026-10-01T12:10:00Z","heartbeat_timeout":600}`},
	}
	for _, tc := range validCases {
		rec := doRawRequest(t, router, tc.method, tc.path, tc.body)
		expectErrorShape(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	}
}

// TestPublicEntriesBodyReadFailureRejected verifies a body that fails while
// being read is a 400 invalid_parameter and changes nothing, including when
// the query parameters are sufficient.
func TestPublicEntriesBodyReadFailureRejected(t *testing.T) {
	router, st := testRouter(t)
	seedBodyIntegrityFixture(t, router)
	before := snapshotRows(t, st)

	targets := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/v1/services/svc/instances/i-1/weight?weight=20"},
		{http.MethodPost, "/api/v1/register?service_name=svc&instance_id=i-new&weight=10&heartbeat_at=" + integrityTime},
		{http.MethodPost, "/api/v1/cleanup?evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=600"},
		{http.MethodGet, "/api/v1/services?evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=600"},
	}
	for _, tc := range targets {
		req := httptest.NewRequest(tc.method, tc.path, failingBody{})
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
	}
	if after := snapshotRows(t, st); !reflect.DeepEqual(before, after) {
		t.Fatalf("records changed after unreadable body:\n%+v\n%+v", before, after)
	}
}

// TestPublicEntriesAcceptBodiesQueryAndMerged covers the legitimate ways a
// request reaches every entry: a JSON body alone, no body with sufficient
// query parameters, a whitespace-only body with query parameters, an empty
// {} object completed by query parameters, a body wrapped in JSON
// whitespace, and a body field winning over a same-named query parameter.
func TestPublicEntriesAcceptBodiesQueryAndMerged(t *testing.T) {
	eval := "2026-10-01T12:10:00Z"
	evalEarly := "2026-10-01T12:02:00Z"

	t.Run("register and overwrite", func(t *testing.T) {
		router, st := testRouter(t)
		registerInstance(t, router, weightRecord())

		// A complete JSON body overwrites the whole existing record.
		rec := doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1",
			`{"service_name":"svc","instance_id":"i-1","address":"10.0.0.2:9001",`+
				`"port":9001,"healthy":false,"weight":12,"heartbeat_at":"2026-10-01T12:05:00Z"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body overwrite: %d %s", rec.Code, rec.Body.String())
		}
		got, found := publicInstance(t, router, "svc", "i-1")
		if !found || got["address"] != "10.0.0.2:9001" || got["weight"].(float64) != 12 ||
			got["healthy"] != false || got["heartbeat_at"] != "2026-10-01T12:05:00Z" {
			t.Fatalf("overwrite mismatch: %v found=%v", got, found)
		}

		// No body, only query parameters, registers through both styles.
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?service_name=svc&instance_id=i-q&weight=10&heartbeat_at="+integrityTime, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("query register: %d %s", rec.Code, rec.Body.String())
		}
		// A whitespace-only body is equivalent to no body.
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?service_name=svc&instance_id=i-ws&weight=10&heartbeat_at="+integrityTime,
			"  \t\r\n ")
		if rec.Code != http.StatusOK {
			t.Fatalf("whitespace register: %d %s", rec.Code, rec.Body.String())
		}
		// An empty {} object is completed by query parameters.
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?service_name=svc&instance_id=i-empty&weight=10&heartbeat_at="+integrityTime,
			"{}")
		if rec.Code != http.StatusOK {
			t.Fatalf("empty-object register: %d %s", rec.Code, rec.Body.String())
		}
		for _, id := range []string{"i-q", "i-ws", "i-empty"} {
			if _, found := publicInstance(t, router, "svc", id); !found {
				t.Fatalf("%s not registered", id)
			}
		}

		// A body field wins over the same-named query parameter; an invalid
		// body business field never falls back to a valid query value.
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?weight=99",
			`{"service_name":"svc","instance_id":"i-bw","weight":10,"heartbeat_at":"`+integrityTime+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins register: %d %s", rec.Code, rec.Body.String())
		}
		if got, _ := publicInstance(t, router, "svc", "i-bw"); got["weight"].(float64) != 10 {
			t.Fatalf("body weight did not win: %v", got)
		}
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?weight=10&heartbeat_at="+integrityTime,
			`{"service_name":"svc","instance_id":"i-nofallback","weight":-5,"heartbeat_at":"`+integrityTime+`"}`)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-nofallback"); found {
			t.Fatalf("invalid body weight fell back to query weight")
		}

		// JSON whitespace around the object and unknown object fields are fine.
		rec = doRawRequest(t, router, http.MethodPost, "/api/v1/register",
			"\r\n\t {\"service_name\":\"svc\",\"instance_id\":\"i-pad\",\"weight\":10,"+
				`"heartbeat_at":"`+integrityTime+`","mystery":1} `+"\n")
		if rec.Code != http.StatusOK {
			t.Fatalf("padded/unknown-field register: %d %s", rec.Code, rec.Body.String())
		}

		// Brackets and escape sequences inside strings are body content, not
		// trailing tokens.
		rec = doRawRequest(t, router, http.MethodPost, "/api/v1/register",
			`{"service_name":"svc","instance_id":"i-br[]","address":"h}p://x\\\"]\u0041",`+
				`"weight":10,"heartbeat_at":"`+integrityTime+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("brackets-in-string register: %d %s", rec.Code, rec.Body.String())
		}
		if got, _ := publicInstance(t, router, "svc", "i-br[]"); got["address"] != `h}p://x\"]A` {
			t.Fatalf("escaped address mismatch: %v", got)
		}

		// A port above 2^53 stays exact through a body and through a {} body
		// completed by query parameters.
		rec = doRawRequest(t, router, http.MethodPost, "/api/v1/register",
			`{"service_name":"svc","instance_id":"i-big","weight":10,`+
				`"port":9007199254740993,"heartbeat_at":"`+integrityTime+`"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":9007199254740993`) {
			t.Fatalf("big body port: %d %s", rec.Code, rec.Body.String())
		}
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/register?service_name=svc&instance_id=i-bigq&weight=10"+
				"&heartbeat_at="+integrityTime+"&port=9007199254740993", "{}")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":9007199254740993`) {
			t.Fatalf("big query port with {}: %d %s", rec.Code, rec.Body.String())
		}
		if got, _, _ := st.GetInstance("svc", "i-bigq"); got.Port != 9007199254740993 {
			t.Fatalf("exact large port not stored: %d", got.Port)
		}
	})

	t.Run("instance and property queries", func(t *testing.T) {
		router, _ := testRouter(t)
		seedBodyIntegrityFixture(t, router)

		queries := []struct {
			path string
			alt  string
		}{
			{"/api/v1/services/svc/instances/i-1", "/api/v1/instances"},
			{"/api/v1/services/svc/instances/i-1/health", "/api/v1/instances/health"},
			{"/api/v1/services/svc/instances/i-1/weight", "/api/v1/instances/weight"},
			{"/api/v1/services/svc/instances/i-1/heartbeat", "/api/v1/instances/heartbeat"},
		}
		for _, q := range queries {
			// No body on the path entry.
			if rec := doRawRequest(t, router, http.MethodGet, q.path, ""); rec.Code != http.StatusOK {
				t.Fatalf("GET %s empty body: %d %s", q.path, rec.Code, rec.Body.String())
			}
			// A JSON object body on the query-parameter style entry.
			rec := doRawRequest(t, router, http.MethodGet, q.alt,
				`{"service_name":"svc","instance_id":"i-1","unknown":1}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s object body: %d %s", q.alt, rec.Code, rec.Body.String())
			}
			// An empty {} body completed by query parameters.
			rec = doRawRequest(t, router, http.MethodGet,
				q.alt+"?service_name=svc&instance_id=i-1", "{}")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s {} body: %d %s", q.alt, rec.Code, rec.Body.String())
			}
			// Whitespace-only body with query parameters.
			rec = doRawRequest(t, router, http.MethodGet,
				q.alt+"?service_name=svc&instance_id=i-1", "\t\n ")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s whitespace body: %d %s", q.alt, rec.Code, rec.Body.String())
			}
		}

		// Without a path locator the body field wins over the query value.
		rec := doRawRequest(t, router, http.MethodGet,
			"/api/v1/instances?service_name=other&instance_id=i-1",
			`{"service_name":"svc","instance_id":"i-1"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins locate: %d %s", rec.Code, rec.Body.String())
		}
		if inst := decodeBody(t, rec)["instance"].(map[string]any); inst["service_name"] != "svc" {
			t.Fatalf("body locator did not win: %v", inst)
		}
		// With a path locator the body cannot redirect the target.
		rec = doRawRequest(t, router, http.MethodGet,
			"/api/v1/services/other/instances/i-1",
			`{"service_name":"svc","instance_id":"i-2"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("path locate: %d %s", rec.Code, rec.Body.String())
		}
		if inst := decodeBody(t, rec)["instance"].(map[string]any); inst["service_name"] != "other" {
			t.Fatalf("body overrode path locator: %v", inst)
		}
	})

	t.Run("instance list", func(t *testing.T) {
		router, _ := testRouter(t)
		seedBodyIntegrityFixture(t, router)
		down := weightRecord()
		down["instance_id"] = "i-3"
		down["healthy"] = false
		registerInstance(t, router, down)

		for _, tc := range []struct {
			name   string
			target string
			body   string
			want   int
		}{
			{"path no body", "/api/v1/services/svc/instances", "", 3},
			{"query no body", "/api/v1/list?service_name=svc", "", 3},
			{"object body", "/api/v1/list", `{"service_name":"svc"}`, 3},
			{"empty object with query", "/api/v1/list?service_name=svc", "{}", 3},
			{"whitespace with query", "/api/v1/list?service_name=svc", "  \n", 3},
			{"query healthy filter", "/api/v1/services/svc/instances?healthy=false", "", 1},
			{"body filter", "/api/v1/list", `{"service_name":"svc","healthy":false}`, 1},
			{"unknown field tolerated", "/api/v1/list", `{"service_name":"svc","junk":true}`, 3},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := doRawRequest(t, router, http.MethodGet, tc.target, tc.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != tc.want {
					t.Fatalf("got %d instances, want %d", len(ids), tc.want)
				}
			})
		}
	})

	t.Run("service overview", func(t *testing.T) {
		router, _ := testRouter(t)
		seedBodyIntegrityFixture(t, router)
		query := "?evaluate_at=" + eval + "&heartbeat_timeout=600"

		for _, tc := range []struct {
			name   string
			target string
			body   string
		}{
			{"query only", "/api/v1/services" + query, ""},
			{"whitespace body", "/api/v1/services" + query, "  \t\r\n"},
			{"empty object", "/api/v1/services" + query, "{}"},
			{"object body", "/api/v1/services", `{"evaluate_at":"` + eval + `","heartbeat_timeout":600}`},
			{"padded object", "/api/v1/services", "\n " + `{"evaluate_at":"` + eval + `","heartbeat_timeout":600}` + "\t"},
			{"unknown field", "/api/v1/services", `{"evaluate_at":"` + eval + `","heartbeat_timeout":600,"extra":1}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := doRawRequest(t, router, http.MethodGet, tc.target, tc.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				services := decodeBody(t, rec)["services"].([]any)
				if len(services) != 2 {
					t.Fatalf("overview services = %d, want 2", len(services))
				}
			})
		}

		// The overview stays read-only: calling it never removes records.
		rec := doRawRequest(t, router, http.MethodGet,
			"/api/v1/services?evaluate_at=2030-01-01T00:00:00Z&heartbeat_timeout=1",
			`{"unknown":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("far-future overview: %d %s", rec.Code, rec.Body.String())
		}
		if _, found := publicInstance(t, router, "svc", "i-1"); !found {
			t.Fatalf("overview deleted svc/i-1")
		}
	})

	t.Run("delete", func(t *testing.T) {
		type deleteCase struct {
			name   string
			method string
			target string
			body   string
			id     string
		}
		var cases []deleteCase
		styles := []struct {
			method string
			path   func(string) string
		}{
			{http.MethodDelete, func(id string) string {
				return "/api/v1/services/svc/instances/" + id
			}},
			{http.MethodPost, func(id string) string {
				return "/api/v1/services/svc/instances/" + id + "/delete"
			}},
			{http.MethodDelete, func(id string) string {
				return "/api/v1/instances?service_name=svc&instance_id=" + id
			}},
			{http.MethodPost, func(id string) string {
				return "/api/v1/deregister?service_name=svc&instance_id=" + id
			}},
		}
		for i, style := range styles {
			id := fmt.Sprintf("i-del-%d", i)
			cases = append(cases, deleteCase{"query-only-" + fmt.Sprint(i), style.method, style.path(id), "", id})
			id = fmt.Sprintf("i-ws-%d", i)
			cases = append(cases, deleteCase{"whitespace-" + fmt.Sprint(i), style.method, style.path(id), "  \n", id})
			id = fmt.Sprintf("i-empty-%d", i)
			cases = append(cases, deleteCase{"empty-object-" + fmt.Sprint(i), style.method, style.path(id), "{}", id})
		}
		// Object bodies for the param-style and path-style deletes.
		cases = append(cases,
			deleteCase{"object-deregister", http.MethodPost, "/api/v1/deregister",
				`{"service_name":"svc","instance_id":"i-del-obj"}`, "i-del-obj"},
			deleteCase{"object-path", http.MethodDelete, "/api/v1/services/svc/instances/i-del-path",
				`{"service_name":"other","instance_id":"i-9","mystery":1}`, "i-del-path"},
		)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				router, _ := testRouter(t)
				body := weightRecord()
				body["instance_id"] = tc.id
				registerInstance(t, router, body)

				rec := doRawRequest(t, router, tc.method, tc.target, tc.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
				}
				if _, found := publicInstance(t, router, "svc", tc.id); found {
					t.Fatalf("%s still present after delete", tc.id)
				}
			})
		}

		// A malformed body blocks the delete; the same target succeeds once
		// the body is valid, proving the record was untouched in between.
		router, _ := testRouter(t)
		body := weightRecord()
		body["instance_id"] = "i-keep"
		registerInstance(t, router, body)
		rec := doRawRequest(t, router, http.MethodDelete,
			"/api/v1/services/svc/instances/i-keep", "null")
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-keep"); !found {
			t.Fatalf("null body deleted the instance")
		}
		rec = doRawRequest(t, router, http.MethodDelete,
			"/api/v1/services/svc/instances/i-keep", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("subsequent delete: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("single heartbeat", func(t *testing.T) {
		router, st := testRouter(t)
		registerInstance(t, router, weightRecord())

		ok := func(label, target, body string, want string) {
			t.Helper()
			rec := doRawRequest(t, router, http.MethodPost, target, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", label, rec.Code, rec.Body.String())
			}
			inst := decodeBody(t, rec)["instance"].(map[string]any)
			if inst["heartbeat_at"] != want || inst["weight"].(float64) != 10 {
				t.Fatalf("%s changed the wrong fields: %v", label, inst)
			}
		}
		want7 := "2026-10-01T12:07:00Z"
		want1 := "2026-10-01T12:01:00Z"
		ok("body", "/api/v1/services/svc/instances/i-1/heartbeat",
			`{"heartbeat_at":"`+want7+`"}`, want7)
		ok("query only", "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+want1, "", want1)
		ok("whitespace", "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+want7, "  \n", want7)
		ok("empty object", "/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+want1, "{}", want1)
		ok("padded object", "/api/v1/services/svc/instances/i-1/heartbeat",
			"\t {\"heartbeat_at\":\""+want7+"\"} \r\n", want7)
		ok("unknown field", "/api/v1/services/svc/instances/i-1/heartbeat",
			`{"heartbeat_at":"`+want1+`","x":1}`, want1)
		// The body heartbeat wins over the query value; an invalid body value
		// never falls back to the valid query one.
		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+want1,
			`{"heartbeat_at":"`+want7+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("body-wins heartbeat: %d %s", rec.Code, rec.Body.String())
		}
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/instances/i-1/heartbeat?heartbeat_at="+want7,
			`{"heartbeat_at":"not-a-time"}`)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, _, _ := st.GetInstance("svc", "i-1")
		if !got.HeartbeatAt.Equal(mustParseTime(want7)) {
			t.Fatalf("invalid body heartbeat fell back to query: %+v", got)
		}
	})

	t.Run("batch heartbeat", func(t *testing.T) {
		router, _ := testRouter(t)
		seedBodyIntegrityFixture(t, router)

		// A valid object body still succeeds; empty or malformed bodies are
		// rejected and cannot be rescued by query parameters.
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/heartbeat",
			`{"service_name":"svc","instances":[
				{"instance_id":"i-2","heartbeat_at":"2026-10-01T12:08:00Z"},
				{"instance_id":"i-1","heartbeat_at":"2026-10-01T12:07:00Z"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("valid batch: %d %s", rec.Code, rec.Body.String())
		}
		for _, raw := range []string{"", "   ", "{}", "null", `{"service_name":"svc"} ]`} {
			rec := doRawRequest(t, router, http.MethodPost,
				"/api/v1/heartbeat?service_name=svc", raw)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		}
		// A whitespace-only or {} body with a query parameter cannot express
		// the instances list, so it stays a business validation error with no
		// writes; null and a trailing token are body integrity errors. The
		// successful batch above is the only mutation.
		if inst, found := publicInstance(t, router, "svc", "i-2"); !found ||
			inst["heartbeat_at"] != "2026-10-01T12:08:00Z" {
			t.Fatalf("rejected batches altered i-2: %v found=%v", inst, found)
		}
	})

	t.Run("single weight", func(t *testing.T) {
		router, st := testRouter(t)
		registerInstance(t, router, weightRecord())

		ok := func(label, target, body string, want float64) {
			t.Helper()
			rec := doRawRequest(t, router, http.MethodPut, target, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", label, rec.Code, rec.Body.String())
			}
			inst := decodeBody(t, rec)["instance"].(map[string]any)
			if inst["weight"].(float64) != want {
				t.Fatalf("%s weight = %v, want %v", label, inst["weight"], want)
			}
		}
		ok("body", "/api/v1/services/svc/instances/i-1/weight", `{"weight":20}`, 20)
		ok("query only", "/api/v1/services/svc/instances/i-1/weight?weight=21", "", 21)
		ok("whitespace", "/api/v1/services/svc/instances/i-1/weight?weight=22", "\r\n ", 22)
		ok("empty object", "/api/v1/services/svc/instances/i-1/weight?weight=23", "{}", 23)
		ok("padded object", "/api/v1/services/svc/instances/i-1/weight",
			" \t{\"weight\":24}\n ", 24)
		ok("unknown field", "/api/v1/services/svc/instances/i-1/weight",
			`{"weight":25,"ignored":true}`, 25)
		// Body wins over the query and an invalid body weight does not fall back.
		ok("body wins", "/api/v1/services/svc/instances/i-1/weight?weight=99",
			`{"weight":26}`, 26)
		rec := doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=27",
			`{"weight":-1}`)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, _, _ := st.GetInstance("svc", "i-1")
		if got.Weight != 26 {
			t.Fatalf("invalid body weight fell back to query: %+v", got)
		}

		// The headline regression: the object followed by a stray bracket is
		// rejected and the record keeps its weight.
		rec = doRawRequest(t, router, http.MethodPut,
			"/api/v1/services/svc/instances/i-1/weight?weight=5",
			`{"weight":5}]`)
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, _, _ = st.GetInstance("svc", "i-1")
		if got.Weight != 26 {
			t.Fatalf("trailing bracket changed weight: %+v", got)
		}
	})

	t.Run("discover", func(t *testing.T) {
		fresh := weightRecord()
		fresh["instance_id"] = "i-fresh"
		fresh["heartbeat_at"] = "2026-10-01T12:09:00Z"
		setup := func() (http.Handler, *store.Store) {
			router, st := testRouter(t)
			gone := weightRecord()
			gone["instance_id"] = "i-gone"
			registerInstance(t, router, gone)
			registerInstance(t, router, fresh)
			peer := weightRecord()
			peer["service_name"] = "other"
			peer["instance_id"] = "i-gone"
			registerInstance(t, router, peer)
			return router, st
		}
		discoverOK := func(t *testing.T, router http.Handler, method, target, body string) {
			t.Helper()
			rec := doRawRequest(t, router, method, target, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s body %q: %d %s", method, target, body, rec.Code, rec.Body.String())
			}
			if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "i-fresh" {
				t.Fatalf("discover hits = %v, want [i-fresh]", ids)
			}
			if _, found := publicInstance(t, router, "svc", "i-gone"); found {
				t.Fatalf("lost instance was not cleaned up")
			}
			if _, found := publicInstance(t, router, "other", "i-gone"); !found {
				t.Fatalf("another service's record was touched")
			}
		}

		router, _ := setup()
		discoverOK(t, router, http.MethodGet,
			"/api/v1/services/svc/discover?evaluate_at="+eval+"&heartbeat_timeout=300", "")
		router, _ = setup()
		discoverOK(t, router, http.MethodGet,
			"/api/v1/discover?service_name=svc&evaluate_at="+eval+"&heartbeat_timeout=300", "  \n")
		router, _ = setup()
		discoverOK(t, router, http.MethodPost,
			"/api/v1/services/svc/discover?evaluate_at="+eval+"&heartbeat_timeout=300", "{}")
		router, _ = setup()
		discoverOK(t, router, http.MethodPost, "/api/v1/discover",
			`{"service_name":"svc","evaluate_at":"`+eval+`","heartbeat_timeout":300,"extra":1}`)
		router, _ = setup()
		discoverOK(t, router, http.MethodPost, "/api/v1/services/svc/discover",
			"\t "+`{"evaluate_at":"`+eval+`","heartbeat_timeout":300}`+"\n")

		// Body parameters win over same-named query parameters: with the query
		// evaluation point the lost record is fresh, but the body point is
		// later, so the cleanup runs at the body point.
		router, _ = setup()
		discoverOK(t, router, http.MethodPost,
			"/api/v1/services/svc/discover?evaluate_at="+evalEarly+"&heartbeat_timeout=300",
			`{"evaluate_at":"`+eval+`","heartbeat_timeout":300}`)

		// An invalid body business field does not fall back to the query, and a
		// body error prevents both the response and the lost cleanup.
		router, st := setup()
		rec := doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/discover?evaluate_at="+eval+"&heartbeat_timeout=300",
			`{"heartbeat_timeout":0}`)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
			t.Fatalf("invalid body timeout reached cleanup despite query value")
		}
		rec = doRawRequest(t, router, http.MethodPost,
			"/api/v1/services/svc/discover?evaluate_at="+eval+"&heartbeat_timeout=300", "null")
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
			t.Fatalf("null body reached cleanup")
		}
		_ = st

		// The strict timeout boundary is unchanged: equality keeps the record.
		// This fixture uses no instance newer than the evaluation point, since
		// a future heartbeat is independently rejected by the skew guard.
		brouter, _ := testRouter(t)
		boundary := weightRecord()
		boundary["instance_id"] = "i-edge"
		boundary["heartbeat_at"] = "2026-10-01T12:00:00Z"
		registerInstance(t, brouter, boundary)
		rec = doRawRequest(t, brouter, http.MethodGet,
			"/api/v1/services/svc/discover?evaluate_at=2026-10-01T12:05:00Z&heartbeat_timeout=300", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("boundary discover: %d %s", rec.Code, rec.Body.String())
		}
		if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "i-edge" {
			t.Fatalf("boundary instance removed or hidden: %v", ids)
		}
		if _, found := publicInstance(t, brouter, "svc", "i-edge"); !found {
			t.Fatalf("record on the strict boundary was deleted")
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		setup := func() (http.Handler, *store.Store) {
			router, st := testRouter(t)
			gone := weightRecord()
			gone["instance_id"] = "i-gone"
			gone["healthy"] = false
			registerInstance(t, router, gone)
			// The peer in another service is fresh at the evaluation point, so
			// an unscoped cleanup must leave it behind.
			peer := weightRecord()
			peer["service_name"] = "other"
			peer["instance_id"] = "i-gone"
			peer["heartbeat_at"] = "2026-10-01T12:09:00Z"
			registerInstance(t, router, peer)
			return router, st
		}
		cleanupOK := func(t *testing.T, router http.Handler, target, body string, removed int) {
			t.Helper()
			rec := doRawRequest(t, router, http.MethodPost, target, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("cleanup %s body %q: %d %s", target, body, rec.Code, rec.Body.String())
			}
			out := decodeBody(t, rec)
			if out["removed"].(float64) != float64(removed) {
				t.Fatalf("removed = %v, want %d", out["removed"], removed)
			}
		}

		query := "?evaluate_at=" + eval + "&heartbeat_timeout=300"
		router, _ := setup()
		cleanupOK(t, router, "/api/v1/cleanup"+query, "", 1)
		if _, found := publicInstance(t, router, "other", "i-gone"); !found {
			t.Fatalf("unscoped cleanup touched another service")
		}

		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup"+query, " \t\n", 1)
		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup"+query, "{}", 1)
		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup",
			`{"evaluate_at":"`+eval+`","heartbeat_timeout":300,"extra":1}`, 1)
		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup",
			"\r\n "+`{"evaluate_at":"`+eval+`","heartbeat_timeout":300}`+"\r\n", 1)

		// An explicit service_name in the body scopes the cleanup; brackets in
		// the name are string content and simply match nothing.
		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup",
			`{"service_name":"svc","evaluate_at":"`+eval+`","heartbeat_timeout":300}`, 1)
		if _, found := publicInstance(t, router, "other", "i-gone"); !found {
			t.Fatalf("scoped cleanup touched another service")
		}
		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup",
			`{"service_name":"x}]y","evaluate_at":"`+eval+`","heartbeat_timeout":300}`, 0)
		if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
			t.Fatalf("brackety scope matched and deleted svc records")
		}

		// Body parameters win and invalid body business fields do not fall
		// back; a malformed body performs no deletion at all.
		router, _ = setup()
		cleanupOK(t, router, "/api/v1/cleanup?evaluate_at="+evalEarly+"&heartbeat_timeout=300",
			`{"evaluate_at":"`+eval+`","heartbeat_timeout":300}`, 1)
		router, _ = setup()
		rec := doRawRequest(t, router, http.MethodPost, "/api/v1/cleanup"+query,
			`{"heartbeat_timeout":0}`)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
			t.Fatalf("invalid body timeout fell back to the query value")
		}
		rec = doRawRequest(t, router, http.MethodPost, "/api/v1/cleanup"+query, `{"evaluate_at":"`+eval+`"} ]`)
		expectOnlyTopLevelError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, found := publicInstance(t, router, "svc", "i-gone"); !found {
			t.Fatalf("trailing token reached cleanup")
		}
	})
}
