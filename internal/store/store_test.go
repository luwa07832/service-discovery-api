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

func TestUpsertInstancesInsertsOverwritesAndKeepsOrder(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc", InstanceID: "i-1", Address: "10.0.0.1:8080",
		Port: 8080, Healthy: true, Weight: 7, HeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	inputs := []InstanceInput{
		{ServiceName: "svc", InstanceID: "i-2", Address: "10.0.0.2", Weight: 5, HeartbeatAt: heartbeat.Add(time.Minute)},
		{ServiceName: "svc", InstanceID: "i-1", Address: "10.0.0.3", Port: 9090, Healthy: false, Weight: 1, HeartbeatAt: heartbeat.Add(2 * time.Minute)},
	}
	saved, err := st.UpsertInstances(inputs)
	if err != nil {
		t.Fatalf("upsert batch: %v", err)
	}
	if len(saved) != 2 || saved[0].InstanceID != "i-2" || saved[1].InstanceID != "i-1" {
		t.Fatalf("saved order = %+v", saved)
	}
	overwritten, found, _ := st.GetInstance("svc", "i-1")
	if !found || overwritten.Address != "10.0.0.3" || overwritten.Port != 9090 ||
		overwritten.Healthy || overwritten.Weight != 1 ||
		!overwritten.HeartbeatAt.Equal(heartbeat.Add(2*time.Minute)) {
		t.Fatalf("i-1 not overwritten: %+v", overwritten)
	}
}

func TestTouchHeartbeatsUpdatesOnlyHeartbeatInRequestOrder(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seed := InstanceInput{
		ServiceName: "svc", Address: "10.0.0.1:8080", Port: 8080,
		Healthy: true, Weight: 7, HeartbeatAt: heartbeat,
	}
	first := seed
	first.InstanceID = "i-1"
	second := seed
	second.InstanceID = "i-2"
	if _, err := st.UpsertInstance(first); err != nil {
		t.Fatalf("upsert i-1: %v", err)
	}
	if _, err := st.UpsertInstance(second); err != nil {
		t.Fatalf("upsert i-2: %v", err)
	}
	other := seed
	other.ServiceName = "svc-other"
	other.InstanceID = "i-2"
	if _, err := st.UpsertInstance(other); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	renewed := heartbeat.Add(5 * time.Minute)
	updated, err := st.TouchHeartbeats("svc", []HeartbeatTarget{
		{InstanceID: "i-2", HeartbeatAt: renewed},
		{InstanceID: "i-1", HeartbeatAt: renewed.Add(time.Second)},
	})
	if err != nil {
		t.Fatalf("touch: %v", err)
	}
	if len(updated) != 2 || updated[0].InstanceID != "i-2" || updated[1].InstanceID != "i-1" {
		t.Fatalf("updated order = %+v", updated)
	}
	got, _, _ := st.GetInstance("svc", "i-1")
	if got.Address != "10.0.0.1:8080" || got.Port != 8080 || !got.Healthy ||
		got.Weight != 7 || !got.HeartbeatAt.Equal(renewed.Add(time.Second)) {
		t.Fatalf("non-heartbeat fields changed: %+v", got)
	}
	if peer, _, _ := st.GetInstance("svc-other", "i-2"); !peer.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("other service heartbeat changed: %+v", peer)
	}
}

func TestTouchHeartbeatsMissingInstanceRollsBack(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc", InstanceID: "i-1", Weight: 1, HeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	_, err := st.TouchHeartbeats("svc", []HeartbeatTarget{
		{InstanceID: "i-1", HeartbeatAt: heartbeat.Add(time.Minute)},
		{InstanceID: "missing", HeartbeatAt: heartbeat.Add(2 * time.Minute)},
	})
	if err != ErrInstanceNotFound {
		t.Fatalf("err = %v, want ErrInstanceNotFound", err)
	}
	if got, _, _ := st.GetInstance("svc", "i-1"); !got.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("batch was not rolled back: %+v", got)
	}
}

func TestListAllInstancesReturnsAllServicesOrdered(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	inputs := []InstanceInput{
		{ServiceName: "zeta", InstanceID: "i-2", Weight: 1, HeartbeatAt: heartbeat},
		{ServiceName: "alpha", InstanceID: "i-2", Weight: 1, HeartbeatAt: heartbeat},
		{ServiceName: "alpha", InstanceID: "i-1", Weight: 1, HeartbeatAt: heartbeat},
	}
	for _, input := range inputs {
		if _, err := st.UpsertInstance(input); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	all, err := st.ListAllInstances()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	got := [3][2]string{
		{all[0].ServiceName, all[0].InstanceID},
		{all[1].ServiceName, all[1].InstanceID},
		{all[2].ServiceName, all[2].InstanceID},
	}
	want := [3][2]string{{"alpha", "i-1"}, {"alpha", "i-2"}, {"zeta", "i-2"}}
	if got != want {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// The read does not modify anything.
	if count := len(all); count != 3 {
		t.Fatalf("unexpected record count %d", count)
	}
}

func TestDeleteInstancesRemovesOnlyTargetedRecords(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	inputs := []InstanceInput{
		{ServiceName: "svc-a", InstanceID: "i-1", Weight: 1, HeartbeatAt: heartbeat},
		{ServiceName: "svc-a", InstanceID: "i-2", Weight: 1, HeartbeatAt: heartbeat},
		{ServiceName: "svc-b", InstanceID: "i-1", Weight: 1, HeartbeatAt: heartbeat},
	}
	for _, input := range inputs {
		if _, err := st.UpsertInstance(input); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	if err := st.DeleteInstances([]InstanceKey{
		{ServiceName: "svc-a", InstanceID: "i-1"},
		{ServiceName: "svc-b", InstanceID: "i-1"},
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, key := range [][2]string{{"svc-a", "i-1"}, {"svc-b", "i-1"}} {
		if _, found, err := st.GetInstance(key[0], key[1]); err != nil || found {
			t.Fatalf("%s/%s should be deleted: found=%v err=%v", key[0], key[1], found, err)
		}
	}
	remaining, err := st.ListAllInstances()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ServiceName != "svc-a" || remaining[0].InstanceID != "i-2" {
		t.Fatalf("remaining = %+v, want only svc-a/i-2", remaining)
	}
}

func TestUpdateWeightsChangesOnlyWeightInRequestOrder(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seed := InstanceInput{
		ServiceName: "svc", Address: "10.0.0.1:8080", Port: 8080,
		Healthy: true, Weight: 7, HeartbeatAt: heartbeat,
	}
	first := seed
	first.InstanceID = "i-1"
	second := seed
	second.InstanceID = "i-2"
	second.Healthy = false
	if _, err := st.UpsertInstance(first); err != nil {
		t.Fatalf("upsert i-1: %v", err)
	}
	if _, err := st.UpsertInstance(second); err != nil {
		t.Fatalf("upsert i-2: %v", err)
	}
	other := seed
	other.ServiceName = "svc-other"
	other.InstanceID = "i-2"
	if _, err := st.UpsertInstance(other); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	updated, err := st.UpdateWeights("svc", []WeightTarget{
		{InstanceID: "i-2", Weight: 42.5},
		{InstanceID: "i-1", Weight: 99},
	})
	if err != nil {
		t.Fatalf("update weights: %v", err)
	}
	if len(updated) != 2 || updated[0].InstanceID != "i-2" || updated[1].InstanceID != "i-1" {
		t.Fatalf("updated order = %+v", updated)
	}
	got, _, _ := st.GetInstance("svc", "i-1")
	if got.Address != "10.0.0.1:8080" || got.Port != 8080 || !got.Healthy ||
		got.Weight != 99 || !got.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("non-weight fields changed: %+v", got)
	}
	got2, _, _ := st.GetInstance("svc", "i-2")
	if got2.Weight != 42.5 || got2.Healthy != false {
		t.Fatalf("i-2 mismatch: %+v", got2)
	}
	if peer, _, _ := st.GetInstance("svc-other", "i-2"); peer.Weight != 7 {
		t.Fatalf("other service weight changed: %+v", peer)
	}
}

func TestUpdateWeightsMissingInstanceRollsBack(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc", InstanceID: "i-1", Weight: 1, HeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	_, err := st.UpdateWeights("svc", []WeightTarget{
		{InstanceID: "i-1", Weight: 10},
		{InstanceID: "missing", Weight: 20},
	})
	if err != ErrInstanceNotFound {
		t.Fatalf("err = %v, want ErrInstanceNotFound", err)
	}
	if got, _, _ := st.GetInstance("svc", "i-1"); got.Weight != 1 {
		t.Fatalf("batch was not rolled back: %+v", got)
	}
}

func TestUpdateHealthsChangesOnlyHealthInRequestOrder(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seed := InstanceInput{
		ServiceName: "svc", Address: "10.0.0.1:8080", Port: 8080,
		Healthy: true, Weight: 7, HeartbeatAt: heartbeat,
	}
	first := seed
	first.InstanceID = "i-1"
	second := seed
	second.InstanceID = "i-2"
	if _, err := st.UpsertInstance(first); err != nil {
		t.Fatalf("upsert i-1: %v", err)
	}
	if _, err := st.UpsertInstance(second); err != nil {
		t.Fatalf("upsert i-2: %v", err)
	}
	other := seed
	other.ServiceName = "svc-other"
	other.InstanceID = "i-2"
	if _, err := st.UpsertInstance(other); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	updated, err := st.UpdateHealths("svc", []HealthTarget{
		{InstanceID: "i-2", Healthy: false},
		{InstanceID: "i-1", Healthy: false},
	})
	if err != nil {
		t.Fatalf("update healths: %v", err)
	}
	if len(updated) != 2 || updated[0].InstanceID != "i-2" || updated[1].InstanceID != "i-1" {
		t.Fatalf("updated order = %+v", updated)
	}
	if updated[0].Healthy || updated[1].Healthy {
		t.Fatalf("updated records still healthy: %+v", updated)
	}
	got, _, _ := st.GetInstance("svc", "i-1")
	if got.Address != "10.0.0.1:8080" || got.Port != 8080 || got.Healthy ||
		got.Weight != 7 || !got.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("non-health fields changed: %+v", got)
	}
	if peer, _, _ := st.GetInstance("svc-other", "i-2"); !peer.Healthy {
		t.Fatalf("other service health changed: %+v", peer)
	}
}

func TestUpdateHealthsMissingInstanceRollsBack(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.UpsertInstance(InstanceInput{
		ServiceName: "svc", InstanceID: "i-1", Healthy: true, Weight: 1, HeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	_, err := st.UpdateHealths("svc", []HealthTarget{
		{InstanceID: "i-1", Healthy: false},
		{InstanceID: "missing", Healthy: false},
	})
	if err != ErrInstanceNotFound {
		t.Fatalf("err = %v, want ErrInstanceNotFound", err)
	}
	if got, _, _ := st.GetInstance("svc", "i-1"); !got.Healthy {
		t.Fatalf("batch was not rolled back: %+v", got)
	}
}
