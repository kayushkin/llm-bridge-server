package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A runner spawn is one start of a session's harness process on a runner
// machine whose output is numbered. The row says which spawn the session is on
// and how far the server has stored its output, so that after a restart the
// server can tell a runner still holding the process where to send again from.
//
// The row is written when the spawn is asked for and ended when the process
// is known to be gone or no longer wanted. A session has at most one row: a
// new spawn replaces it.

// RunnerSpawn is a session's current spawn on a runner machine.
type RunnerSpawn struct {
	SessionID string
	SpawnID   string
	MachineID string
	// LastSequence is the number of the newest output entry the server has
	// stored for this spawn; 0 before the first.
	LastSequence uint64
	// Ended says the spawn's process is gone or the server gave it up.
	Ended bool
}

// RecordRunnerSpawn makes spawnID the session's current spawn, with nothing
// stored yet.
func (s *Store) RecordRunnerSpawn(sessionID, spawnID, machineID string) error {
	if sessionID == "" || spawnID == "" || machineID == "" {
		return errors.New("a runner spawn needs a session id, a spawn id and a machine id")
	}
	_, err := s.db.Exec(`
		INSERT INTO runner_spawns (session_id, spawn_id, machine_id, last_sequence, ended, started_at) VALUES ($1, $2, $3, 0, false, $4)
		ON CONFLICT(session_id) DO UPDATE SET spawn_id = excluded.spawn_id, machine_id = excluded.machine_id,
			last_sequence = 0, ended = false, started_at = excluded.started_at`,
		sessionID, spawnID, machineID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record the runner spawn of session %s: %w", sessionID, err)
	}
	return nil
}

// RunnerSpawnOfSession is the session's current spawn, or nil when it has none.
func (s *Store) RunnerSpawnOfSession(sessionID string) (*RunnerSpawn, error) {
	spawn := RunnerSpawn{SessionID: sessionID}
	err := s.db.QueryRow(`SELECT spawn_id, machine_id, last_sequence, ended FROM runner_spawns WHERE session_id = $1`, sessionID).
		Scan(&spawn.SpawnID, &spawn.MachineID, &spawn.LastSequence, &spawn.Ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the runner spawn of session %s: %w", sessionID, err)
	}
	return &spawn, nil
}

// OpenRunnerSpawnsOnMachine lists the spawns on one machine that have not ended.
func (s *Store) OpenRunnerSpawnsOnMachine(machineID string) ([]RunnerSpawn, error) {
	rows, err := s.db.Query(`SELECT session_id, spawn_id, last_sequence FROM runner_spawns WHERE machine_id = $1 AND NOT ended`, machineID)
	if err != nil {
		return nil, fmt.Errorf("list the open runner spawns on machine %s: %w", machineID, err)
	}
	defer rows.Close()
	var spawns []RunnerSpawn
	for rows.Next() {
		spawn := RunnerSpawn{MachineID: machineID}
		if err := rows.Scan(&spawn.SessionID, &spawn.SpawnID, &spawn.LastSequence); err != nil {
			return nil, err
		}
		spawns = append(spawns, spawn)
	}
	return spawns, rows.Err()
}

// AdvanceRunnerSpawn records that the server has dealt with the spawn's
// output through sequence. It never moves the mark back, and does nothing for
// a spawn that is no longer the session's.
func (s *Store) AdvanceRunnerSpawn(sessionID, spawnID string, sequence uint64) error {
	_, err := s.db.Exec(`UPDATE runner_spawns SET last_sequence = $1 WHERE session_id = $2 AND spawn_id = $3 AND last_sequence < $4`,
		sequence, sessionID, spawnID, sequence)
	if err != nil {
		return fmt.Errorf("advance the runner spawn of session %s to %d: %w", sessionID, sequence, err)
	}
	return nil
}

// EndRunnerSpawn marks the spawn ended, if it is still the session's.
func (s *Store) EndRunnerSpawn(sessionID, spawnID string) error {
	if _, err := s.db.Exec(`UPDATE runner_spawns SET ended = true WHERE session_id = $1 AND spawn_id = $2`, sessionID, spawnID); err != nil {
		return fmt.Errorf("end the runner spawn of session %s: %w", sessionID, err)
	}
	return nil
}
