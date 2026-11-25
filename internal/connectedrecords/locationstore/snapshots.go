package locationstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// Snapshot imports. An on-device Timeline export holds every visit and
// activity of the span it covers, so it is a full snapshot of that span. Two
// exports of the same span can cut the same day into different segments,
// and an import finds a record again only by its exact start and end, so
// without this the second export would add near-copies beside the first's.
//
// The rule: of the snapshot imports whose span holds a record's start, the
// newest decides. If it holds the record, the record stands (and comes back,
// if an older reconcile set it aside); if not, the record is soft-deleted
// with superseded_by_import_id naming that import, unless someone edited it.
// A record whose start no snapshot covers is left alone, so a narrower export
// never touches what lies outside its own span.
//
// Newest is decided by the files' own content: the later newest raw position,
// then the later oldest raw position (the phone keeps raw positions in a
// rolling buffer, so of two exports with the same newest position the later
// one has dropped more of the oldest), then the later import.

type snapshotImport struct {
	id       string
	seq      int64
	snapshot ImportSnapshot
}

func newerSnapshot(a, b snapshotImport) bool {
	if a.snapshot.RawSignalsNewestAt != b.snapshot.RawSignalsNewestAt {
		return a.snapshot.RawSignalsNewestAt > b.snapshot.RawSignalsNewestAt
	}
	if a.snapshot.RawSignalsOldestAt != b.snapshot.RawSignalsOldestAt {
		return a.snapshot.RawSignalsOldestAt > b.snapshot.RawSignalsOldestAt
	}
	return a.seq > b.seq
}

// SnapshotSupersededReason and SnapshotRestoredReason begin the revision
// reason of a record set aside or brought back, followed by the import.
const (
	SnapshotSupersededReason = "superseded by "
	SnapshotRestoredReason   = "restored by "
)

func reconcileSnapshots(tx *sql.Tx, at int64) (*SnapshotChanges, error) {
	changes := &SnapshotChanges{KeptEditedIDs: []string{}}
	rows, err := tx.Query(`SELECT id, seq, snapshot FROM imports WHERE snapshot IS NOT NULL AND finished_at != 0 AND kind = ?`, ImportKindGoogleTimeline)
	if err != nil {
		return nil, err
	}
	var snapshots []snapshotImport
	for rows.Next() {
		var item snapshotImport
		var encoded string
		if err := rows.Scan(&item.id, &item.seq, &encoded); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &item.snapshot); err != nil {
			rows.Close()
			return nil, fmt.Errorf("import %s has a snapshot that is not JSON: %w", item.id, err)
		}
		snapshots = append(snapshots, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return changes, nil
	}
	sort.Slice(snapshots, func(a, b int) bool { return newerSnapshot(snapshots[a], snapshots[b]) })

	held := map[string]map[string]bool{} // import id → record ids it holds
	for _, item := range snapshots {
		held[item.id] = map[string]bool{}
		heldRows, err := tx.Query(`SELECT entity_id FROM import_records WHERE import_id = ?`, item.id)
		if err != nil {
			return nil, err
		}
		for heldRows.Next() {
			var id string
			if err := heldRows.Scan(&id); err != nil {
				heldRows.Close()
				return nil, err
			}
			held[item.id][id] = true
		}
		heldRows.Close()
		if err := heldRows.Err(); err != nil {
			return nil, err
		}
	}

	for _, entityType := range []string{EntityVisit, EntityTrip} {
		table := entityTables[entityType]
		type candidate struct {
			id           string
			startedAt    int64
			deletedAt    int64
			supersededBy sql.NullString
			editedByHand bool
		}
		var candidates []candidate
		query := `SELECT r.id, r.started_at, r.deleted_at, r.superseded_by_import_id,
			EXISTS (SELECT 1 FROM revisions v WHERE v.entity_id = r.id AND v.reason IN ('update', 'attach', 'restore'))`
		if entityType == EntityTrip {
			query += ` OR r.title_by = 'hand' OR r.parent_set_by = 'hand'`
		}
		query += ` FROM ` + table + ` r WHERE r.source = ?`
		recordRows, err := tx.Query(query, SourceGoogleTimeline)
		if err != nil {
			return nil, err
		}
		for recordRows.Next() {
			var item candidate
			if err := recordRows.Scan(&item.id, &item.startedAt, &item.deletedAt, &item.supersededBy, &item.editedByHand); err != nil {
				recordRows.Close()
				return nil, err
			}
			candidates = append(candidates, item)
		}
		recordRows.Close()
		if err := recordRows.Err(); err != nil {
			return nil, err
		}
		for _, item := range candidates {
			var decider *snapshotImport
			for index := range snapshots {
				if snapshots[index].snapshot.CoversFrom <= item.startedAt && item.startedAt <= snapshots[index].snapshot.CoversTo {
					decider = &snapshots[index]
					break
				}
			}
			if decider == nil {
				continue
			}
			switch {
			case held[decider.id][item.id] && item.deletedAt != 0 && item.supersededBy.Valid:
				if err := snapshot(tx, entityType, item.id, SnapshotRestoredReason+decider.id, at); err != nil {
					return nil, err
				}
				if _, err := tx.Exec(`UPDATE `+table+` SET deleted_at=0, superseded_by_import_id=NULL, updated_at=? WHERE id=?`, at, item.id); err != nil {
					return nil, err
				}
				if entityType == EntityVisit {
					changes.VisitsRestored++
				} else {
					changes.TripsRestored++
				}
			case held[decider.id][item.id]:
			case item.deletedAt != 0:
				// Deleted already, by a person or an earlier reconcile.
			case item.editedByHand:
				changes.KeptEditedIDs = append(changes.KeptEditedIDs, item.id)
			default:
				if err := snapshot(tx, entityType, item.id, SnapshotSupersededReason+decider.id, at); err != nil {
					return nil, err
				}
				if _, err := tx.Exec(`UPDATE `+table+` SET deleted_at=?, superseded_by_import_id=?, updated_at=? WHERE id=?`, at, decider.id, at, item.id); err != nil {
					return nil, err
				}
				if entityType == EntityVisit {
					changes.VisitsSuperseded++
				} else {
					changes.TripsSuperseded++
				}
			}
		}
	}
	return changes, nil
}

// recordHeld notes that an import's file holds a visit or trip.
func recordHeld(tx *sql.Tx, importID, entityType, id string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO import_records (import_id, entity_type, entity_id) VALUES (?,?,?)`, importID, entityType, id)
	return err
}
