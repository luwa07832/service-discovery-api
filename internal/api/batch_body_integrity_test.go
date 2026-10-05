package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Complete, otherwise-valid batch bodies. The register batch mixes an
// overwrite of the existing i-1 with creation of i-3; accepting any of them
// would either rewrite i-1 or create i-3, so a rejection leaves both visible.
const registerTrailerObjectByPath = `{"instances":[` +
	`{"instance_id":"i-1","address":"10.0.0.1:9000","port":9000,"healthy":false,"weight":77,"heartbeat_at":"2026-10-01T12:30:00Z"},` +
	`{"instance_id":"i-3","address":"10.0.0.3:9003","port":9003,"healthy":true,"weight":88,"heartbeat_at":"2026-10-01T12:31:00Z"}]}`

const registerTrailerObjectByBody = `{"service_name":"svc","instances":[` +
	`{"instance_id":"i-1","address":"10.0.0.1:9000","port":9000,"healthy":false,"weight":77,"heartbeat_at":"2026-10-01T12:30:00Z"},` +
	`{"instance_id":"i-3","address":"10.0.0.3:9003","port":9003,"healthy":true,"weight":88,"heartbeat_at":"2026-10-01T12:31:00Z"}]}`

const weightTrailerObject = `{"updates":[` +
	`{"instance_id":"i-1","weight":55},` +
	`{"instance_id":"i-2","weight":66}]}`

type batchEndpoint struct {
	name   string
	method string
	path   string
	object string
}

func bodyIntegrityEndpoints() []batchEndpoint {
	return []batchEndpoint{
		{"register-path", http.MethodPost, "/api/v1/services/svc/instances/batch", registerTrailerObjectByPath},
		{"register-body", http.MethodPost, "/api/v1/register/batch", registerTrailerObjectByBody},
		{"weight", http.MethodPut, "/api/v1/services/svc/instances/weight", weightTrailerObject},
	}
}

// publicInstance fetches a full record through the public GET entry. A 404
// reports found=false so rejected batches can be proven not to create rows.
func publicInstance(t *testing.T, router http.Handler, service, id string) (map[string]any, bool) {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/services/%s/instances/%s", service, id), nil)
	switch rec.Code {
	case http.StatusOK:
		return decodeBody(t, rec)["instance"].(map[string]any), true
	case http.StatusNotFound:
		return nil, false
	default:
		t.Fatalf("GET %s/%s: status %d body %s", service, id, rec.Code, rec.Body.String())
		return nil, false
	}
}

// assertOriginalI1 verifies every field of the pre-existing svc/i-1 record
// through the public query, proving a rejected request performed no overwrite.
func assertOriginalI1(t *testing.T, inst map[string]any) {
	t.Helper()
	if inst["service_name"] != "svc" || inst["instance_id"] != "i-1" ||
		inst["address"] != "10.0.0.8:8080" || inst["port"].(float64) != 8080 ||
		inst["healthy"] != true || inst["weight"].(float64) != 10 ||
		inst["heartbeat_at"] != baseTime {
		t.Fatalf("svc/i-1 changed after rejected request: %v", inst)
	}
}

// TestBatchEntriesRejectDataAfterObject covers the integrity gap: a complete
// valid object followed immediately or after JSON whitespace by a stray "}",
// "]", a second object or null must be rejected on all three batch entries,
// and the rejection must leave no partial write behind.
func TestBatchEntriesRejectDataAfterObject(t *testing.T) {
	trailers := []string{"}", "]", "{}", "null", " }", "\t]\n", " {}", " null"}

	for _, ep := range bodyIntegrityEndpoints() {
		router, st := testRouter(t)
		registerInstance(t, router, weightRecord())
		second := weightRecord()
		second["instance_id"] = "i-2"
		second["weight"] = 3
		registerInstance(t, router, second)
		other := weightRecord()
		other["service_name"] = "other"
		registerInstance(t, router, other)

		for _, trailer := range trailers {
			t.Run(ep.name+"+"+fmt.Sprintf("%q", trailer), func(t *testing.T) {
				rec := doRawRequest(t, router, ep.method, ep.path, ep.object+trailer)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

				// Existing instances keep every field; no new instance appears.
				got, found := publicInstance(t, router, "svc", "i-1")
				if !found {
					t.Fatalf("svc/i-1 disappeared after rejected request")
				}
				assertOriginalI1(t, got)
				i2, found := publicInstance(t, router, "svc", "i-2")
				if !found || i2["weight"].(float64) != 3 {
					t.Fatalf("svc/i-2 changed after rejected request: %v found=%v", i2, found)
				}
				if _, found := publicInstance(t, router, "svc", "i-3"); found {
					t.Fatalf("rejected register batch created svc/i-3")
				}
				// Records of another service are untouched.
				peer, found := publicInstance(t, router, "other", "i-1")
				if !found || peer["weight"].(float64) != 10 {
					t.Fatalf("other/i-1 changed after rejected request: %v found=%v", peer, found)
				}
				// The store still reports exactly the three pre-existing rows.
				if all, err := st.ListAllInstances(); err != nil || len(all) != 3 {
					t.Fatalf("store rows = %d (err %v), want 3", len(all), err)
				}
			})
		}
	}
}

// TestBatchEntriesRejectNonObjectBodies covers empty, whitespace-only and
// bare-value bodies. Query parameters must not be able to repair any of them.
func TestBatchEntriesRejectNonObjectBodies(t *testing.T) {
	bodies := []string{
		"", "   ", "\t\r\n ",
		"null", "[]", "[{}]", "123", "true", `"text"`, "{bad",
	}

	for _, ep := range bodyIntegrityEndpoints() {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())
		second := weightRecord()
		second["instance_id"] = "i-2"
		second["weight"] = 3
		registerInstance(t, router, second)

		for _, raw := range bodies {
			t.Run(ep.name+"+"+fmt.Sprintf("%q", raw), func(t *testing.T) {
				rec := doRawRequest(t, router, ep.method, ep.path, raw)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
				got, found := publicInstance(t, router, "svc", "i-1")
				if !found {
					t.Fatalf("svc/i-1 disappeared")
				}
				assertOriginalI1(t, got)
				if i2, found := publicInstance(t, router, "svc", "i-2"); !found || i2["weight"].(float64) != 3 {
					t.Fatalf("svc/i-2 changed after rejected request: %v found=%v", i2, found)
				}
				if _, found := publicInstance(t, router, "svc", "i-3"); found {
					t.Fatalf("rejected request created svc/i-3")
				}
			})
		}
	}

	// Query parameters cannot rescue a body that is not one JSON object.
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())
	rescueCases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/register/batch?service_name=svc", ""},
		{http.MethodPost, "/api/v1/register/batch?service_name=svc", "null"},
		{http.MethodPost, "/api/v1/register/batch?service_name=svc", registerTrailerObjectByBody + "}"},
		{http.MethodPut, "/api/v1/services/svc/instances/weight?weight=5", ""},
		{http.MethodPut, "/api/v1/services/svc/instances/weight?weight=5", "null"},
		{http.MethodPut, "/api/v1/services/svc/instances/weight?weight=5", weightTrailerObject + "]"},
	}
	for _, rc := range rescueCases {
		rec := doRawRequest(t, router, rc.method, rc.path, rc.body)
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		got, found := publicInstance(t, router, "svc", "i-1")
		if !found {
			t.Fatalf("svc/i-1 disappeared after %s %s", rc.method, rc.path)
		}
		if got["weight"].(float64) != 10 {
			t.Fatalf("query parameter rescued an invalid body: %v", got)
		}
	}
}

// TestBatchEntriesAcceptWhitespaceAroundObject ensures leading and trailing
// JSON whitespace (spaces, tabs, newlines, carriage returns) around an
// otherwise valid object still succeeds on every batch entry.
func TestBatchEntriesAcceptWhitespaceAroundObject(t *testing.T) {
	wrappers := []string{"  %s  ", "\t%s\n", "\r\n\t %s \r\n"}

	t.Run("register", func(t *testing.T) {
		router, _ := testRouter(t)
		for i, wrap := range wrappers {
			id := fmt.Sprintf("i-w%d", i)
			for _, target := range []string{
				"/api/v1/services/svc/instances/batch",
				"/api/v1/register/batch",
			} {
				var raw string
				if strings.HasPrefix(target, "/api/v1/services/") {
					raw = fmt.Sprintf(wrap, fmt.Sprintf(
						`{"instances":[{"instance_id":%q,"weight":10,"heartbeat_at":%q}]}`,
						id, baseTime))
				} else {
					raw = fmt.Sprintf(wrap, fmt.Sprintf(
						`{"service_name":"svc","instances":[{"instance_id":%q,"weight":10,"heartbeat_at":%q}]}`,
						id, baseTime))
				}
				rec := doRawRequest(t, router, http.MethodPost, target, raw)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s wrapped body %q: status %d body %s", target, raw, rec.Code, rec.Body.String())
				}
				got, found := publicInstance(t, router, "svc", id)
				if !found || got["weight"].(float64) != 10 {
					t.Fatalf("%s did not register %s: %v found=%v", target, id, got, found)
				}
			}
		}
	})

	t.Run("weight", func(t *testing.T) {
		router, _ := testRouter(t)
		registerInstance(t, router, weightRecord())
		for i, wrap := range wrappers {
			want := 21 + i
			raw := fmt.Sprintf(wrap, fmt.Sprintf(
				`{"updates":[{"instance_id":"i-1","weight":%d}]}`, want))
			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/weight", raw)
			if rec.Code != http.StatusOK {
				t.Fatalf("wrapped weight body %q: status %d body %s", raw, rec.Code, rec.Body.String())
			}
			got, found := publicInstance(t, router, "svc", "i-1")
			if !found || got["weight"].(float64) != float64(want) {
				t.Fatalf("weight = %v found=%v, want %d", got["weight"], found, want)
			}
		}
	})
}

// TestBatchEntriesAcceptClosingSymbolsInsideStrings ensures the trailing-data
// check only looks outside JSON strings: closing brackets that are part of a
// string value must not be mistaken for trailing tokens.
func TestBatchEntriesAcceptClosingSymbolsInsideStrings(t *testing.T) {
	router, _ := testRouter(t)

	const entry = `{"instance_id":"i-br","address":"10.0.0.8}:80]80",` +
		`"port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:00:00Z"}`
	for _, raw := range []string{
		`{"instances":[` + entry + `]}`,
		`{"service_name":"svc","instances":[` + entry + `]}`,
	} {
		target := "/api/v1/services/svc/instances/batch"
		if strings.Contains(raw, `"service_name"`) {
			target = "/api/v1/register/batch"
		}
		rec := doRawRequest(t, router, http.MethodPost, target, raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", target, rec.Code, rec.Body.String())
		}
	}
	got, found := publicInstance(t, router, "svc", "i-br")
	if !found || got["address"] != "10.0.0.8}:80]80" {
		t.Fatalf("address with brackets not stored: %v found=%v", got, found)
	}

	// The weight batch carries brackets inside an instance id string.
	brackety := weightRecord()
	brackety["instance_id"] = "i-x}]y"
	registerInstance(t, router, brackety)
	raw := `{"updates":[{"instance_id":"i-x}]y","weight":42}]}`
	rec := doRawRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("weight body with brackets inside id: status %d body %s", rec.Code, rec.Body.String())
	}
	updated, found := publicInstance(t, router, "svc", "i-x}]y")
	if !found || updated["weight"].(float64) != 42 {
		t.Fatalf("weight not applied: %v found=%v", updated, found)
	}
}

// TestBatchRegisterPortExactAndRejected re-states the exact-integer port
// semantics through the two batch register entries: values above 2^53 write
// and read back over the public query exactly, while out-of-range or
// non-integer values reject the whole batch without writing the valid entry.
func TestBatchRegisterPortExactAndRejected(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, weightRecord())

	// Exact large integers are accepted on both entries and read back whole.
	entries := []any{
		map[string]any{"instance_id": "i-big", "weight": 10,
			"port": json.Number("9007199254740993"), "heartbeat_at": at(0)},
		map[string]any{"instance_id": "i-max", "weight": 5,
			"port": json.Number("9223372036854775807"), "heartbeat_at": at(0)},
	}
	for _, target := range []string{
		"/api/v1/services/svc/instances/batch",
		"/api/v1/register/batch",
	} {
		rec := doRequest(t, router, http.MethodPost, target, map[string]any{
			"service_name": "svc",
			"instances":    entries,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", target, rec.Code, rec.Body.String())
		}
		rec = doRequest(t, router, http.MethodGet,
			"/api/v1/services/svc/instances/i-big", nil)
		if rec.Code != http.StatusOK ||
			!strings.Contains(rec.Body.String(), `"port":9007199254740993`) {
			t.Fatalf("i-big readback: status %d body %s", rec.Code, rec.Body.String())
		}
		rec = doRequest(t, router, http.MethodGet,
			"/api/v1/services/svc/instances/i-max", nil)
		if rec.Code != http.StatusOK ||
			!strings.Contains(rec.Body.String(), `"port":9223372036854775807`) {
			t.Fatalf("i-max readback: status %d body %s", rec.Code, rec.Body.String())
		}
	}
	if got, _, _ := st.GetInstance("svc", "i-big"); got.Port != 9007199254740993 {
		t.Fatalf("i-big stored port = %d", got.Port)
	}
	if got, _, _ := st.GetInstance("svc", "i-max"); got.Port != 9223372036854775807 {
		t.Fatalf("i-max stored port = %d", got.Port)
	}

	// A bad entry rejects the batch on both entries and writes nothing.
	badBatches := []struct {
		name string
		port any
	}{
		{"beyond-int64", json.Number("9223372036854775808")},
		{"fractional-number", json.Number("8080.5")},
		{"fractional-text", "8080.5"},
		{"negative-text", "-1"},
	}
	for _, bc := range badBatches {
		newID := "i-new-" + bc.name
		badID := "i-bad-" + bc.name
		batch := []any{
			map[string]any{"instance_id": newID, "weight": 10,
				"port": json.Number("8080"), "heartbeat_at": at(0)},
			map[string]any{"instance_id": badID, "weight": 10,
				"port": bc.port, "heartbeat_at": at(0)},
		}
		for _, target := range []string{
			"/api/v1/services/svc/instances/batch",
			"/api/v1/register/batch",
		} {
			rec := doRequest(t, router, http.MethodPost, target, map[string]any{
				"service_name": "svc",
				"instances":    batch,
			})
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
		}
		if _, found := publicInstance(t, router, "svc", newID); found {
			t.Fatalf("%s: valid entry written despite invalid batch", newID)
		}
		if _, found := publicInstance(t, router, "svc", badID); found {
			t.Fatalf("%s: invalid entry written", badID)
		}
		got, found := publicInstance(t, router, "svc", "i-1")
		if !found {
			t.Fatalf("svc/i-1 disappeared")
		}
		assertOriginalI1(t, got)
	}
}

// TestBatchWeightMissingTargetLeavesNoPartialUpdate verifies through public
// queries that a valid weight batch naming a missing target changes none of
// the existing targets.
func TestBatchWeightMissingTargetLeavesNoPartialUpdate(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, weightRecord())
	second := weightRecord()
	second["instance_id"] = "i-2"
	second["weight"] = 3
	registerInstance(t, router, second)

	rec := doRawRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight",
		`{"updates":[{"instance_id":"i-1","weight":50},{"instance_id":"ghost","weight":60},{"instance_id":"i-2","weight":70}]}`)
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
	if got, found := publicInstance(t, router, "svc", "i-1"); !found || got["weight"].(float64) != 10 {
		t.Fatalf("i-1 changed after 404 batch: %v found=%v", got, found)
	}
	if got, found := publicInstance(t, router, "svc", "i-2"); !found || got["weight"].(float64) != 3 {
		t.Fatalf("i-2 changed after 404 batch: %v found=%v", got, found)
	}
	if _, found := publicInstance(t, router, "svc", "ghost"); found {
		t.Fatalf("ghost instance appeared after 404 batch")
	}
}
