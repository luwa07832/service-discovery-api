package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestUpsertGetOverwriteAndList(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	saved, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc-a", InstanceID: "i-1", Address: "10.0.0.1:8080",
		Port: 8080, Healthy: true, Weight: 5, HeartbeatAt: heartbeat,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !saved.Healthy || saved.Weight != 5 || saved.Port != 8080 || !saved.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("unexpected saved instance: %+v", saved)
	}

	got, found, err := st.GetInstance("svc-a", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.Address != "10.0.0.1:8080" || got.Port != 8080 {
		t.Fatalf("address = %q port = %d", got.Address, got.Port)
	}

	// Repeated registration overwrites the current record.
	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc-a", InstanceID: "i-1", Address: "10.0.0.2:8080",
		Healthy: false, Weight: 9, HeartbeatAt: heartbeat.Add(time.Minute),
	}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _, _ = st.GetInstance("svc-a", "i-1")
	if got.Address != "10.0.0.2:8080" || got.Healthy || got.Weight != 9 {
		t.Fatalf("overwrite did not replace fields: %+v", got)
	}

	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc-a", InstanceID: "i-2", HeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("upsert i-2: %v", err)
	}
	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc-b", InstanceID: "i-9", HeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("upsert svc-b: %v", err)
	}

	listA, err := st.ListInstances("svc-a")
	if err != nil || len(listA) != 2 {
		t.Fatalf("list svc-a: len=%d err=%v", len(listA), err)
	}
	listB, _ := st.ListInstances("svc-b")
	if len(listB) != 1 {
		t.Fatalf("list svc-b len = %d", len(listB))
	}
	missing, _ := st.ListInstances("svc-none")
	if len(missing) != 0 {
		t.Fatalf("unknown service returned %d rows", len(missing))
	}

	deleted, err := st.DeleteInstance("svc-a", "i-1")
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if _, found, err := st.GetInstance("svc-a", "i-1"); err != nil || found {
		t.Fatalf("instance still present after delete: found=%v err=%v", found, err)
	}
	deleted, err = st.DeleteInstance("svc-a", "i-1")
	if err != nil || deleted {
		t.Fatalf("second delete: deleted=%v err=%v", deleted, err)
	}
	// Other services survive a delete.
	if _, found, _ := st.GetInstance("svc-b", "i-9"); !found {
		t.Fatalf("other service instance was removed")
	}
}

func TestOpenMigratesDatabaseWithoutPortColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
CREATE TABLE service_instances (
	service_name TEXT NOT NULL,
	instance_id  TEXT NOT NULL,
	address      TEXT NOT NULL DEFAULT '',
	healthy      INTEGER NOT NULL DEFAULT 0,
	weight       REAL NOT NULL DEFAULT 0,
	heartbeat_at TEXT NOT NULL,
	PRIMARY KEY(service_name, instance_id)
);
INSERT INTO service_instances
	(service_name, instance_id, address, healthy, weight, heartbeat_at)
VALUES ('svc', 'i', '10.0.0.1', 1, 3, '2026-10-01T12:00:00Z');`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer st.Close()

	got, found, err := st.GetInstance("svc", "i")
	if err != nil || !found {
		t.Fatalf("get after migrate: found=%v err=%v", found, err)
	}
	if got.Port != 0 || got.Address != "10.0.0.1" || !got.Healthy || got.Weight != 3 {
		t.Fatalf("migrated row mismatch: %+v", got)
	}
	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc", InstanceID: "i", Port: 9000,
		Healthy: true, Weight: 4, HeartbeatAt: time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("upsert after migrate: %v", err)
	}
	if got, _, _ := st.GetInstance("svc", "i"); got.Port != 9000 {
		t.Fatalf("port after upsert = %d", got.Port)
	}
}
