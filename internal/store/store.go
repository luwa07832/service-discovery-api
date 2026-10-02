// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrInstanceNotFound reports that a targeted instance does not exist.
var ErrInstanceNotFound = errors.New("instance not found")

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Instance is the current record of one registered service instance.
type Instance struct {
	ServiceName string
	InstanceID  string
	Address     string
	Port        int64
	Healthy     bool
	Weight      float64
	HeartbeatAt time.Time
}

// InstanceInput carries the fields callers provide on registration or update.
type InstanceInput struct {
	ServiceName string
	InstanceID  string
	Address     string
	Port        int64
	Healthy     bool
	Weight      float64
	HeartbeatAt time.Time
}

// HeartbeatTarget carries one heartbeat renewal in a batch: the existing
// instance identified by InstanceID gets only its HeartbeatAt replaced.
type HeartbeatTarget struct {
	InstanceID  string
	HeartbeatAt time.Time
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate upgrades database files created by earlier versions in place.
func migrate(db *sql.DB) error {
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(1) FROM pragma_table_info('service_instances') WHERE name = 'port'`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := db.Exec(`ALTER TABLE service_instances ADD COLUMN port INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// UpsertInstance inserts an instance record or overwrites the current record
// of the same (service name, instance id) pair.
func (s *Store) UpsertInstance(input InstanceInput) (Instance, error) {
	heartbeat := input.HeartbeatAt.UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
INSERT INTO service_instances
	(service_name, instance_id, address, port, healthy, weight, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(service_name, instance_id) DO UPDATE SET
	address = excluded.address,
	port = excluded.port,
	healthy = excluded.healthy,
	weight = excluded.weight,
	heartbeat_at = excluded.heartbeat_at`,
		input.ServiceName, input.InstanceID, input.Address, input.Port, input.Healthy, input.Weight, heartbeat)
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		ServiceName: input.ServiceName,
		InstanceID:  input.InstanceID,
		Address:     input.Address,
		Port:        input.Port,
		Healthy:     input.Healthy,
		Weight:      input.Weight,
		HeartbeatAt: input.HeartbeatAt.UTC(),
	}, nil
}

// UpsertInstances inserts or overwrites several instances of one service in a
// single transaction. Every input shares the same service name and instance
// ids are expected to be unique within the batch; stored records are returned
// in request order. Any failure rolls the whole batch back without leaving
// partial updates.
func (s *Store) UpsertInstances(inputs []InstanceInput) ([]Instance, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	statement, err := tx.Prepare(`
INSERT INTO service_instances
	(service_name, instance_id, address, port, healthy, weight, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(service_name, instance_id) DO UPDATE SET
	address = excluded.address,
	port = excluded.port,
	healthy = excluded.healthy,
	weight = excluded.weight,
	heartbeat_at = excluded.heartbeat_at`)
	if err != nil {
		return nil, err
	}
	defer statement.Close()

	for _, input := range inputs {
		if _, err := statement.Exec(
			input.ServiceName, input.InstanceID, input.Address, input.Port,
			input.Healthy, input.Weight,
			input.HeartbeatAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
	}

	instances := make([]Instance, 0, len(inputs))
	for _, input := range inputs {
		instances = append(instances, Instance{
			ServiceName: input.ServiceName,
			InstanceID:  input.InstanceID,
			Address:     input.Address,
			Port:        input.Port,
			Healthy:     input.Healthy,
			Weight:      input.Weight,
			HeartbeatAt: input.HeartbeatAt.UTC(),
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return instances, nil
}

// GetInstance reads the current record of one instance.
func (s *Store) GetInstance(serviceName, instanceID string) (Instance, bool, error) {
	row := s.db.QueryRow(`
SELECT service_name, instance_id, address, port, healthy, weight, heartbeat_at
FROM service_instances
WHERE service_name = ? AND instance_id = ?`, serviceName, instanceID)
	instance, err := scanInstance(row)
	if err == sql.ErrNoRows {
		return Instance{}, false, nil
	}
	if err != nil {
		return Instance{}, false, err
	}
	return instance, true, nil
}

// ListInstances reads every current record belonging to one service. Other
// services are never touched or removed.
func (s *Store) ListInstances(serviceName string) ([]Instance, error) {
	rows, err := s.db.Query(`
SELECT service_name, instance_id, address, port, healthy, weight, heartbeat_at
FROM service_instances
WHERE service_name = ?`, serviceName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	instances := make([]Instance, 0)
	for rows.Next() {
		instance, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return instances, nil
}

// ListAllInstances reads every current record across all services. It never
// creates, updates or deletes rows; callers aggregate the read-only snapshot
// themselves. Results are ordered by service name and then instance id so
// identical storage content yields identical output.
func (s *Store) ListAllInstances() ([]Instance, error) {
	rows, err := s.db.Query(`
SELECT service_name, instance_id, address, port, healthy, weight, heartbeat_at
FROM service_instances
ORDER BY service_name ASC, instance_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	instances := make([]Instance, 0)
	for rows.Next() {
		instance, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return instances, nil
}

// DeleteInstance removes one instance record and reports whether it existed.
func (s *Store) DeleteInstance(serviceName, instanceID string) (bool, error) {
	result, err := s.db.Exec(`
DELETE FROM service_instances WHERE service_name = ? AND instance_id = ?`, serviceName, instanceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// TouchHeartbeats replaces only heartbeat_at of the named service's
// instances. The whole batch runs in one transaction: when any target does
// not exist the transaction rolls back and ErrInstanceNotFound is returned
// without partial updates. Updated records are returned in request order.
func (s *Store) TouchHeartbeats(serviceName string, targets []HeartbeatTarget) ([]Instance, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	statement, err := tx.Prepare(`
UPDATE service_instances SET heartbeat_at = ?
WHERE service_name = ? AND instance_id = ?`)
	if err != nil {
		return nil, err
	}
	defer statement.Close()

	for _, target := range targets {
		result, err := statement.Exec(
			target.HeartbeatAt.UTC().Format(time.RFC3339Nano), serviceName, target.InstanceID)
		if err != nil {
			return nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			return nil, ErrInstanceNotFound
		}
	}

	updated := make([]Instance, 0, len(targets))
	for _, target := range targets {
		row := tx.QueryRow(`
SELECT service_name, instance_id, address, port, healthy, weight, heartbeat_at
FROM service_instances
WHERE service_name = ? AND instance_id = ?`, serviceName, target.InstanceID)
		instance, err := scanInstance(row)
		if err == sql.ErrNoRows {
			return nil, ErrInstanceNotFound
		}
		if err != nil {
			return nil, err
		}
		updated = append(updated, instance)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(scanner rowScanner) (Instance, error) {
	var instance Instance
	var healthy int
	var heartbeat string
	if err := scanner.Scan(&instance.ServiceName, &instance.InstanceID, &instance.Address,
		&instance.Port, &healthy, &instance.Weight, &heartbeat); err != nil {
		return Instance{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, heartbeat)
	if err != nil {
		return Instance{}, err
	}
	instance.Healthy = healthy != 0
	instance.HeartbeatAt = parsed
	return instance, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS service_instances (
	service_name TEXT NOT NULL,
	instance_id  TEXT NOT NULL,
	address      TEXT NOT NULL DEFAULT '',
	port         INTEGER NOT NULL DEFAULT 0,
	healthy      INTEGER NOT NULL DEFAULT 0,
	weight       REAL NOT NULL DEFAULT 0,
	heartbeat_at TEXT NOT NULL,
	PRIMARY KEY(service_name, instance_id)
);
`
