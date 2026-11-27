package locations

import (
	"context"
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

// reconcileSnapshots reconciles one owner's snapshot imports and that
// owner's google_timeline visits and trips; another owner's exports never
// decide this owner's records.
func reconcileSnapshots(ctx context.Context, tx *sql.Tx, owner string, at int64) (*SnapshotChanges, error) {
	changes := &SnapshotChanges{KeptEditedIDs: []string{}}
	rows, err := tx.QueryContext(ctx, `SELECT id, seq, snapshot FROM imports WHERE owner_principal_id = $1 AND snapshot IS NOT NULL AND finished_at <> 0 AND kind = $2`,
		owner, ImportKindGoogleTimeline)
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
		heldRows, err := tx.QueryContext(ctx, `SELECT entity_id FROM import_records WHERE import_id = $1`, item.id)
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
		query += ` FROM ` + table + ` r WHERE r.owner_principal_id = $1 AND r.source = $2`
		recordRows, err := tx.QueryContext(ctx, query, owner, SourceGoogleTimeline)
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
				if err := snapshot(ctx, tx, entityType, item.id, SnapshotRestoredReason+decider.id, at); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET deleted_at = 0, superseded_by_import_id = NULL, updated_at = $1 WHERE id = $2`, at, item.id); err != nil {
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
				if err := snapshot(ctx, tx, entityType, item.id, SnapshotSupersededReason+decider.id, at); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET deleted_at = $1, superseded_by_import_id = $2, updated_at = $3 WHERE id = $4`, at, decider.id, at, item.id); err != nil {
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

// recordHeld notes that an owner's import's file holds a visit or trip of
// the same owner.
func recordHeld(ctx context.Context, tx *sql.Tx, owner, importID, entityType, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO import_records (owner_principal_id, import_id, entity_type, entity_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, owner, importID, entityType, id)
	return err
}
