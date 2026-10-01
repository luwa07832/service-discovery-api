// Package store owns the in-process instance registry used by the service.
// Records live only in memory: the service never writes a database file or
// any other durable state.
package store

import (
	"sort"
	"sync"
	"time"
)

// Instance is the current record of one registered service instance.
type Instance struct {
	ServiceName string
	InstanceID  string
	Host        string
	Port        int
	Address     string
	Healthy     bool
	Weight      float64
	HeartbeatAt time.Time
}

// Store is the in-process registry of service instances.
type Store struct {
	mu        sync.RWMutex
	instances map[string]map[string]Instance
}

// Open creates an empty in-process registry. The path is accepted for
// compatibility with the previous file-backed entry point but is never used:
// this store keeps no durable state.
func Open(_ string) (*Store, error) {
	return &Store{instances: make(map[string]map[string]Instance)}, nil
}

// Ping reports whether the in-process registry is usable.
func (s *Store) Ping() error { return nil }

// Close releases the in-process registry.
func (s *Store) Close() error { return nil }

// UpsertInstance records a registration, overwriting the current record of
// the same (service name, instance id) pair. It reports whether an instance
// with that pair was already registered.
func (s *Store) UpsertInstance(instance Instance) (Instance, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	group := s.instances[instance.ServiceName]
	if group == nil {
		group = make(map[string]Instance)
		s.instances[instance.ServiceName] = group
	}
	_, existed := group[instance.InstanceID]
	group[instance.InstanceID] = instance
	return instance, existed, nil
}

// InstanceUpdate carries the fields of a heartbeat update. Nil pointers mean
// the current value of that field must be preserved.
type InstanceUpdate struct {
	Healthy     *bool
	Weight      *float64
	HeartbeatAt time.Time
}

// UpdateInstance applies a heartbeat update to an existing instance. Lookup
// and write happen under one lock so a concurrent delete cannot be observed
// as a storage failure. It returns false without creating a record when the
// instance is unknown.
func (s *Store) UpdateInstance(serviceName, instanceID string, update InstanceUpdate) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	group := s.instances[serviceName]
	if group == nil {
		return Instance{}, false
	}
	current, ok := group[instanceID]
	if !ok {
		return Instance{}, false
	}
	if update.Healthy != nil {
		current.Healthy = *update.Healthy
	}
	if update.Weight != nil {
		current.Weight = *update.Weight
	}
	current.HeartbeatAt = update.HeartbeatAt
	group[instanceID] = current
	return current, true
}

// GetInstance reads the current record of one instance.
func (s *Store) GetInstance(serviceName, instanceID string) (Instance, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	group := s.instances[serviceName]
	if group == nil {
		return Instance{}, false, nil
	}
	instance, ok := group[instanceID]
	return instance, ok, nil
}

// ListInstances reads every current record belonging to one service. Other
// services are never touched or removed.
func (s *Store) ListInstances(serviceName string) ([]Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	group := s.instances[serviceName]
	instances := make([]Instance, 0, len(group))
	for _, instance := range group {
		instances = append(instances, instance)
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].InstanceID < instances[j].InstanceID
	})
	return instances, nil
}

// DeleteInstance removes one instance record and reports whether it existed.
func (s *Store) DeleteInstance(serviceName, instanceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	group := s.instances[serviceName]
	if group == nil {
		return false, nil
	}
	if _, ok := group[instanceID]; !ok {
		return false, nil
	}
	delete(group, instanceID)
	if len(group) == 0 {
		delete(s.instances, serviceName)
	}
	return true, nil
}

// RemoveLost deletes every instance of the service whose heartbeat is strictly
// earlier than evaluateAt minus timeout, returning the deleted instance ids.
// Health state does not protect an instance from this reaper.
func (s *Store) RemoveLost(serviceName string, evaluateAt time.Time, timeout time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	group := s.instances[serviceName]
	deadline := evaluateAt.Add(-timeout)
	removed := make([]string, 0)
	for instanceID, instance := range group {
		if instance.HeartbeatAt.Before(deadline) {
			delete(group, instanceID)
			removed = append(removed, instanceID)
		}
	}
	if len(group) == 0 {
		delete(s.instances, serviceName)
	}
	sort.Strings(removed)
	return removed
}
