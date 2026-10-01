package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Sentinel errors callers can map to the published error codes.
var (
	// ErrInvalidArgument means a request violates the published parameter rules.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNotFound means no current instance record matches the lookup.
	ErrNotFound = errors.New("instance not found")
	// ErrEvaluateBeforeHeartbeat means evaluate_at is earlier than a stored heartbeat.
	ErrEvaluateBeforeHeartbeat = errors.New("evaluate_at is before heartbeat_at")
)

// Instance is the current record of one service instance.
type Instance struct {
	ServiceName string
	InstanceID  string
	Address     string
	Healthy     bool
	Weight      float64
	HeartbeatAt time.Time
}

const instanceColumns = `instance_id, address, healthy, weight, heartbeat_ns`

func validateInstance(in Instance) error {
	switch {
	case in.ServiceName == "":
		return fmt.Errorf("%w: service_name is required", ErrInvalidArgument)
	case in.InstanceID == "":
		return fmt.Errorf("%w: instance_id is required", ErrInvalidArgument)
	case in.Weight < 0:
		return fmt.Errorf("%w: weight must be a non-negative number", ErrInvalidArgument)
	case in.HeartbeatAt.IsZero():
		return fmt.Errorf("%w: heartbeat_at is required", ErrInvalidArgument)
	default:
		return nil
	}
}

// UpsertInstance creates the current record for a service instance or replaces the
// existing record for the same service name and instance identifier.
func (s *Store) UpsertInstance(ctx context.Context, in Instance) error {
	if err := validateInstance(in); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO service_instances
	(service_name, instance_id, address, healthy, weight, heartbeat_ns)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(service_name, instance_id) DO UPDATE SET
	address      = excluded.address,
	healthy      = excluded.healthy,
	weight       = excluded.weight,
	heartbeat_ns = excluded.heartbeat_ns
`,
		in.ServiceName, in.InstanceID, in.Address, in.Healthy, in.Weight, in.HeartbeatAt.UnixNano())
	if err != nil {
		return fmt.Errorf("upsert instance: %w", err)
	}
	return nil
}

// GetInstance returns the current record for one service instance.
func (s *Store) GetInstance(ctx context.Context, serviceName, instanceID string) (Instance, error) {
	if serviceName == "" || instanceID == "" {
		return Instance{}, fmt.Errorf("%w: service_name and instance_id are required", ErrInvalidArgument)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+instanceColumns+` FROM service_instances WHERE service_name = ? AND instance_id = ?`,
		serviceName, instanceID)
	return scanInstance(serviceName, row)
}

// DeleteInstance removes the current record for one service instance.
func (s *Store) DeleteInstance(ctx context.Context, serviceName, instanceID string) error {
	if serviceName == "" || instanceID == "" {
		return fmt.Errorf("%w: service_name and instance_id are required", ErrInvalidArgument)
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM service_instances WHERE service_name = ? AND instance_id = ?`,
		serviceName, instanceID)
	if err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete instance rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Discover returns the healthy, still-reachable instances of one service.
//
// Instances whose heartbeat is at or before evaluateAt minus timeout are treated as
// lost: they are evicted from the discoverable set inside one transaction. Instances
// that are merely unhealthy stay in storage but are omitted from the result. When
// evaluateAt is earlier than any stored heartbeat of the service, the call fails with
// ErrEvaluateBeforeHeartbeat and mutates nothing.
func (s *Store) Discover(ctx context.Context, serviceName string, evaluateAt time.Time, timeout time.Duration) ([]Instance, error) {
	if serviceName == "" {
		return nil, fmt.Errorf("%w: service_name is required", ErrInvalidArgument)
	}
	if evaluateAt.IsZero() {
		return nil, fmt.Errorf("%w: evaluate_at is required", ErrInvalidArgument)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("%w: timeout must be greater than zero", ErrInvalidArgument)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin discover: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+instanceColumns+` FROM service_instances WHERE service_name = ?`, serviceName)
	if err != nil {
		return nil, fmt.Errorf("select instances: %w", err)
	}
	records := make([]Instance, 0)
	for rows.Next() {
		inst, err := scanInstance(serviceName, rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, inst)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("scan instances: %w", err)
	}
	rows.Close()

	for _, inst := range records {
		if inst.HeartbeatAt.After(evaluateAt) {
			return nil, ErrEvaluateBeforeHeartbeat
		}
	}

	deadline := evaluateAt.Add(-timeout)
	lost := false
	candidates := make([]Instance, 0, len(records))
	for _, inst := range records {
		if !inst.HeartbeatAt.After(deadline) {
			lost = true
			continue
		}
		if inst.Healthy {
			candidates = append(candidates, inst)
		}
	}

	if lost {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM service_instances WHERE service_name = ? AND heartbeat_ns <= ?`,
			serviceName, deadline.UnixNano()); err != nil {
			return nil, fmt.Errorf("evict lost instances: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit discover: %w", err)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Weight != candidates[j].Weight {
			return candidates[i].Weight > candidates[j].Weight
		}
		if !candidates[i].HeartbeatAt.Equal(candidates[j].HeartbeatAt) {
			return candidates[i].HeartbeatAt.After(candidates[j].HeartbeatAt)
		}
		return candidates[i].InstanceID < candidates[j].InstanceID
	})
	return candidates, nil
}

func scanInstance(serviceName string, scanner interface {
	Scan(dest ...any) error
}) (Instance, error) {
	var inst Instance
	var heartbeatNS int64
	if err := scanner.Scan(
		&inst.InstanceID,
		&inst.Address,
		&inst.Healthy,
		&inst.Weight,
		&heartbeatNS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, ErrNotFound
		}
		return Instance{}, fmt.Errorf("scan instance: %w", err)
	}
	inst.ServiceName = serviceName
	inst.HeartbeatAt = time.Unix(0, heartbeatNS).UTC()
	return inst, nil
}
