package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// cross_service_delete_failure installs a trigger that aborts every DELETE
// targeting the named service. The trigger lives in the database file itself,
// so a second connection preparing the schema affects the store connection,
// letting a batch fail after earlier DELETE statements already ran inside the
// same (still uncommitted) transaction.
const crossServiceDeleteFailureTrigger = `
CREATE TRIGGER cross_service_delete_failure
BEFORE DELETE ON service_instances
WHEN OLD.service_name = 'beta'
BEGIN
	SELECT RAISE(ABORT, 'forced cross-service delete failure');
END;`

// sqliteAuxConnection opens a second connection to the same database file,
// without going through the store package.
func sqliteAuxConnection(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open aux sqlite %s: %v", path, err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping aux sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func armCrossServiceDeleteFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(crossServiceDeleteFailureTrigger); err != nil {
		t.Fatalf("install delete failure trigger: %v", err)
	}
}

func disarmCrossServiceDeleteFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DROP TRIGGER cross_service_delete_failure`); err != nil {
		t.Fatalf("drop delete failure trigger: %v", err)
	}
}

type crossServiceRecord struct {
	service string
	id      string
	want    map[string]any
}

// crossServiceFixtureRecords seeds two services with lost, boundary and
// unhealthy-but-fresh instances plus an unrequested third service. evaluate_at
// 12:10 with a 5m timeout puts the lost boundary exactly at 12:05.
func crossServiceFixtureRecords() []crossServiceRecord {
	return []crossServiceRecord{
		{"alpha", "i-lost", map[string]any{
			"service_name": "alpha", "instance_id": "i-lost", "address": "10.0.0.1:8080",
			"port": float64(8080), "healthy": true, "weight": float64(10), "heartbeat_at": at(0)}},
		{"alpha", "i-edge", map[string]any{
			"service_name": "alpha", "instance_id": "i-edge", "address": "",
			"port": float64(0), "healthy": true, "weight": float64(10), "heartbeat_at": at(5)}},
		{"alpha", "i-tie", map[string]any{
			"service_name": "alpha", "instance_id": "i-tie", "address": "10.0.0.2:7000",
			"port": float64(7000), "healthy": true, "weight": float64(10), "heartbeat_at": at(6)}},
		{"alpha", "i-mid", map[string]any{
			"service_name": "alpha", "instance_id": "i-mid", "address": "10.0.0.3:6000",
			"port": float64(6000), "healthy": true, "weight": float64(5), "heartbeat_at": at(8)}},
		{"alpha", "i-sick", map[string]any{
			"service_name": "alpha", "instance_id": "i-sick", "address": "10.0.0.4:5000",
			"port": float64(5000), "healthy": false, "weight": float64(3), "heartbeat_at": at(9)}},
		{"beta", "i-lost", map[string]any{
			"service_name": "beta", "instance_id": "i-lost", "address": "10.1.0.1:8080",
			"port": float64(8080), "healthy": true, "weight": float64(9), "heartbeat_at": at(1)}},
		{"beta", "i-keep", map[string]any{
			"service_name": "beta", "instance_id": "i-keep", "address": "10.1.0.2:8080",
			"port": float64(8080), "healthy": true, "weight": float64(7), "heartbeat_at": at(9)}},
		{"gamma", "i-lost", map[string]any{
			"service_name": "gamma", "instance_id": "i-lost", "address": "10.2.0.1:9000",
			"port": float64(9000), "healthy": true, "weight": float64(20), "heartbeat_at": at(0)}},
	}
}

func seedCrossServiceFixture(t *testing.T, router http.Handler) {
	t.Helper()
	for _, record := range crossServiceFixtureRecords() {
		registerInstance(t, router, record.want)
	}
}

// assertCrossServiceRecordsIntact reads every fixture record through the public
// full-record entry and confirms every field still matches the pre-request
// snapshot.
func assertCrossServiceRecordsIntact(t *testing.T, router http.Handler, label string) {
	t.Helper()
	for _, record := range crossServiceFixtureRecords() {
		got := getFullRecord(t, router, record.service, record.id)
		expectRecordFields(t, label, record.service, record.id, got, record.want)
	}
}

func assertServiceListIDs(t *testing.T, router http.Handler, service string, want []string) {
	t.Helper()
	out := decodeBody(t, doRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/services/%s/instances", service), nil))
	got := instanceIDs(t, out)
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if fmt.Sprint(gotSorted) != fmt.Sprint(wantSorted) {
		t.Fatalf("%s list ids = %v, want %v", service, gotSorted, wantSorted)
	}
}

// assertStorageErrorLeaksNothing verifies the 503 contract: one top-level error
// object whose code and message are plain strings, with no SQL text, trigger
// message, stack frame or file path.
func assertStorageErrorLeaksNothing(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	expectOnlyTopLevelError(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	out := decodeBody(t, rec)
	errObj := out["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	message, _ := errObj["message"].(string)
	if code != "storage_unavailable" || message != "database is not available" {
		t.Fatalf("unexpected error fields: %v", errObj)
	}
	for _, leaked := range []string{"forced", "RAISE", "service_instances", ".db", "goroutine"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("error response leaks %q: %s", leaked, rec.Body.String())
		}
	}
}

// TestBatchDiscoverCrossServiceDeleteRollsBackAndRetries proves the cross-service
// lost-record deletion is one atomic batch: after alpha's lost DELETE has already
// executed, a failure on beta's lost DELETE rolls the whole request back so no
// record in any service changes. Once the fault clears, the same request
// succeeds and only strictly-past-boundary records disappear.
func TestBatchDiscoverCrossServiceDeleteRollsBackAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cross-service-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })
	seedCrossServiceFixture(t, router)

	aux := sqliteAuxConnection(t, path)
	armCrossServiceDeleteFailure(t, aux)

	// service_names names alpha then beta: DeleteInstances runs alpha's lost
	// DELETE first (succeeding inside the transaction) and only afterwards
	// hits beta, where the trigger aborts the whole batch.
	rec := batchDiscover(t, router, map[string]any{
		"service_names":     []any{"alpha", "beta"},
		"evaluate_at":       at(10),
		"heartbeat_timeout": 300,
	})
	assertStorageErrorLeaksNothing(t, rec)

	// Complete-record queries and service lists must match the pre-request
	// state field by field.
	assertCrossServiceRecordsIntact(t, router, "rolled-back batch discover")
	assertServiceListIDs(t, router, "alpha", []string{"i-lost", "i-edge", "i-tie", "i-mid", "i-sick"})
	assertServiceListIDs(t, router, "beta", []string{"i-lost", "i-keep"})
	assertServiceListIDs(t, router, "gamma", []string{"i-lost"})

	// Closing and reopening the same database file keeps the rollback visible.
	router, st = reopenRouterAtPath(t, path, st)
	assertCrossServiceRecordsIntact(t, router, "reopened rolled-back store")

	// Clear the fault and retry with the original parameters, adding a service
	// name that has no records at all.
	disarmCrossServiceDeleteFailure(t, aux)
	rec = batchDiscover(t, router, map[string]any{
		"service_names":     []any{"alpha", "unused-svc", "beta"},
		"evaluate_at":       at(10),
		"heartbeat_timeout": "5m",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("retried batch discover: status %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["evaluate_at"] != "2026-10-01T12:10:00Z" {
		t.Fatalf("evaluate_at = %v, want %s", out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("heartbeat_timeout = %v, want 300", out["heartbeat_timeout"])
	}
	services := out["services"].([]any)
	if len(services) != 3 {
		t.Fatalf("services len = %d, want 3", len(services))
	}
	for index, name := range []string{"alpha", "unused-svc", "beta"} {
		if services[index].(map[string]any)["service_name"] != name {
			t.Fatalf("service %d = %v, want %s (request order)", index,
				services[index].(map[string]any)["service_name"], name)
		}
	}
	// Candidates are healthy and non-lost only, weight descending with the
	// instance id ascending on equal weights.
	alphaHits := services[0].(map[string]any)["instances"].([]any)
	if len(alphaHits) != 3 {
		t.Fatalf("alpha hits = %d, want 3: %v", len(alphaHits), alphaHits)
	}
	for index, id := range []string{"i-edge", "i-tie", "i-mid"} {
		if alphaHits[index].(map[string]any)["instance_id"] != id {
			t.Fatalf("alpha hit %d = %v, want %s", index, alphaHits[index], id)
		}
	}
	expectRecordFields(t, "retry discover", "alpha", "i-edge",
		alphaHits[0].(map[string]any), map[string]any{
			"service_name": "alpha", "instance_id": "i-edge", "address": "",
			"port": float64(0), "healthy": true, "weight": float64(10), "heartbeat_at": at(5)})
	if empty, ok := services[1].(map[string]any)["instances"].([]any); !ok || len(empty) != 0 {
		t.Fatalf("unrequested service must return empty instances: %v",
			services[1].(map[string]any)["instances"])
	}
	betaHits := services[2].(map[string]any)["instances"].([]any)
	if len(betaHits) != 1 || betaHits[0].(map[string]any)["instance_id"] != "i-keep" {
		t.Fatalf("beta hits = %v, want [i-keep]", betaHits)
	}

	// Reopen: deleted records are gone, surviving records keep every old field.
	router, st = reopenRouterAtPath(t, path, st)
	for _, key := range [][2]string{{"alpha", "i-lost"}, {"beta", "i-lost"}} {
		rec := doRequest(t, router, http.MethodGet,
			fmt.Sprintf("/api/v1/services/%s/instances/%s", key[0], key[1]), nil)
		expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")
	}
	for _, record := range []crossServiceRecord{
		{"alpha", "i-edge", crossServiceRecordMap("alpha", "i-edge")},
		{"alpha", "i-tie", crossServiceRecordMap("alpha", "i-tie")},
		{"alpha", "i-mid", crossServiceRecordMap("alpha", "i-mid")},
		{"alpha", "i-sick", crossServiceRecordMap("alpha", "i-sick")},
		{"beta", "i-keep", crossServiceRecordMap("beta", "i-keep")},
	} {
		expectRecordFields(t, "reopened survivor", record.service, record.id,
			getFullRecord(t, router, record.service, record.id), record.want)
	}

	// A repeated discover returns the identical candidates and deletes nothing
	// more; the unrequested gamma service was never touched at any point.
	again := decodeBody(t, batchDiscover(t, router, map[string]any{
		"service_names":     []any{"alpha", "unused-svc", "beta"},
		"evaluate_at":       at(10),
		"heartbeat_timeout": 300,
	}))
	againServices := again["services"].([]any)
	againAlpha := againServices[0].(map[string]any)["instances"].([]any)
	againBeta := againServices[2].(map[string]any)["instances"].([]any)
	if len(againAlpha) != 3 || len(againBeta) != 1 {
		t.Fatalf("repeated discover changed candidates: %v", again)
	}
	if againAlpha[0].(map[string]any)["instance_id"] != "i-edge" ||
		againAlpha[1].(map[string]any)["instance_id"] != "i-tie" ||
		againAlpha[2].(map[string]any)["instance_id"] != "i-mid" {
		t.Fatalf("repeated discover order changed: %v", againAlpha)
	}
	assertServiceListIDs(t, router, "alpha", []string{"i-edge", "i-tie", "i-mid", "i-sick"})
	assertServiceListIDs(t, router, "beta", []string{"i-keep"})
	expectRecordFields(t, "untouched gamma", "gamma", "i-lost",
		getFullRecord(t, router, "gamma", "i-lost"), crossServiceRecordMap("gamma", "i-lost"))
}

// crossServiceRecordMap looks up the seeded expectation for one record.
func crossServiceRecordMap(service, id string) map[string]any {
	for _, record := range crossServiceFixtureRecords() {
		if record.service == service && record.id == id {
			return record.want
		}
	}
	return nil
}

// TestCleanupAllServicesDeleteRollsBackAndRetries exercises the all-services
// cleanup mode (service_name omitted): a DELETE failure on beta after alpha's
// lost record already executed rolls the entire cleanup back, and after the
// fault clears the cleanup removes every strictly-past-boundary record across
// all services and reports the deleted records sorted by service then instance.
func TestCleanupAllServicesDeleteRollsBackAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-all-rollback.db")
	router, st := openRouterAtPath(t, path)
	t.Cleanup(func() { st.Close() })
	seedCrossServiceFixture(t, router)

	aux := sqliteAuxConnection(t, path)
	armCrossServiceDeleteFailure(t, aux)

	// No service_name: the cleanup evaluates every service, deleting across
	// alpha, beta and gamma in one transaction.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"evaluate_at":       at(10),
		"heartbeat_timeout": 300,
	})
	assertStorageErrorLeaksNothing(t, rec)
	assertCrossServiceRecordsIntact(t, router, "rolled-back all-services cleanup")

	router, st = reopenRouterAtPath(t, path, st)
	assertCrossServiceRecordsIntact(t, router, "reopened rolled-back cleanup")

	disarmCrossServiceDeleteFailure(t, aux)
	rec = doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"evaluate_at":       at(10),
		"heartbeat_timeout": "5m",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("retried cleanup: status %d body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["evaluate_at"] != "2026-10-01T12:10:00Z" {
		t.Fatalf("evaluate_at = %v, want %s", out["evaluate_at"], at(10))
	}
	if out["heartbeat_timeout"].(float64) != 300 {
		t.Fatalf("heartbeat_timeout = %v, want 300", out["heartbeat_timeout"])
	}
	if out["removed"].(float64) != 3 {
		t.Fatalf("removed = %v, want 3 (alpha, beta and gamma lost records)", out["removed"])
	}
	removed := out["instances"].([]any)
	if len(removed) != 3 {
		t.Fatalf("removed instances len = %d, want 3", len(removed))
	}
	// Deleted records come back as full records, service name then instance id.
	for index, key := range [][2]string{
		{"alpha", "i-lost"}, {"beta", "i-lost"}, {"gamma", "i-lost"},
	} {
		item := removed[index].(map[string]any)
		if item["service_name"] != key[0] || item["instance_id"] != key[1] {
			t.Fatalf("removed %d = %v/%v, want %s/%s", index,
				item["service_name"], item["instance_id"], key[0], key[1])
		}
		expectRecordFields(t, "cleanup removed", key[0], key[1], item,
			crossServiceRecordMap(key[0], key[1]))
	}

	// Boundary, unhealthy-but-fresh and online records survive untouched.
	for _, key := range [][2]string{
		{"alpha", "i-edge"}, {"alpha", "i-tie"}, {"alpha", "i-mid"},
		{"alpha", "i-sick"}, {"beta", "i-keep"},
	} {
		expectRecordFields(t, "cleanup survivor", key[0], key[1],
			getFullRecord(t, router, key[0], key[1]), crossServiceRecordMap(key[0], key[1]))
	}

	// Reopen: deleted full-record queries are 404, survivors keep old values.
	router, st = reopenRouterAtPath(t, path, st)
	for _, service := range []string{"alpha", "beta", "gamma"} {
		rec := doRequest(t, router, http.MethodGet,
			fmt.Sprintf("/api/v1/services/%s/instances/i-lost", service), nil)
		expectOnlyTopLevelError(t, rec, http.StatusNotFound, "instance_not_found")
	}
	for _, key := range [][2]string{
		{"alpha", "i-edge"}, {"alpha", "i-tie"}, {"alpha", "i-mid"},
		{"alpha", "i-sick"}, {"beta", "i-keep"},
	} {
		expectRecordFields(t, "reopened cleanup survivor", key[0], key[1],
			getFullRecord(t, router, key[0], key[1]), crossServiceRecordMap(key[0], key[1]))
	}

	// Repeating the cleanup removes nothing and reports an empty list.
	rec = doRequest(t, router, http.MethodPost, "/api/v1/cleanup", map[string]any{
		"evaluate_at":       at(10),
		"heartbeat_timeout": 300,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("repeated cleanup: status %d body %s", rec.Code, rec.Body.String())
	}
	out = decodeBody(t, rec)
	if out["removed"].(float64) != 0 {
		t.Fatalf("repeated removed = %v, want 0", out["removed"])
	}
	if list, ok := out["instances"].([]any); !ok || len(list) != 0 {
		t.Fatalf("repeated instances = %v, want []", out["instances"])
	}
	if !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("repeated cleanup must serialize instances as []: %s", rec.Body.String())
	}
}
