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

// portBody builds a registration body whose port is the given raw value.
func portBody(id string, port any) map[string]any {
	return map[string]any{
		"service_name": "svc",
		"instance_id":  id,
		"weight":       10,
		"heartbeat_at": at(0),
		"port":         port,
	}
}

func TestPortExactIntegerForms(t *testing.T) {
	router, st := testRouter(t)
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"plain integer", "8080", 8080},
		{"zero", "0", 0},
		{"trailing fraction", "8080.0", 8080},
		{"scientific notation", "8.08e3", 8080},
		{"above float53", "9007199254740993", 9007199254740993},
		{"int64 upper bound", "9223372036854775807", 9223372036854775807},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("i-%d", i)
			rec := doRequest(t, router, http.MethodPost, "/api/v1/register",
				portBody(id, json.Number(tc.raw)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			// The success response carries the exact JSON integer.
			wantJSON := fmt.Sprintf(`"port":%d`, tc.want)
			if !strings.Contains(rec.Body.String(), wantJSON) {
				t.Fatalf("response body %s missing %s", rec.Body.String(), wantJSON)
			}
			// The stored record keeps the exact integer.
			got, found, err := st.GetInstance("svc", id)
			if err != nil || !found {
				t.Fatalf("stored: found=%v err=%v", found, err)
			}
			if got.Port != tc.want {
				t.Fatalf("stored port = %d, want %d", got.Port, tc.want)
			}
			// The public query returns the exact JSON integer.
			rec = doRequest(t, router, http.MethodGet,
				"/api/v1/services/svc/instances/"+id, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("get status = %d body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), wantJSON) {
				t.Fatalf("get body %s missing %s", rec.Body.String(), wantJSON)
			}
		})
	}
}

func TestPortNumericStringForms(t *testing.T) {
	router, st := testRouter(t)

	// Numeric strings keep exact integer semantics and may carry whitespace.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register",
		portBody("i-str", " 9007199254740993 "))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if got, _, _ := st.GetInstance("svc", "i-str"); got.Port != 9007199254740993 {
		t.Fatalf("string port = %d, want 9007199254740993", got.Port)
	}

	// The query parameter form parses the int64 upper bound exactly.
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/register?port=9223372036854775807",
		map[string]any{
			"service_name": "svc",
			"instance_id":  "i-query",
			"weight":       10,
			"heartbeat_at": at(0),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if got, _, _ := st.GetInstance("svc", "i-query"); got.Port != 9223372036854775807 {
		t.Fatalf("query port = %d, want 9223372036854775807", got.Port)
	}

	// The body wins over the query parameter; an invalid query port does not
	// fall back, and an invalid body port does not fall back either.
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/register?port=9223372036854775807",
		portBody("i-body-wins", json.Number("8080")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if got, _, _ := st.GetInstance("svc", "i-body-wins"); got.Port != 8080 {
		t.Fatalf("body port = %d, want 8080", got.Port)
	}
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/register?port=8080",
		portBody("i-no-fallback", json.Number("9223372036854775808")))
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-no-fallback"); found {
		t.Fatalf("invalid body port fell back to the query value")
	}
}

func TestPortRejectedValues(t *testing.T) {
	router, st := testRouter(t)
	registerInstance(t, router, map[string]any{
		"service_name": "svc",
		"instance_id":  "i-1",
		"port":         8080,
		"weight":       10,
		"heartbeat_at": at(0),
	})

	cases := []struct {
		name string
		port any
	}{
		{"explicit null", nil},
		{"boolean", true},
		{"array", []any{8080}},
		{"object", map[string]any{"port": 8080}},
		{"empty text", ""},
		{"blank text", "   "},
		{"unparseable text", "eight thousand"},
		{"non-finite text", "NaN"},
		{"infinite text", "Inf"},
		{"negative number", json.Number("-1")},
		{"negative text", "-1"},
		{"fractional number", json.Number("1.5")},
		{"fractional text", "8080.5"},
		{"non-integer above float53", json.Number("9007199254740992.5")},
		{"beyond int64", json.Number("9223372036854775808")},
		{"beyond int64 text", "9223372036854775808"},
		{"huge exponent", json.Number("1e400")},
		{"huge exponent text", "1e400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := portBody("i-1", tc.port)
			for _, target := range []struct {
				method string
				path   string
			}{
				{http.MethodPost, "/api/v1/register"},
				{http.MethodPost, "/api/v1/instances"},
				{http.MethodPut, "/api/v1/instances"},
				{http.MethodPost, "/api/v1/services/svc/instances"},
				{http.MethodPut, "/api/v1/services/svc/instances/i-1"},
			} {
				rec := doRequest(t, router, target.method, target.path, body)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
			}
			// The rejected writes never changed the existing record.
			got, found, _ := st.GetInstance("svc", "i-1")
			if !found || got.Port != 8080 || got.Weight != 10 ||
				!got.HeartbeatAt.Equal(mustParseTime(baseTime)) {
				t.Fatalf("record changed after invalid port: %+v", got)
			}
		})
	}
}

func TestPortBatchExactAndAtomic(t *testing.T) {
	router, st := testRouter(t)

	// A valid batch keeps exact integers and request order on both entries.
	entries := []any{
		map[string]any{
			"instance_id": "i-big", "weight": 10,
			"port": json.Number("9007199254740993"), "heartbeat_at": at(0),
		},
		map[string]any{
			"instance_id": "i-max", "weight": 5,
			"port": json.Number("9223372036854775807"), "heartbeat_at": at(0),
		},
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
			t.Fatalf("%s: status = %d body %s", target, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		list := out["instances"].([]any)
		if len(list) != 2 ||
			list[0].(map[string]any)["instance_id"] != "i-big" ||
			list[1].(map[string]any)["instance_id"] != "i-max" {
			t.Fatalf("%s: order not preserved: %v", target, out)
		}
		if !strings.Contains(rec.Body.String(), `"port":9007199254740993`) ||
			!strings.Contains(rec.Body.String(), `"port":9223372036854775807`) {
			t.Fatalf("%s: inexact ports in %s", target, rec.Body.String())
		}
	}
	if got, _, _ := st.GetInstance("svc", "i-big"); got.Port != 9007199254740993 {
		t.Fatalf("i-big port = %d", got.Port)
	}
	if got, _, _ := st.GetInstance("svc", "i-max"); got.Port != 9223372036854775807 {
		t.Fatalf("i-max port = %d", got.Port)
	}

	// One invalid entry rejects the whole batch and writes nothing.
	invalid := []any{
		map[string]any{
			"instance_id": "i-ok", "weight": 10,
			"port": json.Number("8080"), "heartbeat_at": at(0),
		},
		map[string]any{
			"instance_id": "i-bad", "weight": 10,
			"port": json.Number("9223372036854775808"), "heartbeat_at": at(0),
		},
	}
	for _, target := range []string{
		"/api/v1/services/svc/instances/batch",
		"/api/v1/register/batch",
	} {
		rec := doRequest(t, router, http.MethodPost, target, map[string]any{
			"service_name": "svc",
			"instances":    invalid,
		})
		expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	}
	for _, id := range []string{"i-ok", "i-bad"} {
		if _, found, _ := st.GetInstance("svc", id); found {
			t.Fatalf("invalid batch left %s behind", id)
		}
	}
}

func TestPortSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register",
		portBody("i-1", json.Number("9007199254740993")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, found, err := reopened.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("reopened: found=%v err=%v", found, err)
	}
	if got.Port != 9007199254740993 {
		t.Fatalf("reopened port = %d, want 9007199254740993", got.Port)
	}
	rec = doRequest(t, NewRouter(reopened), http.MethodGet,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"port":9007199254740993`) {
		t.Fatalf("reopened get: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPortPreservedByHeartbeatAndUpdates(t *testing.T) {
	router, st := testRouter(t)
	rec := doRequest(t, router, http.MethodPost, "/api/v1/register",
		portBody("i-1", json.Number("9223372036854775807")))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: status = %d body %s", rec.Code, rec.Body.String())
	}
	const wantJSON = `"port":9223372036854775807`

	// Heartbeat renewal keeps the port and echoes it exactly.
	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": at(5)})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wantJSON) {
		t.Fatalf("heartbeat: status %d body %s", rec.Code, rec.Body.String())
	}
	// Weight and health updates keep the port and echo it exactly.
	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight",
		map[string]any{"weight": 20})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wantJSON) {
		t.Fatalf("weight: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wantJSON) {
		t.Fatalf("health: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _, _ := st.GetInstance("svc", "i-1"); got.Port != 9223372036854775807 {
		t.Fatalf("port after updates = %d", got.Port)
	}

	// List and discovery responses carry the exact integer too.
	rec = doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wantJSON) {
		t.Fatalf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("health restore: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/discover?evaluate_at="+at(10)+"&heartbeat_timeout=300", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wantJSON) {
		t.Fatalf("discover: status %d body %s", rec.Code, rec.Body.String())
	}
}
