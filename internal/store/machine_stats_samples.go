package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// machine_stats_samples keeps every reading of every machine's health, one
// row per machine per minute, for the Machines page's history. Rows older
// than the retention are deleted by the sampler (internal/server).

// machineStatsTimeFormat is fixed-width so times compare as text in SQL.
const machineStatsTimeFormat = "2006-01-02T15:04:05.000000000Z"

// migrateMachineStatsSamples creates machine_stats_samples. Called from
// migrate().
func (s *Store) migrateMachineStatsSamples() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS machine_stats_samples (
			machine_id   TEXT NOT NULL,
			collected_at TEXT NOT NULL,
			stats_json   TEXT NOT NULL,
			PRIMARY KEY (machine_id, collected_at)
		) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS idx_machine_stats_samples_collected_at ON machine_stats_samples(collected_at);
	`)
	return err
}

// RecordMachineStatsSample stores one reading of a machine. A second reading
// with the same time replaces the first.
func (s *Store) RecordMachineStatsSample(machineID string, stats msg.MachineStats) error {
	if machineID == "" {
		return fmt.Errorf("record machine stats: no machine id")
	}
	if stats.CollectedAt.IsZero() {
		return fmt.Errorf("record machine stats for %s: no collection time", machineID)
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("encode machine stats: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO machine_stats_samples (machine_id, collected_at, stats_json) VALUES (?, ?, ?)
		ON CONFLICT(machine_id, collected_at) DO UPDATE SET stats_json = excluded.stats_json`,
		machineID, stats.CollectedAt.UTC().Format(machineStatsTimeFormat), string(encoded))
	return err
}

// LatestMachineStats returns each machine's newest reading, keyed by machine id.
func (s *Store) LatestMachineStats() (map[string]msg.MachineStats, error) {
	rows, err := s.db.Query(`
		SELECT samples.machine_id, samples.stats_json
		FROM machine_stats_samples AS samples
		JOIN (SELECT machine_id, MAX(collected_at) AS newest FROM machine_stats_samples GROUP BY machine_id) AS newest
		  ON newest.machine_id = samples.machine_id AND newest.newest = samples.collected_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	latest := map[string]msg.MachineStats{}
	for rows.Next() {
		var machineID, encoded string
		if err := rows.Scan(&machineID, &encoded); err != nil {
			return nil, err
		}
		var stats msg.MachineStats
		if err := json.Unmarshal([]byte(encoded), &stats); err != nil {
			return nil, fmt.Errorf("decode machine stats for %s: %w", machineID, err)
		}
		latest[machineID] = stats
	}
	return latest, rows.Err()
}

// MachineStatsSamplesBetween returns a machine's readings collected in
// [since, until), oldest first.
func (s *Store) MachineStatsSamplesBetween(machineID string, since, until time.Time) ([]msg.MachineStats, error) {
	rows, err := s.db.Query(`
		SELECT stats_json FROM machine_stats_samples
		WHERE machine_id = ? AND collected_at >= ? AND collected_at < ?
		ORDER BY collected_at`,
		machineID, since.UTC().Format(machineStatsTimeFormat), until.UTC().Format(machineStatsTimeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []msg.MachineStats
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var stats msg.MachineStats
		if err := json.Unmarshal([]byte(encoded), &stats); err != nil {
			return nil, fmt.Errorf("decode machine stats for %s: %w", machineID, err)
		}
		samples = append(samples, stats)
	}
	return samples, rows.Err()
}

// DeleteMachineStatsSamplesBefore deletes readings collected before cutoff
// and returns how many it deleted.
func (s *Store) DeleteMachineStatsSamplesBefore(cutoff time.Time) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM machine_stats_samples WHERE collected_at < ?`, cutoff.UTC().Format(machineStatsTimeFormat))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
