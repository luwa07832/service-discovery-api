package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// decodeExactBody decodes a response keeping numbers as json.Number, so a
// port above 2^53 is checked digit-by-digit instead of through float64.
func decodeExactBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	decoder := json.NewDecoder(rec.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func exactPort(t *testing.T, instance map[string]any, want int64) {
	t.Helper()
	port, ok := instance["port"].(json.Number)
	if !ok {
		t.Fatalf("port is not a JSON number: %v", instance)
	}
	if port.String() != strconv.FormatInt(want, 10) {
		t.Fatalf("port = %s, want %d", port.String(), want)
	}
}

func portRegisterBody(portToken string) string {
	return fmt.Sprintf(`{"service_name":"svc","instance_id":"i-1","port":%s,"weight":1,"heartbeat_at":"2026-10-01T12:00:00Z"}`, portToken)
}

func TestPortExactIntegerForms(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  int64
	}{
		{"plain integer", `8080`, 8080},
		{"trailing zero fraction", `8080.0`, 8080},
		{"exponent form", `8.08e3`, 8080},
		{"zero", `0`, 0},
		{"above float53", `9007199254740993`, 9007199254740993},
		{"int64 upper bound", `9223372036854775807`, 9223372036854775807},
		{"string integer", `"8080"`, 8080},
		{"string with whitespace", `"  9223372036854775807  "`, 9223372036854775807},
		{"string fraction", `"8080.0"`, 8080},
		{"string exponent", `"8.08e3"`, 8080},
		{"string above float53", `"9007199254740993"`, 9007199254740993},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register", portRegisterBody(tc.token))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			out := decodeExactBody(t, rec)
			instance, ok := out["instance"].(map[string]any)
			if !ok {
				t.Fatalf("no instance in response: %s", rec.Body.String())
			}
			exactPort(t, instance, tc.want)

			stored, found, err := st.GetInstance("svc", "i-1")
			if err != nil || !found {
				t.Fatalf("stored record: found=%v err=%v", found, err)
			}
			if stored.Port != tc.want {
				t.Fatalf("stored port = %d, want %d", stored.Port, tc.want)
			}
		})
	}
}

func TestPortExactFromQueryParameter(t *testing.T) {
	router, st := testRouter(t)
	rec := doRequest(t, router, http.MethodPost,
		"/api/v1/register?service_name=svc&instance_id=i-1&weight=1&heartbeat_at=2026-10-01T12:00:00Z&port=9007199254740993",
		nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	exactPort(t, decodeExactBody(t, rec)["instance"].(map[string]any), 9007199254740993)
	stored, found, _ := st.GetInstance("svc", "i-1")
	if !found || stored.Port != 9007199254740993 {
		t.Fatalf("stored port = %d, found=%v", stored.Port, found)
	}
}

func TestPortRejectedValues(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"rounded-looking fraction", `9007199254740992.5`},
		{"int64 overflow", `9223372036854775808`},
		{"string int64 overflow", `"9223372036854775808"`},
		{"negative", `-1`},
		{"fractional", `1.5`},
		{"string fractional", `"1.5"`},
		{"explicit null", `null`},
		{"boolean true", `true`},
		{"boolean false", `false`},
		{"array", `[8080]`},
		{"object", `{"p":8080}`},
		{"empty string", `""`},
		{"blank string", `"   "`},
		{"unparseable string", `"abc"`},
		{"NaN text", `"NaN"`},
		{"Infinity text", `"Infinity"`},
		{"negative Infinity text", `"-Infinity"`},
		{"huge exponent number", `1e400`},
		{"huge exponent string", `"1e400"`},
		{"hex string", `"0x1F90"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st := testRouter(t)
			registerInstance(t, router, heartbeatRecord())

			// A full-overwrite update carrying the invalid port must fail
			// and leave the existing record untouched.
			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/i-1",
				fmt.Sprintf(`{"port":%s,"weight":20,"heartbeat_at":"2026-10-01T12:05:00Z"}`, tc.token))
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

			stored, found, _ := st.GetInstance("svc", "i-1")
			if !found {
				t.Fatalf("record lost after invalid port")
			}
			if stored.Port != 8080 || stored.Weight != 10 ||
				!stored.HeartbeatAt.Equal(mustParseTime(baseTime)) {
				t.Fatalf("record changed after invalid port: %+v", stored)
			}
		})
	}
}

func TestPortBodyDoesNotFallBackToQuery(t *testing.T) {
	router, st := testRouter(t)

	// An invalid body port is an error even when the query carries a valid
	// one: the body wins and there is no fallback to the other source.
	rec := doRawRequest(t, router, http.MethodPost,
		"/api/v1/register?port=8080",
		`{"service_name":"svc","instance_id":"i-1","port":"abc","weight":1,"heartbeat_at":"2026-10-01T12:00:00Z"}`)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-1"); found {
		t.Fatalf("invalid body port created a record")
	}

	// A valid body port wins over a conflicting query port.
	rec = doRawRequest(t, router, http.MethodPost,
		"/api/v1/register?port=1",
		`{"service_name":"svc","instance_id":"i-1","port":9007199254740993,"weight":1,"heartbeat_at":"2026-10-01T12:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	exactPort(t, decodeExactBody(t, rec)["instance"].(map[string]any), 9007199254740993)
}

func TestPortBatchExactOrderAndAtomicity(t *testing.T) {
	router, st := testRouter(t)

	rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register/batch", `{
		"service_name": "svc",
		"instances": [
			{"instance_id": "i-1", "port": 9223372036854775807, "weight": 1, "heartbeat_at": "2026-10-01T12:00:00Z"},
			{"instance_id": "i-2", "port": 9007199254740993, "weight": 1, "heartbeat_at": "2026-10-01T12:00:00Z"},
			{"instance_id": "i-3", "port": "8.08e3", "weight": 1, "heartbeat_at": "2026-10-01T12:00:00Z"}
		]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeExactBody(t, rec)
	if out["registered"].(json.Number).String() != "3" {
		t.Fatalf("registered = %v", out["registered"])
	}
	entries := out["instances"].([]any)
	if len(entries) != 3 {
		t.Fatalf("instances len = %d", len(entries))
	}
	wantIDs := []string{"i-1", "i-2", "i-3"}
	wantPorts := []int64{9223372036854775807, 9007199254740993, 8080}
	for i := range entries {
		entry := entries[i].(map[string]any)
		if entry["instance_id"] != wantIDs[i] {
			t.Fatalf("entry %d id = %v, want %s", i, entry["instance_id"], wantIDs[i])
		}
		exactPort(t, entry, wantPorts[i])
		stored, found, _ := st.GetInstance("svc", wantIDs[i])
		if !found || stored.Port != wantPorts[i] {
			t.Fatalf("stored %s port = %d, found=%v", wantIDs[i], stored.Port, found)
		}
	}

	// One invalid port rejects the whole batch on the path entry as well:
	// nothing is created and the existing records keep their ports.
	rec = doRawRequest(t, router, http.MethodPost, "/api/v1/services/svc/instances/batch", `{
		"instances": [
			{"instance_id": "i-9", "port": 1, "weight": 1, "heartbeat_at": "2026-10-01T12:00:00Z"},
			{"instance_id": "i-1", "port": 9223372036854775808, "weight": 1, "heartbeat_at": "2026-10-01T12:00:00Z"}
		]}`)
	expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")
	if _, found, _ := st.GetInstance("svc", "i-9"); found {
		t.Fatalf("invalid batch created i-9")
	}
	stored, found, _ := st.GetInstance("svc", "i-1")
	if !found || stored.Port != 9223372036854775807 {
		t.Fatalf("i-1 changed by invalid batch: %+v", stored)
	}
}

func TestPortSurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register", portRegisterBody("9007199254740993"))
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
	t.Cleanup(func() { reopened.Close() })
	stored, found, err := reopened.GetInstance("svc", "i-1")
	if err != nil || !found {
		t.Fatalf("stored record: found=%v err=%v", found, err)
	}
	if stored.Port != 9007199254740993 {
		t.Fatalf("port after reopen = %d, want 9007199254740993", stored.Port)
	}

	rec = doRequest(t, NewRouter(reopened), http.MethodGet,
		"/api/v1/services/svc/instances/i-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"port":9007199254740993`) {
		t.Fatalf("response port not exact: %s", rec.Body.String())
	}
	exactPort(t, decodeExactBody(t, rec)["instance"].(map[string]any), 9007199254740993)
}

func TestPortPreservedByAttributeUpdates(t *testing.T) {
	router, st := testRouter(t)
	rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register", portRegisterBody("9007199254740993"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, router, http.MethodPost,
		"/api/v1/services/svc/instances/i-1/heartbeat",
		map[string]any{"heartbeat_at": "2026-10-01T12:05:00Z"})
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d body %s", rec.Code, rec.Body.String())
	}
	exactPort(t, decodeExactBody(t, rec)["instance"].(map[string]any), 9007199254740993)

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/weight",
		map[string]any{"weight": 20})
	if rec.Code != http.StatusOK {
		t.Fatalf("weight status = %d body %s", rec.Code, rec.Body.String())
	}
	exactPort(t, decodeExactBody(t, rec)["instance"].(map[string]any), 9007199254740993)

	rec = doRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/i-1/health",
		map[string]any{"healthy": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d body %s", rec.Code, rec.Body.String())
	}
	exactPort(t, decodeExactBody(t, rec)["instance"].(map[string]any), 9007199254740993)

	stored, found, _ := st.GetInstance("svc", "i-1")
	if !found || stored.Port != 9007199254740993 {
		t.Fatalf("stored port = %d, found=%v", stored.Port, found)
	}
}

func TestPortExactInListAndDiscover(t *testing.T) {
	router, _ := testRouter(t)
	rec := doRawRequest(t, router, http.MethodPost, "/api/v1/register",
		`{"service_name":"svc","instance_id":"i-1","port":9223372036854775807,"healthy":true,"weight":1,"heartbeat_at":"2026-10-01T12:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":9223372036854775807`) {
		t.Fatalf("list port not exact: %d %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/discover?evaluate_at=2026-10-01T12:10:00Z&heartbeat_timeout=30m", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":9223372036854775807`) {
		t.Fatalf("discover port not exact: %d %s", rec.Code, rec.Body.String())
	}
}
