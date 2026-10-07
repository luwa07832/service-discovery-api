package api

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the cross-service rollback contract of the two entries
// that delete lost records across service boundaries:
// POST /api/v1/discover/batch and the all-services mode of
// POST /api/v1/cleanup (service_name omitted). When the storage layer fails
// after one service's lost record has already been deleted inside the single
// cleanup transaction but before a later service's record is reached, every
// deletion rolls back: both entries answer HTTP 503 with only the top-level
// storage_unavailable error object, no record anywhere changes, and the
// identical request succeeds once the fault clears. Fixed evaluation points
// keep every verdict independent of the wall clock. Every check runs against
// a real SQLite file and is repeated after closing and reopening it.

// crossDeleteFixture seeds two services the request evaluates and one service
// it never names. alpha and beta each carry a strictly lost record, a record
// exactly on the lost boundary and an unhealthy-but-fresh record; beta also
// carries a healthy fresh candidate. The named-but-empty gamma service carries
// one strictly lost record that the batch deletes and answers with an empty
// instance list; the unnamed delta service carries one strictly lost record
// that neither a scoped batch nor its failure may read or delete.
func seedCrossDeleteFixture(t *testing.T, router http.Handler) {
	t.Helper()
	instances := []map[string]any{
		{"service_name": "alpha", "instance_id": "lost-a", "address": "10.7.0.1:7101",
			"port": 7101, "healthy": true, "weight": 11, "heartbeat_at": at(0)},
		{"service_name": "alpha", "instance_id": "edge-a", "healthy": true,
			"weight": 8, "heartbeat_at": at(5)},
		{"service_name": "alpha", "instance_id": "sick-a", "healthy": false,
			"weight": 4, "heartbeat_at": at(8)},
		{"service_name": "beta", "instance_id": "lost-b", "address": "10.8.0.1:7201",
			"port": 7201, "healthy": true, "weight": 7, "heartbeat_at": at(1)},
		{"service_name": "beta", "instance_id": "fresh-b", "healthy": true,
			"weight": 9, "heartbeat_at": at(9)},
		{"service_name": "gamma", "instance_id": "lost-g", "address": "10.9.0.1:7301",
			"port": 7301, "healthy": true, "weight": 3, "heartbeat_at": at(0)},
		{"service_name": "delta", "instance_id": "lost-d", "address": "10.10.0.1:7401",
			"port": 7401, "healthy": true, "weight": 6, "heartbeat_at": at(0)},
	}
	for _, instance := range instances {
		registerInstance(t, router, instance)
	}
}

var crossDeleteWant = map[string]map[string]any{
	"alpha/lost-a": {"service_name": "alpha", "instance_id": "lost-a",
		"address": "10.7.0.1:7101", "port": float64(7101), "healthy": true,
		"weight": float64(11), "heartbeat_at": at(0)},
	"alpha/edge-a": {"service_name": "alpha", "instance_id": "edge-a",
		"address": "", "port": float64(0), "healthy": true,
		"weight": float64(8), "heartbeat_at": at(5)},
	"alpha/sick-a": {"service_name": "alpha", "instance_id": "sick-a",
		"address": "", "port": float64(0), "healthy": false,
		"weight": float64(4), "heartbeat_at": at(8)},
	"beta/lost-b": {"service_name": "beta", "instance_id": "lost-b",
		"address": "10.8.0.1:7201", "port": float64(7201), "healthy": true,
		"weight": float64(7), "heartbeat_at": at(1)},
	"beta/fresh-b": {"service_name": "beta", "instance_id": "fresh-b",
		"address": "", "port": float64(0), "healthy": true,
		"weight": float64(9), "heartbeat_at": at(9)},
	"gamma/lost-g": {"service_name": "gamma", "instance_id": "lost-g",
		"address": "10.9.0.1:7301", "port": float64(7301), "healthy": true,
		"weight": float64(3), "heartbeat_at": at(0)},
	"delta/lost-d": {"service_name": "delta", "instance_id": "lost-d",
		"address": "10.10.0.1:7401", "port": float64(7401), "healthy": true,
		"weight": float64(6), "heartbeat_at": at(0)},
}

// assertCrossDeleteRolledBack pins the state every failed cross-service
// deletion must leave: all seven records keep their pre-request values field
// by field, the two lost records the transaction already reached are back,
// the named-but-empty service's lost record is back and the unnamed
// service's lost record is untouched.
func assertCrossDeleteRolledBack(t *testing.T, phase string, router http.Handler) {
	t.Helper()
	for key, want := range crossDeleteWant {
		service, id, _ := strings.Cut(key, "/")
		expectRecordFields(t, phase+" GET /api/v1/services/"+service+"/instances/"+id,
			service, id, getFullRecord(t, router, service, id), want)
	}
	alpha := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/alpha/instances", nil))
	expectInstanceIDSet(t, phase+" GET /api/v1/services/alpha/instances", alpha,
		"lost-a", "edge-a", "sick-a")
	beta := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/beta/instances", nil))
	expectInstanceIDSet(t, phase+" GET /api/v1/services/beta/instances", beta,
		"lost-b", "fresh-b")
	gamma := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/gamma/instances", nil))
	expectInstanceIDSet(t, phase+" GET /api/v1/services/gamma/instances", gamma,
		"lost-g")
	delta := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/services/delta/instances", nil))
	expectInstanceIDSet(t, phase+" GET /api/v1/services/delta/instances", delta,
		"lost-d")
}

func TestBatchDiscoverCrossServiceDeleteMidFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch-discover-cross-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })
	seedCrossDeleteFixture(t, router)

	body := map[string]any{
		"service_names":     []any{"alpha", "beta", "gamma"},
		"evaluate_at":       at(10),
		"heartbeat_timeout": "5m",
	}
	const entry = "POST /api/v1/discover/batch"

	// Deletion order follows the request order: alpha/lost-a is already
	// deleted inside the transaction when beta/lost-b aborts, so the whole
	// cross-service batch must roll back. The empty third service keeps its
	// place in the response order and yields an empty instance list.
	clearFault := failDeleteOf(t, path, "lost-b")

	for attempt := 1; attempt <= 2; attempt++ {
		rec := batchDiscover(t, router, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s attempt-%d: status %d body %s", entry, attempt, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		if len(out) != 1 {
			t.Fatalf("%s attempt-%d: error body must contain only the error object: %v", entry, attempt, out)
		}
		errObj := out["error"].(map[string]any)
		if errObj["code"] != "storage_unavailable" {
			t.Fatalf("%s attempt-%d: code = %v, want storage_unavailable", entry, attempt, errObj["code"])
		}
		message, _ := errObj["message"].(string)
		if message == "" {
			t.Fatalf("%s attempt-%d: error message must be non-empty", entry, attempt)
		}
		for _, leak := range []string{"simulated delete failure", "sql", "goroutine", ".go"} {
			if strings.Contains(strings.ToLower(message), leak) {
				t.Fatalf("%s attempt-%d: message leaks internals %q: %q", entry, attempt, leak, message)
			}
		}
		if _, present := out["services"]; present {
			t.Fatalf("%s attempt-%d: failed batch must not return services: %v", entry, attempt, out)
		}
		assertCrossDeleteRolledBack(t, fmtAttemptPhase(entry, attempt), router)
	}

	// The rollback survives closing and reopening the same database file.
	router, st = reopenRouterAtPath(t, path, st)
	assertCrossDeleteRolledBack(t, entry+" after-reopen", router)

	// With the fault cleared, the identical request succeeds: only strictly
	// past-boundary records were deleted, the boundary record and every
	// unhealthy-but-fresh record are kept, and gamma is returned empty
	// without being read or touched.
	clearFault()
	rec := batchDiscover(t, router, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s retry: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["evaluate_at"] != at(10) {
		t.Fatalf("%s retry: evaluate_at = %v, want %s", entry, out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("%s retry: heartbeat_timeout = %v, want 300", entry, out["heartbeat_timeout"])
	}
	services := out["services"].([]any)
	if len(services) != 3 {
		t.Fatalf("%s retry: services len = %d, want 3", entry, len(services))
	}
	first := services[0].(map[string]any)
	second := services[1].(map[string]any)
	third := services[2].(map[string]any)
	if first["service_name"] != "alpha" || second["service_name"] != "beta" ||
		third["service_name"] != "gamma" {
		t.Fatalf("%s retry: service order = %v, %v, %v; want alpha, beta, gamma",
			entry, first["service_name"], second["service_name"], third["service_name"])
	}
	alphaHits := first["instances"].([]any)
	if len(alphaHits) != 1 {
		t.Fatalf("%s retry: alpha hits = %v, want only the boundary record", entry, alphaHits)
	}
	alphaHit := alphaHits[0].(map[string]any)
	if alphaHit["instance_id"] != "edge-a" {
		t.Fatalf("%s retry: alpha hit = %v, want edge-a", entry, alphaHit["instance_id"])
	}
	expectRecordFields(t, entry+" retry alpha hit", "alpha", "edge-a", alphaHit,
		crossDeleteWant["alpha/edge-a"])
	betaHits := second["instances"].([]any)
	if len(betaHits) != 1 {
		t.Fatalf("%s retry: beta hits = %v, want one healthy fresh record", entry, betaHits)
	}
	betaHit := betaHits[0].(map[string]any)
	if betaHit["instance_id"] != "fresh-b" {
		t.Fatalf("%s retry: beta hit = %v, want fresh-b", entry, betaHit["instance_id"])
	}
	expectRecordFields(t, entry+" retry beta hit", "beta", "fresh-b", betaHit,
		crossDeleteWant["beta/fresh-b"])
	if gammaEmpty, ok := third["instances"].([]any); !ok || len(gammaEmpty) != 0 {
		t.Fatalf("%s retry: unnamed-service slot must be an empty list: %v", entry, third["instances"])
	}

	// The committed deletions survive a restart: alpha and beta lost
	// records answer 404, while every survivor keeps its original values.
	// gamma/lost-g was also deleted because gamma was a named service.
	router, st = reopenRouterAtPath(t, path, st)
	for _, key := range []string{"alpha/lost-a", "beta/lost-b", "gamma/lost-g"} {
		service, id, _ := strings.Cut(key, "/")
		missed := doRequest(t, router, http.MethodGet,
			"/api/v1/services/"+service+"/instances/"+id, nil)
		expectOnlyTopLevelError(t, missed, http.StatusNotFound, "instance_not_found")
		missedHeartbeat := doRequest(t, router, http.MethodGet,
			"/api/v1/services/"+service+"/instances/"+id+"/heartbeat", nil)
		expectOnlyTopLevelError(t, missedHeartbeat, http.StatusNotFound, "instance_not_found")
	}
	for _, key := range []string{"alpha/edge-a", "alpha/sick-a", "beta/fresh-b"} {
		service, id, _ := strings.Cut(key, "/")
		expectRecordFields(t, entry+" after-reopen "+key, service, id,
			getFullRecord(t, router, service, id), crossDeleteWant[key])
	}

	// Repeating the identical discovery returns the identical result.
	rec = batchDiscover(t, router, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s repeat: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	repeated := decodeBody(t, rec)
	repeatedServices := repeated["services"].([]any)
	if len(repeatedServices) != 3 {
		t.Fatalf("%s repeat: services len = %d, want 3", entry, len(repeatedServices))
	}
	repeatedFirst := repeatedServices[0].(map[string]any)
	repeatedSecond := repeatedServices[1].(map[string]any)
	repeatedThird := repeatedServices[2].(map[string]any)
	if repeatedFirst["instances"].([]any)[0].(map[string]any)["instance_id"] != "edge-a" ||
		repeatedSecond["instances"].([]any)[0].(map[string]any)["instance_id"] != "fresh-b" {
		t.Fatalf("%s repeat: candidate ids changed: %v", entry, repeatedServices)
	}
	if empty, ok := repeatedThird["instances"].([]any); !ok || len(empty) != 0 {
		t.Fatalf("%s repeat: gamma slot must stay empty: %v", entry, repeatedThird["instances"])
	}

	// A follow-up cleanup scoped to alpha removes nothing: the boundary
	// record is exactly at heartbeat_at + timeout and the other survivor
	// is unhealthy but fresh, so removed is 0 with an empty instance list.
	code, cleanupOut := cleanupRequest(t, router, "/api/v1/cleanup", map[string]any{
		"service_name": "alpha", "evaluate_at": at(10), "heartbeat_timeout": "5m",
	})
	if code != http.StatusOK {
		t.Fatalf("%s follow-up cleanup: status %d body %v", entry, code, cleanupOut)
	}
	if cleanupOut["removed"] != float64(0) {
		t.Fatalf("%s follow-up cleanup: removed = %v, want 0", entry, cleanupOut["removed"])
	}
	if list, ok := cleanupOut["instances"].([]any); !ok || len(list) != 0 {
		t.Fatalf("%s follow-up cleanup: instances = %v, want []", entry, cleanupOut["instances"])
	}
	// The record the batch deleted in another named service stays deleted:
	// the scoped cleanup must not recreate it.
	missed := doRequest(t, router, http.MethodGet,
		"/api/v1/services/gamma/instances/lost-g", nil)
	expectOnlyTopLevelError(t, missed, http.StatusNotFound, "instance_not_found")
	// The service the batch never named is still completely unchanged.
	if _, found, err := st.GetInstance("delta", "lost-d"); err != nil || !found {
		t.Fatalf("%s follow-up cleanup: unnamed service lost record changed: found=%v err=%v",
			entry, found, err)
	}
}

func TestCleanupAllServicesDeleteMidFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-all-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })
	seedCrossDeleteFixture(t, router)

	// All-services mode: service_name omitted from both the query string and
	// the JSON body. ListAllInstances orders deletions by service name then
	// instance id, so alpha/lost-a is already deleted inside the transaction
	// when beta/lost-b aborts and gamma/lost-g is never reached.
	target := "/api/v1/cleanup?evaluate_at=" + at(10) + "&heartbeat_timeout=5m"
	const entry = "POST /api/v1/cleanup (all services)"
	clearFault := failDeleteOf(t, path, "lost-b")

	for attempt := 1; attempt <= 2; attempt++ {
		rec := doRequest(t, router, http.MethodPost, target, nil)
		expectOnlyTopLevelError(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
		assertCrossDeleteRolledBack(t, fmtAttemptPhase(entry, attempt), router)
	}

	// The rollback persists across closing and reopening the same file.
	router, st = reopenRouterAtPath(t, path, st)
	assertCrossDeleteRolledBack(t, entry+" after-reopen", router)

	// With the fault cleared, the identical request removes every strictly
	// lost record across all services in one transaction, including the
	// delta record no batch ever named; boundary and unhealthy-but-fresh
	// records survive with their original values.
	clearFault()
	rec := doRequest(t, router, http.MethodPost, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s retry: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["evaluate_at"] != at(10) {
		t.Fatalf("%s retry: evaluate_at = %v, want %s", entry, out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("%s retry: heartbeat_timeout = %v, want 300", entry, out["heartbeat_timeout"])
	}
	if out["removed"] != float64(4) {
		t.Fatalf("%s retry: removed = %v, want 4", entry, out["removed"])
	}
	removed := out["instances"].([]any)
	if len(removed) != 4 {
		t.Fatalf("%s retry: instances len = %d, want 4", entry, len(removed))
	}
	// All-services cleanup orders the deleted records by service name and
	// then instance id, so delta sorts between beta and gamma.
	wantRemoved := []string{"alpha/lost-a", "beta/lost-b", "delta/lost-d", "gamma/lost-g"}
	for i, key := range wantRemoved {
		service, id, _ := strings.Cut(key, "/")
		got := removed[i].(map[string]any)
		if got["service_name"] != service || got["instance_id"] != id {
			t.Fatalf("%s retry: removed[%d] = %v/%v, want %s sorted by service then instance",
				entry, i, got["service_name"], got["instance_id"], key)
		}
		expectRecordFields(t, entry+" retry removed "+key, service, id, got, crossDeleteWant[key])
	}

	// The committed deletions survive a restart; the deleted full records
	// answer 404, and every survivor keeps its pre-request field values.
	router, st = reopenRouterAtPath(t, path, st)
	for _, key := range wantRemoved {
		service, id, _ := strings.Cut(key, "/")
		missed := doRequest(t, router, http.MethodGet,
			"/api/v1/services/"+service+"/instances/"+id, nil)
		expectOnlyTopLevelError(t, missed, http.StatusNotFound, "instance_not_found")
	}
	for _, key := range []string{"alpha/edge-a", "alpha/sick-a", "beta/fresh-b"} {
		service, id, _ := strings.Cut(key, "/")
		expectRecordFields(t, entry+" after-reopen "+key, service, id,
			getFullRecord(t, router, service, id), crossDeleteWant[key])
	}

	// Repeating the identical all-services cleanup removes nothing: every
	// surviving record is on the boundary or unhealthy but fresh.
	rec = doRequest(t, router, http.MethodPost, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s repeat: status %d body %s", entry, rec.Code, rec.Body.String())
	}
	repeated := decodeBody(t, rec)
	if repeated["removed"] != float64(0) {
		t.Fatalf("%s repeat: removed = %v, want 0", entry, repeated["removed"])
	}
	if list, ok := repeated["instances"].([]any); !ok || len(list) != 0 {
		t.Fatalf("%s repeat: instances = %v, want []", entry, repeated["instances"])
	}
	if !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("%s repeat: instances must serialize as an empty array: %s", entry, rec.Body.String())
	}
}

// fmtAttemptPhase labels a rollback assertion with the request and attempt.
func fmtAttemptPhase(entry string, attempt int) string {
	return entry + " attempt-" + itoa(attempt)
}

// itoa formats a small positive attempt counter without pulling in strconv
// for the test helpers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
