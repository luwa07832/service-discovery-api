package store

import (
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
		Healthy: true, Weight: 5, HeartbeatAt: heartbeat,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !saved.Healthy || saved.Weight != 5 || !saved.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("unexpected saved instance: %+v", saved)
	}

	got, found, err := st.GetInstance("svc-a", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.Address != "10.0.0.1:8080" {
		t.Fatalf("address = %q", got.Address)
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
