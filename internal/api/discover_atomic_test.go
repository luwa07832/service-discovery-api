package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// discoverEntries enumerates the four public single-service discovery
// entries the atomic lost cleanup must hold for.
var discoverEntries = []struct {
	name   string
	method string
	target string
	body   any
}{
	{"get query entry", http.MethodGet,
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=5m", nil},
	{"get path entry", http.MethodGet,
		"/api/v1/services/svc/discover?evaluate_at=" + at(10) + "&heartbeat_timeout=5m", nil},
	{"post query entry", http.MethodPost, "/api/v1/discover", map[string]any{
		"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": "5m"}},
	{"post path entry", http.MethodPost, "/api/v1/services/svc/discover", map[string]any{
		"evaluate_at": at(10), "heartbeat_timeout": 300}},
}

// seedAtomicFixture registers two strictly lost records (one of them
// unhealthy), two healthy fresh candidates, one unhealthy fresh record and
// one record of another service that is old enough to look lost but must
// never be touched by a svc-scoped discovery.
func seedAtomicFixture(t *testing.T, router http.Handler) {
	t.Helper()
	instances := []map[string]any{
		{"service_name": "svc", "instance_id": "lost-a", "address": "10.0.0.1",
			"port": 8001, "healthy": true, "weight": 10, "heartbeat_at": at(0)},
		{"service_name": "svc", "instance_id": "lost-b", "address": "10.0.0.2",
			"port": 8002, "healthy": false, "weight": 9, "heartbeat_at": at(1)},
		{"service_name": "svc", "instance_id": "fresh-high", "address": "10.0.0.3",
			"port": 8003, "healthy": true, "weight": 8, "heartbeat_at": at(9)},
		{"service_name": "svc", "instance_id": "fresh-low", "address": "10.0.0.4",
			"port": 8004, "healthy": true, "weight": 5, "heartbeat_at": at(9)},
		{"service_name": "svc", "instance_id": "sick-fresh", "address": "10.0.0.5",
			"port": 8005, "healthy": false, "weight": 7, "heartbeat_at": at(9)},
		{"service_name": "other", "instance_id": "other-1", "address": "10.0.1.1",
			"port": 9001, "healthy": true, "weight": 1, "heartbeat_at": at(0)},
	}
	for _, instance := range instances {
		registerInstance(t, router, instance)
	}
}

// failDeleteOf installs a trigger that aborts the deletion of one specific
// instance. The lost records are deleted in registration order, so the
// deletion of lost-a is already executed when the deletion of lost-b fails.
// The returned function removes the trigger again.
func failDeleteOf(t *testing.T, dbPath, instanceID string) (clear func()) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	quoted := strings.ReplaceAll(instanceID, "'", "''")
	statement := fmt.Sprintf(`
CREATE TRIGGER discover_test_fail_delete BEFORE DELETE ON service_instances
WHEN OLD.instance_id = '%s'
BEGIN SELECT RAISE(ABORT, 'simulated delete failure'); END`, quoted)
	if _, err := db.Exec(statement); err != nil {
		db.Close()
		t.Fatalf("install trigger: %v", err)
	}
	return func() {
		if _, err := db.Exec("DROP TRIGGER discover_test_fail_delete"); err != nil {
			t.Fatalf("drop trigger: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close second connection: %v", err)
		}
	}
}

// svcRecords reads the svc records through the public list entry and indexes
// them by instance id.
func svcRecords(t *testing.T, router http.Handler) map[string]map[string]any {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet, "/api/v1/services/svc/instances", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public list: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	rawList, ok := out["instances"].([]any)
	if !ok {
		t.Fatalf("instances is not a list: %v", out)
	}
	records := make(map[string]map[string]any, len(rawList))
	for _, raw := range rawList {
		item := raw.(map[string]any)
		records[item["instance_id"].(string)] = item
	}
	return records
}

// wantRecord is the full field set one stored svc record must show through
// the public list entry.
type wantRecord struct {
	address   string
	port      float64
	healthy   bool
	weight    float64
	heartbeat string
}

func expectRecords(t *testing.T, router http.Handler, want map[string]wantRecord) {
	t.Helper()
	records := svcRecords(t, router)
	if len(records) != len(want) {
		t.Fatalf("public list has %d records, want %d: %v", len(records), len(want), records)
	}
	for id, expected := range want {
		item, ok := records[id]
		if !ok {
			t.Fatalf("record %s missing from public list: %v", id, records)
		}
		if item["service_name"] != "svc" || item["address"] != expected.address ||
			item["port"] != expected.port || item["healthy"] != expected.healthy ||
			item["weight"] != expected.weight || item["heartbeat_at"] != expected.heartbeat {
			t.Fatalf("record %s mismatch: %v, want %+v", id, item, expected)
		}
	}
}

func atomicFixtureRecords() map[string]wantRecord {
	return map[string]wantRecord{
		"lost-a":     {"10.0.0.1", 8001, true, 10, at(0)},
		"lost-b":     {"10.0.0.2", 8002, false, 9, at(1)},
		"fresh-high": {"10.0.0.3", 8003, true, 8, at(9)},
		"fresh-low":  {"10.0.0.4", 8004, true, 5, at(9)},
		"sick-fresh": {"10.0.0.5", 8005, false, 7, at(9)},
	}
}

func TestDiscoverLostCleanupIsAtomic(t *testing.T) {
	for _, entry := range discoverEntries {
		t.Run(entry.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "service.db")
			st, err := store.Open(dbPath)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			router := NewRouter(st)
			seedAtomicFixture(t, router)

			// The deletion of lost-b fails after the deletion of lost-a has
			// already been executed inside the cleanup.
			clear := failDeleteOf(t, dbPath, "lost-b")

			rec := doRequest(t, router, entry.method, entry.target, entry.body)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
			}
			out := decodeBody(t, rec)
			if len(out) != 1 {
				t.Fatalf("error body must contain only the error object: %v", out)
			}
			errObj, ok := out["error"].(map[string]any)
			if !ok {
				t.Fatalf("missing error object: %v", out)
			}
			if errObj["code"] != "storage_unavailable" {
				t.Fatalf("code = %v, want storage_unavailable", errObj["code"])
			}
			message, ok := errObj["message"].(string)
			if !ok || message == "" {
				t.Fatalf("message must be a non-empty string: %v", errObj)
			}
			for _, leak := range []string{"simulated delete failure", "sql", "goroutine", ".go"} {
				if strings.Contains(strings.ToLower(message), leak) {
					t.Fatalf("message leaks internals %q: %q", leak, message)
				}
			}
			if _, present := out["instances"]; present {
				t.Fatalf("failed cleanup must not return a candidate list: %v", out)
			}

			// No deletion may have taken effect: every svc record is still
			// stored with its full field set, and the other service is
			// untouched.
			expectRecords(t, router, atomicFixtureRecords())
			other, err := st.ListInstances("other")
			if err != nil || len(other) != 1 || other[0].InstanceID != "other-1" {
				t.Fatalf("other service changed: %v err=%v", other, err)
			}

			// With the failure cleared, the same request removes both lost
			// records and returns the remaining healthy candidates.
			clear()
			rec = doRequest(t, router, entry.method, entry.target, entry.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("retry status = %d, want 200, body %s", rec.Code, rec.Body.String())
			}
			out = decodeBody(t, rec)
			if out["service_name"] != "svc" || out["evaluate_at"] != at(10) ||
				out["heartbeat_timeout"] != float64(300) {
				t.Fatalf("retry response metadata mismatch: %v", out)
			}
			if ids := instanceIDs(t, out); len(ids) != 2 || ids[0] != "fresh-high" || ids[1] != "fresh-low" {
				t.Fatalf("retry ids = %v, want [fresh-high fresh-low]", ids)
			}
			expectRecords(t, router, map[string]wantRecord{
				"fresh-high": {"10.0.0.3", 8003, true, 8, at(9)},
				"fresh-low":  {"10.0.0.4", 8004, true, 5, at(9)},
				"sick-fresh": {"10.0.0.5", 8005, false, 7, at(9)},
			})
			if other, err := st.ListInstances("other"); err != nil || len(other) != 1 {
				t.Fatalf("other service changed by retry: %v err=%v", other, err)
			}

			// Repeating the request yields the same candidates and changes
			// nothing further.
			rec = doRequest(t, router, entry.method, entry.target, entry.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("repeat status = %d, want 200, body %s", rec.Code, rec.Body.String())
			}
			if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 2 || ids[0] != "fresh-high" || ids[1] != "fresh-low" {
				t.Fatalf("repeat ids = %v, want [fresh-high fresh-low]", ids)
			}
			expectRecords(t, router, map[string]wantRecord{
				"fresh-high": {"10.0.0.3", 8003, true, 8, at(9)},
				"fresh-low":  {"10.0.0.4", 8004, true, 5, at(9)},
				"sick-fresh": {"10.0.0.5", 8005, false, 7, at(9)},
			})
		})
	}
}

func TestDiscoverStrictBoundarySurvivesFailedCleanup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	router := NewRouter(st)

	// edge sits exactly on the lost boundary at 12:05, lost sits strictly
	// past it; the cleanup of lost is made to fail.
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "edge", "healthy": true,
		"weight": 2, "heartbeat_at": at(5),
	})
	registerInstance(t, router, map[string]any{
		"service_name": "svc", "instance_id": "lost", "healthy": true,
		"weight": 1, "heartbeat_at": at(0),
	})
	clear := failDeleteOf(t, dbPath, "lost")

	target := "/api/v1/services/svc/discover?evaluate_at=" + at(10) + "&heartbeat_timeout=5m"
	rec := doRequest(t, router, http.MethodGet, target, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	// The boundary record must survive the failed cleanup exactly as stored.
	if _, found, err := st.GetInstance("svc", "edge"); err != nil || !found {
		t.Fatalf("boundary record lost: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetInstance("svc", "lost"); err != nil || !found {
		t.Fatalf("lost record deleted despite failed cleanup: found=%v err=%v", found, err)
	}

	// Once the failure is cleared the boundary record is still online and
	// only the strictly lost record is removed.
	clear()
	rec = doRequest(t, router, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "edge" {
		t.Fatalf("retry ids = %v, want [edge]", ids)
	}
	if _, found, _ := st.GetInstance("svc", "edge"); !found {
		t.Fatalf("boundary record deleted by retry")
	}
	if _, found, _ := st.GetInstance("svc", "lost"); found {
		t.Fatalf("lost record not deleted by retry")
	}
}

func TestDiscoverRejectedParametersChangeNothing(t *testing.T) {
	router, st := testRouter(t)
	seedAtomicFixture(t, router)

	getTargets := []string{
		"/api/v1/discover?service_name=&evaluate_at=" + at(10) + "&heartbeat_timeout=5m", // empty service name
		"/api/v1/discover?service_name=svc&heartbeat_timeout=5m",                         // missing evaluate_at
		"/api/v1/discover?service_name=svc&evaluate_at=not-a-time&heartbeat_timeout=5m",  // bad evaluate_at
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(10),                        // missing timeout
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=0",
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=-5s",
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(10) + "&heartbeat_timeout=abc",
		"/api/v1/discover?service_name=svc&evaluate_at=" + at(8) + "&heartbeat_timeout=5m", // before a heartbeat
	}
	for _, target := range getTargets {
		rec := doRequest(t, router, http.MethodGet, target, nil)
		expectParameterError(t, rec)
		if out := decodeBody(t, rec); len(out) != 1 {
			t.Fatalf("%s: error body must contain only the error object: %v", target, out)
		}
	}

	postBodies := []map[string]any{
		{"service_name": "svc", "heartbeat_timeout": "5m"},
		{"service_name": "svc", "evaluate_at": "soon", "heartbeat_timeout": "5m"},
		{"service_name": "svc", "evaluate_at": at(10)},
		{"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": 0},
		{"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": -30},
		{"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": "abc"},
		{"service_name": "svc", "evaluate_at": at(8), "heartbeat_timeout": "5m"},
	}
	for _, body := range postBodies {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/services/svc/discover", body)
		expectParameterError(t, rec)
		if out := decodeBody(t, rec); len(out) != 1 {
			t.Fatalf("%v: error body must contain only the error object: %v", body, out)
		}
	}

	// A blank or conflicting service_name in the JSON body must not override
	// the path value: the request stays valid and targets the path service.
	// These valid requests run the lost cleanup, so they come after the
	// nothing-changed assertions below.

	// An empty service segment in the path itself stays a parameter error
	// even when the body supplies a valid service name.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/services//discover",
		map[string]any{"service_name": "svc", "evaluate_at": at(10), "heartbeat_timeout": "5m"})
	expectParameterError(t, rec)

	// No rejected request deleted or altered anything, including the two
	// strictly lost records that a valid request would have removed.
	expectRecords(t, router, atomicFixtureRecords())
	if other, err := st.ListInstances("other"); err != nil || len(other) != 1 {
		t.Fatalf("other service changed: %v err=%v", other, err)
	}

	for _, body := range []map[string]any{
		{"service_name": "  ", "evaluate_at": at(10), "heartbeat_timeout": "5m"},
		{"service_name": "other", "evaluate_at": at(10), "heartbeat_timeout": "5m"},
		{"service": "other", "evaluate_at": at(10), "heartbeat_timeout": "5m"},
	} {
		rec := doRequest(t, router, http.MethodPost, "/api/v1/services/svc/discover", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%v: path service must win, got %d %s", body, rec.Code, rec.Body.String())
		}
		out := decodeBody(t, rec)
		if out["service_name"] != "svc" {
			t.Fatalf("%v: targeted %v, want svc", body, out["service_name"])
		}
		if ids := instanceIDs(t, out); len(ids) != 2 || ids[0] != "fresh-high" || ids[1] != "fresh-low" {
			t.Fatalf("%v: ids = %v, want the healthy fresh svc instances", body, ids)
		}
		// The other service must never be read or cleaned by a svc-scoped
		// discovery, even when the body tries to rename it.
		if other, err := st.ListInstances("other"); err != nil || len(other) != 1 {
			t.Fatalf("%v: other service touched: %v err=%v", body, other, err)
		}
	}
}

// TestDiscoverTimeoutBoundaryAcrossEntries pins the strict lost boundary on
// every discovery entry: evaluate_at exactly heartbeat_at + timeout keeps
// the record online, one second later evicts and deletes it.
func TestDiscoverTimeoutBoundaryAcrossEntries(t *testing.T) {
	// One second past the boundary: heartbeat 12:05 + 5m < 12:10:01.
	past := time.Date(2026, 10, 1, 12, 10, 1, 0, time.UTC).Format(time.RFC3339)
	pastEntries := []struct {
		method string
		target string
		body   any
	}{
		{http.MethodGet, "/api/v1/discover?service_name=svc&evaluate_at=" + past + "&heartbeat_timeout=5m", nil},
		{http.MethodGet, "/api/v1/services/svc/discover?evaluate_at=" + past + "&heartbeat_timeout=5m", nil},
		{http.MethodPost, "/api/v1/discover", map[string]any{
			"service_name": "svc", "evaluate_at": past, "heartbeat_timeout": "5m"}},
		{http.MethodPost, "/api/v1/services/svc/discover", map[string]any{
			"evaluate_at": past, "heartbeat_timeout": 300}},
	}
	for index, entry := range discoverEntries {
		t.Run(entry.name, func(t *testing.T) {
			router, st := testRouter(t)
			registerInstance(t, router, map[string]any{
				"service_name": "svc", "instance_id": "edge", "healthy": true,
				"weight": 1, "heartbeat_at": at(5),
			})

			// Exactly on the boundary: online, returned, kept.
			rec := doRequest(t, router, entry.method, entry.target, entry.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("boundary status = %d, body %s", rec.Code, rec.Body.String())
			}
			if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 1 || ids[0] != "edge" {
				t.Fatalf("boundary ids = %v, want [edge]", ids)
			}
			if _, found, _ := st.GetInstance("svc", "edge"); !found {
				t.Fatalf("boundary record deleted")
			}

			// Strictly past the boundary: lost, excluded, deleted.
			pastEntry := pastEntries[index]
			rec = doRequest(t, router, pastEntry.method, pastEntry.target, pastEntry.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("past-boundary status = %d, body %s", rec.Code, rec.Body.String())
			}
			if ids := instanceIDs(t, decodeBody(t, rec)); len(ids) != 0 {
				t.Fatalf("past-boundary ids = %v, want []", ids)
			}
			if _, found, _ := st.GetInstance("svc", "edge"); found {
				t.Fatalf("strictly lost record not deleted")
			}
		})
	}
}
