package api

import (
	"net/http"
	"strings"
	"testing"
)

// validRegisterBatchBody is a complete, valid batch registration body: it
// overwrites the pre-existing svc/i-1 and creates the new svc/i-new, so a
// partial write would be visible on both.
const validRegisterBatchBody = `{"service_name":"svc","instances":[` +
	`{"instance_id":"i-1","address":"10.0.0.9:9000","port":9090,"healthy":false,"weight":99,"heartbeat_at":"2026-10-01T12:30:00Z"},` +
	`{"instance_id":"i-new","weight":5,"heartbeat_at":"2026-10-01T12:05:00Z"}]}`

// validWeightBatchBody rewrites the weights of the two existing instances
// svc/i-1 and svc/i-2.
const validWeightBatchBody = `{"updates":[` +
	`{"instance_id":"i-1","weight":50},` +
	`{"instance_id":"i-2","weight":60}]}`

var batchRegisterTargets = []struct {
	name string
	path string
}{
	{"path entry", "/api/v1/services/svc/instances/batch"},
	{"register entry", "/api/v1/register/batch"},
}

// getInstance queries the public full-record entry and fails on non-200.
func getInstance(t *testing.T, router http.Handler, service, id string) map[string]any {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+service+"/instances/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get %s/%s: status %d body %s", service, id, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)["instance"].(map[string]any)
}

func expectInstanceMissing(t *testing.T, router http.Handler, service, id string) {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+service+"/instances/"+id, nil)
	expectErrorShape(t, rec, http.StatusNotFound, "instance_not_found")
}

// assertOriginalRecord verifies through the public query that the instance
// still holds every field written by heartbeatRecord/weightRecord.
func assertOriginalRecord(t *testing.T, router http.Handler, service, id string) {
	t.Helper()
	instance := getInstance(t, router, service, id)
	if instance["address"] != "10.0.0.8:8080" || instance["port"].(float64) != 8080 ||
		instance["healthy"] != true || instance["weight"].(float64) != 10 ||
		instance["heartbeat_at"] != at(0) {
		t.Fatalf("%s/%s changed after rejected request: %v", service, id, instance)
	}
}

func assertWeight(t *testing.T, router http.Handler, service, id string, want float64) {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet,
		"/api/v1/services/"+service+"/instances/"+id+"/weight", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("weight %s/%s: status %d body %s", service, id, rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["weight"].(float64); got != want {
		t.Fatalf("%s/%s weight = %v, want %v", service, id, got, want)
	}
}

func registerPair(t *testing.T, router http.Handler) {
	t.Helper()
	registerInstance(t, router, heartbeatRecord())
	second := heartbeatRecord()
	second["instance_id"] = "i-2"
	registerInstance(t, router, second)
	other := heartbeatRecord()
	other["service_name"] = "other"
	registerInstance(t, router, other)
}

// Content after the single JSON object — a stray closing brace or bracket, a
// second value, any other bytes — rejects the whole batch even when the
// leading object is complete and valid.
func TestBatchRegisterRejectsContentAfterObject(t *testing.T) {
	suffixes := []struct{ name, text string }{
		{"extra closing brace", "}"},
		{"extra closing bracket", "]"},
		{"closing brace after whitespace", " \t\n}\n"},
		{"closing bracket after whitespace", "\r\n ]"},
		{"second object", ` {"service_name":"svc","instances":[]}`},
		{"trailing null", " null"},
		{"trailing array", " []"},
		{"trailing text", " garbage"},
	}
	for _, target := range batchRegisterTargets {
		for _, suffix := range suffixes {
			t.Run(target.name+"/"+suffix.name, func(t *testing.T) {
				router, _ := testRouter(t)
				registerPair(t, router)

				rec := doRawRequest(t, router, http.MethodPost, target.path,
					validRegisterBatchBody+suffix.text)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

				// No partial write: the overwrite target keeps every field,
				// the new instance never appears, other services are intact.
				assertOriginalRecord(t, router, "svc", "i-1")
				assertOriginalRecord(t, router, "other", "i-1")
				expectInstanceMissing(t, router, "svc", "i-new")
			})
		}
	}
}

// An empty body, pure whitespace, null, an array or any other bare value is
// not a JSON object and cannot be rescued by query parameters.
func TestBatchRegisterRejectsNonObjectBodies(t *testing.T) {
	bodies := []struct{ name, raw string }{
		{"empty body", ""},
		{"whitespace only", " \t\r\n "},
		{"null", "null"},
		{"empty array", "[]"},
		{"array with valid entries", `[{"instance_id":"i-new","weight":1,"heartbeat_at":"2026-10-01T12:05:00Z"}]`},
		{"bare string", `"svc"`},
		{"bare number", "42"},
		{"bare boolean", "true"},
	}
	for _, target := range batchRegisterTargets {
		for _, body := range bodies {
			t.Run(target.name+"/"+body.name, func(t *testing.T) {
				router, _ := testRouter(t)
				registerPair(t, router)

				// Query parameters cannot make a missing or non-object body
				// acceptable.
				rec := doRawRequest(t, router, http.MethodPost,
					target.path+"?service_name=svc", body.raw)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

				assertOriginalRecord(t, router, "svc", "i-1")
				assertOriginalRecord(t, router, "other", "i-1")
				expectInstanceMissing(t, router, "svc", "i-new")
			})
		}
	}
}

// JSON whitespace around the single object stays legal on both entries.
func TestBatchRegisterAcceptsWhitespaceAroundObject(t *testing.T) {
	for _, target := range batchRegisterTargets {
		t.Run(target.name, func(t *testing.T) {
			router, _ := testRouter(t)
			registerPair(t, router)

			rec := doRawRequest(t, router, http.MethodPost, target.path,
				" \t\r\n"+validRegisterBatchBody+"\n\t ")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			out := decodeBody(t, rec)
			if out["registered"].(float64) != 2 {
				t.Fatalf("registered = %v, want 2", out["registered"])
			}
			overwritten := getInstance(t, router, "svc", "i-1")
			if overwritten["weight"].(float64) != 99 || overwritten["port"].(float64) != 9090 {
				t.Fatalf("i-1 not overwritten: %v", overwritten)
			}
			created := getInstance(t, router, "svc", "i-new")
			if created["weight"].(float64) != 5 {
				t.Fatalf("i-new not created: %v", created)
			}
		})
	}
}

// Closing braces and brackets inside string fields are content, not trailing
// garbage, and stay legal.
func TestBatchRegisterAcceptsClosersInsideStrings(t *testing.T) {
	for _, target := range batchRegisterTargets {
		t.Run(target.name, func(t *testing.T) {
			router, _ := testRouter(t)
			body := `{"service_name":"svc","instances":[` +
				`{"instance_id":"i-1","address":"a}b]c","weight":3,"heartbeat_at":"2026-10-01T12:05:00Z"},` +
				`{"instance_id":"i-}2]","weight":1,"heartbeat_at":"2026-10-01T12:06:00Z"}]}`
			rec := doRawRequest(t, router, http.MethodPost, target.path, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			if got := getInstance(t, router, "svc", "i-1"); got["address"] != "a}b]c" {
				t.Fatalf("address = %v, want a}b]c", got["address"])
			}
			list := decodeBody(t, doRequest(t, router, http.MethodGet,
				"/api/v1/services/svc/instances", nil))
			ids := instanceIDs(t, list)
			// The instance id with closers must round-trip through the
			// public list as well.
			found := false
			for _, id := range ids {
				if id == "i-}2]" {
					found = true
				}
			}
			if !found {
				t.Fatalf("i-}2] missing from list: %v", ids)
			}
		})
	}
}

// The path-style batch weight update applies the same single-object rule.
func TestBatchWeightRejectsContentAfterObject(t *testing.T) {
	suffixes := []struct{ name, text string }{
		{"extra closing brace", "}"},
		{"extra closing bracket", "]"},
		{"closing brace after whitespace", " \t\n}\n"},
		{"closing bracket after whitespace", "\n ]"},
		{"second object", ` {"updates":[]}`},
		{"trailing null", " null"},
		{"trailing text", " garbage"},
	}
	for _, suffix := range suffixes {
		t.Run(suffix.name, func(t *testing.T) {
			router, _ := testRouter(t)
			registerPair(t, router)

			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/weight",
				validWeightBatchBody+suffix.text)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

			// No partial update across the multiple existing targets.
			assertWeight(t, router, "svc", "i-1", 10)
			assertWeight(t, router, "svc", "i-2", 10)
			assertOriginalRecord(t, router, "other", "i-1")
		})
	}
}

func TestBatchWeightRejectsNonObjectBodies(t *testing.T) {
	bodies := []struct{ name, raw string }{
		{"empty body", ""},
		{"whitespace only", " \t\r\n "},
		{"null", "null"},
		{"empty array", "[]"},
		{"array with valid entries", `[{"instance_id":"i-1","weight":50}]`},
		{"bare string", `"weight"`},
		{"bare number", "42"},
		{"bare boolean", "true"},
	}
	for _, body := range bodies {
		t.Run(body.name, func(t *testing.T) {
			router, _ := testRouter(t)
			registerPair(t, router)

			rec := doRawRequest(t, router, http.MethodPut,
				"/api/v1/services/svc/instances/weight?weight=50", body.raw)
			expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

			assertWeight(t, router, "svc", "i-1", 10)
			assertWeight(t, router, "svc", "i-2", 10)
			assertOriginalRecord(t, router, "other", "i-1")
		})
	}
}

// Whitespace around the object and closing symbols inside string fields stay
// legal on the batch weight entry too.
func TestBatchWeightAcceptsWhitespaceAndStringClosers(t *testing.T) {
	router, _ := testRouter(t)
	registerInstance(t, router, heartbeatRecord())
	closers := heartbeatRecord()
	closers["instance_id"] = "i-}2]"
	registerInstance(t, router, closers)

	body := " \t\n" + `{"updates":[` +
		`{"instance_id":"i-1","weight":7},` +
		`{"instance_id":"i-}2]","weight":"8"}]}` + "\r\n "
	rec := doRawRequest(t, router, http.MethodPut,
		"/api/v1/services/svc/instances/weight", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["updated"].(float64) != 2 {
		t.Fatalf("updated = %v, want 2", decodeBody(t, rec)["updated"])
	}
	assertWeight(t, router, "svc", "i-1", 7)
	list := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/svc/instances", nil))
	found := false
	for _, raw := range list["instances"].([]any) {
		instance := raw.(map[string]any)
		if instance["instance_id"] == "i-}2]" {
			found = true
			if instance["weight"].(float64) != 8 {
				t.Fatalf("i-}2] weight = %v, want 8", instance["weight"])
			}
		}
	}
	if !found {
		t.Fatalf("i-}2] missing from list: %v", list["instances"])
	}
}

// The strict body reader keeps exact integer semantics for port: values above
// 2^53 and at the int64 upper bound round-trip, while out-of-range or
// non-integer ports reject the whole batch.
func TestBatchRegisterPortPrecisionRawBody(t *testing.T) {
	for _, target := range batchRegisterTargets {
		t.Run(target.name, func(t *testing.T) {
			router, _ := testRouter(t)
			body := `{"service_name":"svc","instances":[` +
				`{"instance_id":"i-big","weight":10,"port":9007199254740993,"heartbeat_at":"2026-10-01T12:00:00Z"},` +
				`{"instance_id":"i-max","weight":5,"port":9223372036854775807,"heartbeat_at":"2026-10-01T12:00:00Z"}]}`
			rec := doRawRequest(t, router, http.MethodPost, target.path, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"port":9007199254740993`) ||
				!strings.Contains(rec.Body.String(), `"port":9223372036854775807`) {
				t.Fatalf("inexact ports in %s", rec.Body.String())
			}
			for id, want := range map[string]string{
				"i-big": `"port":9007199254740993`,
				"i-max": `"port":9223372036854775807`,
			} {
				rec := doRequest(t, router, http.MethodGet,
					"/api/v1/services/svc/instances/"+id, nil)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("get %s: status %d body %s, want %s", id, rec.Code, rec.Body.String(), want)
				}
			}
		})
	}
}

func TestBatchRegisterRejectsBadPortRawBody(t *testing.T) {
	ports := []struct{ name, literal string }{
		{"beyond int64", "9223372036854775808"},
		{"non-integer", "1.5"},
		{"non-numeric string", `"eight thousand"`},
	}
	for _, target := range batchRegisterTargets {
		for _, port := range ports {
			t.Run(target.name+"/"+port.name, func(t *testing.T) {
				router, _ := testRouter(t)
				registerPair(t, router)

				body := `{"service_name":"svc","instances":[` +
					`{"instance_id":"i-ok","weight":10,"port":8080,"heartbeat_at":"2026-10-01T12:00:00Z"},` +
					`{"instance_id":"i-bad","weight":10,"port":` + port.literal + `,"heartbeat_at":"2026-10-01T12:00:00Z"}]}`
				rec := doRawRequest(t, router, http.MethodPost, target.path, body)
				expectErrorShape(t, rec, http.StatusBadRequest, "invalid_parameter")

				// The whole batch is rejected: the valid entry never appears
				// and the pre-existing record is untouched.
				expectInstanceMissing(t, router, "svc", "i-ok")
				expectInstanceMissing(t, router, "svc", "i-bad")
				assertOriginalRecord(t, router, "svc", "i-1")
			})
		}
	}
}
