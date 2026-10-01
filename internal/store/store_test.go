package store

import (
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open("ignored-path")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func instance(service, id string, heartbeat time.Time) Instance {
	return Instance{
		ServiceName: service,
		InstanceID:  id,
		Host:        "10.0.0.1",
		Port:        8080,
		Address:     "10.0.0.1:8080",
		Healthy:     true,
		Weight:      5,
		HeartbeatAt: heartbeat,
	}
}

func TestUpsertReportsExistenceAndOverwrites(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	saved, existed, err := st.UpsertInstance(instance("svc-a", "i-1", heartbeat))
	if err != nil || existed {
		t.Fatalf("first upsert: existed=%v err=%v", existed, err)
	}
	if saved.ServiceName != "svc-a" || saved.InstanceID != "i-1" || saved.Host != "10.0.0.1" || saved.Port != 8080 {
		t.Fatalf("unexpected saved instance: %+v", saved)
	}

	got, found, err := st.GetInstance("svc-a", "i-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.Address != "10.0.0.1:8080" {
		t.Fatalf("address = %q", got.Address)
	}

	replacement := instance("svc-a", "i-1", heartbeat.Add(time.Minute))
	replacement.Host = "10.0.0.2"
	replacement.Port = 9090
	replacement.Address = "10.0.0.2:9090"
	replacement.Healthy = false
	replacement.Weight = 9
	_, existed, err = st.UpsertInstance(replacement)
	if err != nil || !existed {
		t.Fatalf("overwrite: existed=%v err=%v", existed, err)
	}
	got, _, _ = st.GetInstance("svc-a", "i-1")
	if got.Host != "10.0.0.2" || got.Port != 9090 || got.Address != "10.0.0.2:9090" || got.Healthy || got.Weight != 9 {
		t.Fatalf("overwrite did not replace fields: %+v", got)
	}

	// Different instance ids under one service are kept separately.
	if _, existed, _ := st.UpsertInstance(instance("svc-a", "i-2", heartbeat)); existed {
		t.Fatalf("i-2 should be a new record")
	}
	if _, existed, _ := st.UpsertInstance(instance("svc-b", "i-9", heartbeat)); existed {
		t.Fatalf("svc-b/i-9 should be a new record")
	}
	if listA, _ := st.ListInstances("svc-a"); len(listA) != 2 {
		t.Fatalf("list svc-a len = %d, want 2", len(listA))
	}
	if listB, _ := st.ListInstances("svc-b"); len(listB) != 1 {
		t.Fatalf("list svc-b len = %d, want 1", len(listB))
	}
	if missing, _ := st.ListInstances("svc-none"); len(missing) != 0 {
		t.Fatalf("unknown service returned %d rows", len(missing))
	}
}

func TestUpdateInstanceRefreshesExistingOnly(t *testing.T) {
	st := openTestStore(t)
	first := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := st.UpsertInstance(instance("svc", "i-1", first)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	healthy, weight := false, 2.5
	updated, ok := st.UpdateInstance("svc", "i-1", InstanceUpdate{
		Healthy: &healthy, Weight: &weight, HeartbeatAt: first.Add(time.Minute),
	})
	if !ok {
		t.Fatalf("update should succeed for a known instance")
	}
	if updated.Healthy || updated.Weight != 2.5 || !updated.HeartbeatAt.Equal(first.Add(time.Minute)) {
		t.Fatalf("updated record mismatch: %+v", updated)
	}
	got, _, _ := st.GetInstance("svc", "i-1")
	if got.Healthy || got.Weight != 2.5 {
		t.Fatalf("stored record was not refreshed: %+v", got)
	}

	// Repeating the exact same update at the same heartbeat time is stable.
	again, ok := st.UpdateInstance("svc", "i-1", InstanceUpdate{
		Healthy: &healthy, Weight: &weight, HeartbeatAt: first.Add(time.Minute),
	})
	if !ok || !again.HeartbeatAt.Equal(updated.HeartbeatAt) {
		t.Fatalf("identical repeat update is not stable: %+v", again)
	}
	if list, _ := st.ListInstances("svc"); len(list) != 1 {
		t.Fatalf("repeated update created a record: %d rows", len(list))
	}

	// Unknown instance ids and unknown services report false without creating.
	unknownHealthy, unknownWeight := true, 1.0
	if _, ok := st.UpdateInstance("svc", "missing", InstanceUpdate{
		Healthy: &unknownHealthy, Weight: &unknownWeight, HeartbeatAt: first,
	}); ok {
		t.Fatalf("update for unknown instance id should fail")
	}
	if _, ok := st.UpdateInstance("other", "i-1", InstanceUpdate{
		Healthy: &unknownHealthy, Weight: &unknownWeight, HeartbeatAt: first,
	}); ok {
		t.Fatalf("update for unknown service should fail")
	}
	if _, found, _ := st.GetInstance("svc", "missing"); found {
		t.Fatalf("unknown instance was silently created")
	}
	if list, _ := st.ListInstances("other"); len(list) != 0 {
		t.Fatalf("unknown service gained records: %d", len(list))
	}
}

func TestDeleteInstance(t *testing.T) {
	st := openTestStore(t)
	heartbeat := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := st.UpsertInstance(instance("svc-a", "i-1", heartbeat)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, _, err := st.UpsertInstance(instance("svc-b", "i-9", heartbeat)); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	deleted, err := st.DeleteInstance("svc-a", "i-1")
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if _, found, _ := st.GetInstance("svc-a", "i-1"); found {
		t.Fatalf("instance still present after delete")
	}
	if deleted, _ := st.DeleteInstance("svc-a", "i-1"); deleted {
		t.Fatalf("second delete should report false")
	}
	if _, found, _ := st.GetInstance("svc-b", "i-9"); !found {
		t.Fatalf("other service instance was removed")
	}
}

func TestRemoveLostUsesStrictBoundaryAndIsServiceScoped(t *testing.T) {
	st := openTestStore(t)
	evaluate := time.Date(2026, 10, 1, 10, 10, 0, 0, time.UTC)
	timeout := 10 * time.Minute
	deadline := evaluate.Add(-timeout)

	mk := func(service, id string, healthy bool, heartbeat time.Time) {
		record := instance(service, id, heartbeat)
		record.Healthy = healthy
		if _, _, err := st.UpsertInstance(record); err != nil {
			t.Fatalf("upsert %s/%s: %v", service, id, err)
		}
	}
	mk("svc", "z-lost-healthy", true, deadline.Add(-time.Second))
	mk("svc", "a-lost-unhealthy", false, deadline.Add(-time.Minute))
	mk("svc", "boundary-healthy", true, deadline)
	mk("svc", "fresh-healthy", true, deadline.Add(time.Second))
	mk("other", "lost-other-service", true, deadline.Add(-time.Hour))

	removed := st.RemoveLost("svc", evaluate, timeout)
	want := []string{"a-lost-unhealthy", "z-lost-healthy"}
	if len(removed) != len(want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	for i := range want {
		if removed[i] != want[i] {
			t.Fatalf("removed = %v, want %v (sorted ids)", removed, want)
		}
	}
	if _, found, _ := st.GetInstance("svc", "boundary-healthy"); !found {
		t.Fatalf("heartbeat equal to the deadline must survive")
	}
	if _, found, _ := st.GetInstance("svc", "fresh-healthy"); !found {
		t.Fatalf("fresh instance must survive")
	}
	if _, found, _ := st.GetInstance("other", "lost-other-service"); !found {
		t.Fatalf("cleanup touched another service")
	}

	if removed := st.RemoveLost("none", evaluate, timeout); len(removed) != 0 {
		t.Fatalf("unknown service cleanup = %v, want empty", removed)
	}
}
