package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustUpsert(t *testing.T, st *Store, inst Instance) {
	t.Helper()
	if err := st.UpsertInstance(context.Background(), inst); err != nil {
		t.Fatalf("upsert %s/%s: %v", inst.ServiceName, inst.InstanceID, err)
	}
}

func TestUpsertOverwritesCurrentRecord(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	heartbeat := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	mustUpsert(t, st, Instance{
		ServiceName: "orders", InstanceID: "a", Address: "10.0.0.1:80",
		Healthy: false, Weight: 5, HeartbeatAt: heartbeat,
	})
	mustUpsert(t, st, Instance{
		ServiceName: "orders", InstanceID: "a", Address: "10.0.0.2:81",
		Healthy: true, Weight: 9, HeartbeatAt: heartbeat.Add(time.Minute),
	})

	got, err := st.GetInstance(ctx, "orders", "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Address != "10.0.0.2:81" || !got.Healthy || got.Weight != 9 ||
		!got.HeartbeatAt.Equal(heartbeat.Add(time.Minute)) {
		t.Fatalf("record was not replaced: %+v", got)
	}
}

func TestUpsertRejectsInvalidParameters(t *testing.T) {
	st := testStore(t)
	heartbeat := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]Instance{
		"empty service":   {ServiceName: "", InstanceID: "a", Weight: 1, HeartbeatAt: heartbeat},
		"empty instance":  {ServiceName: "svc", InstanceID: "", Weight: 1, HeartbeatAt: heartbeat},
		"negative weight": {ServiceName: "svc", InstanceID: "a", Weight: -1, HeartbeatAt: heartbeat},
		"zero heartbeat":  {ServiceName: "svc", InstanceID: "a", Weight: 1, HeartbeatAt: time.Time{}},
	}
	for name, inst := range cases {
		t.Run(name, func(t *testing.T) {
			if err := st.UpsertInstance(context.Background(), inst); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestGetAndDeleteNotFound(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if _, err := st.GetInstance(ctx, "svc", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteInstance(ctx, "svc", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete err = %v, want ErrNotFound", err)
	}
}

func TestDiscoverFiltersSortsAndEvicts(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	evaluateAt := base.Add(10 * time.Second)
	timeout := 5 * time.Second
	// deadline = base + 5s; heartbeats at or before it are lost.

	mustUpsert(t, st, Instance{"svc", "z-lost", "addr1", true, 1, base})
	mustUpsert(t, st, Instance{"svc", "a-unhealthy-fresh", "addr2", false, 100, base.Add(6 * time.Second)})
	mustUpsert(t, st, Instance{"svc", "m-high", "addr3", true, 50, base.Add(7 * time.Second)})
	mustUpsert(t, st, Instance{"svc", "m-mid", "addr4", true, 10, base.Add(8 * time.Second)})
	mustUpsert(t, st, Instance{"svc", "m-tie-new", "addr5", true, 10, base.Add(9 * time.Second)})
	mustUpsert(t, st, Instance{"svc", "m-tie-old", "addr6", true, 10, base.Add(6 * time.Second)})
	mustUpsert(t, st, Instance{"svc", "boundary-lost", "addr7", true, 1, base.Add(5 * time.Second)})
	mustUpsert(t, st, Instance{"other", "o1", "addr8", true, 1, base})

	got, err := st.Discover(ctx, "svc", evaluateAt, timeout)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	wantIDs := []string{"m-high", "m-tie-new", "m-mid", "m-tie-old"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d instances %v, want %v", len(got), got, wantIDs)
	}
	for i, wantID := range wantIDs {
		if got[i].InstanceID != wantID {
			t.Fatalf("position %d = %q, want %q (order: %v)", i, got[i].InstanceID, wantID, got)
		}
	}

	// Lost instances are evicted; unhealthy-but-fresh and other services stay.
	for _, id := range []string{"z-lost", "boundary-lost"} {
		if _, err := st.GetInstance(ctx, "svc", id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lost instance %s still present: %v", id, err)
		}
	}
	if _, err := st.GetInstance(ctx, "svc", "a-unhealthy-fresh"); err != nil {
		t.Fatalf("unhealthy fresh instance should remain stored: %v", err)
	}
	if _, err := st.GetInstance(ctx, "other", "o1"); err != nil {
		t.Fatalf("other service records must be untouched: %v", err)
	}

	// Second discover: unhealthy instance is not returned and nothing else is lost.
	again, err := st.Discover(ctx, "svc", evaluateAt, timeout)
	if err != nil {
		t.Fatalf("second discover: %v", err)
	}
	if len(again) != len(wantIDs) {
		t.Fatalf("second discover = %d instances, want %d", len(again), len(wantIDs))
	}
}

func TestDiscoverEmptyResults(t *testing.T) {
	st := testStore(t)
	evaluateAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	got, err := st.Discover(context.Background(), "unknown", evaluateAt, time.Second)
	if err != nil {
		t.Fatalf("discover unknown service: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty list, got %v", got)
	}

	mustUpsert(t, st, Instance{"svc", "only-lost", "addr", true, 1, evaluateAt.Add(-2 * time.Second)})
	got, err = st.Discover(context.Background(), "svc", evaluateAt, time.Second)
	if err != nil {
		t.Fatalf("discover all-lost service: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty list, got %v", got)
	}
}

func TestDiscoverFutureHeartbeatFailsWithoutMutation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	evaluateAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mustUpsert(t, st, Instance{"svc", "future", "addr", true, 1, evaluateAt.Add(time.Second)})
	mustUpsert(t, st, Instance{"svc", "past", "addr2", true, 1, evaluateAt.Add(-30 * time.Second)})

	_, err := st.Discover(ctx, "svc", evaluateAt, 5*time.Second)
	if !errors.Is(err, ErrEvaluateBeforeHeartbeat) {
		t.Fatalf("err = %v, want ErrEvaluateBeforeHeartbeat", err)
	}
	for _, id := range []string{"future", "past"} {
		if _, err := st.GetInstance(ctx, "svc", id); err != nil {
			t.Fatalf("invalid discover must not mutate records, %s missing: %v", id, err)
		}
	}
}

func TestDiscoverRejectsInvalidParameters(t *testing.T) {
	st := testStore(t)
	evaluateAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		service string
		at      time.Time
		timeout time.Duration
	}{
		"empty service": {"", evaluateAt, time.Second},
		"zero evaluate": {"svc", time.Time{}, time.Second},
		"zero timeout":  {"svc", evaluateAt, 0},
		"minus timeout": {"svc", evaluateAt, -time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := st.Discover(context.Background(), tc.service, tc.at, tc.timeout); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}
