package locationstore

import (
	"database/sql"
	"fmt"
	"strings"
)

// reasonMerge is the revision reason of every record a merge moves. It is not
// a hand edit (update, attach, restore), so imports and phone segmenting keep
// treating moved records as their own.
const reasonMerge = "merge"

// MergePlace merges place id into place into, two records of one real
// place: every visit, trip end, calendar link, recurring-series spot and home
// period that named id names into instead, each moved record first copied to
// revisions (reason merge); a place merged into id earlier now points at into;
// and id is soft-deleted with merged_into_place_id set, so a later export that
// names it by Google's id lands on into. The kept place's own fields are
// untouched. All in one transaction.
func (s *Store) MergePlace(id string, request PlaceMergeRequest) (*PlaceMerge, error) {
	into := strings.TrimSpace(request.Into)
	if into == "" {
		return nil, fmt.Errorf("%w: into is required: the place to keep", ErrInvalid)
	}
	if into == id {
		return nil, fmt.Errorf("%w: a place cannot be merged into itself", ErrInvalid)
	}
	merge := &PlaceMerge{MergedPlaceID: id}
	err := s.inTransaction(func(tx *sql.Tx) error {
		merged, err := getPlaceRow(tx, id)
		if err != nil {
			return err
		}
		kept, err := getPlaceRow(tx, into)
		if err != nil {
			return err
		}
		switch {
		case merged.DeletedAt != 0:
			return fmt.Errorf("%w: %s is deleted; restore it first", ErrConflict, id)
		case kept.DeletedAt != 0:
			return fmt.Errorf("%w: %s, the place to keep, is deleted", ErrConflict, into)
		}
		at := now()
		if merge.VisitsMoved, err = moveReferences(tx, EntityVisit, "visits", "place_id", id, into, at); err != nil {
			return err
		}
		if merge.TripStartsMoved, err = moveReferences(tx, EntityTrip, "trips", "start_place_id", id, into, at); err != nil {
			return err
		}
		if merge.TripEndsMoved, err = moveReferences(tx, EntityTrip, "trips", "end_place_id", id, into, at); err != nil {
			return err
		}
		if merge.CalendarLinksMoved, merge.CalendarLinksDuplicate, err = moveCalendarLinks(tx, id, into, at); err != nil {
			return err
		}
		// Series spots and home periods are rewritten by their own runs; moving
		// them here keeps them right until then.
		result, err := tx.Exec(`UPDATE calendar_series_places SET place_id = ? WHERE place_id = ?`, into, id)
		if err != nil {
			return err
		}
		if merge.CalendarSeriesMoved, err = rowsAffected(result); err != nil {
			return err
		}
		if result, err = tx.Exec(`UPDATE home_periods SET place_id = ? WHERE place_id = ?`, into, id); err != nil {
			return err
		}
		if merge.HomePeriodsMoved, err = rowsAffected(result); err != nil {
			return err
		}
		earlier, err := tx.Query(`SELECT id FROM places WHERE merged_into_place_id = ?`, id)
		if err != nil {
			return err
		}
		var repoint []string
		for earlier.Next() {
			var earlierID string
			if err := earlier.Scan(&earlierID); err != nil {
				earlier.Close()
				return err
			}
			repoint = append(repoint, earlierID)
		}
		earlier.Close()
		if err := earlier.Err(); err != nil {
			return err
		}
		for _, earlierID := range repoint {
			if err := snapshot(tx, EntityPlace, earlierID, reasonMerge, at); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE places SET merged_into_place_id = ?, updated_at = ? WHERE id = ?`, into, at, earlierID); err != nil {
				return err
			}
		}
		merge.PlacesRepointed = len(repoint)
		if err := snapshot(tx, EntityPlace, id, reasonMerge, at); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE places SET deleted_at = ?, merged_into_place_id = ?, updated_at = ? WHERE id = ?`, at, into, at, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	kept, err := s.GetPlace(into, false)
	if err != nil {
		return nil, err
	}
	merge.Place = *kept
	return merge, nil
}

// moveReferences points every record of table whose column names from at to,
// deleted records too, copying each to revisions first.
func moveReferences(tx *sql.Tx, entityType, table, column, from, to string, at int64) (int, error) {
	rows, err := tx.Query(`SELECT id FROM `+table+` WHERE `+column+` = ?`, from)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := snapshot(tx, entityType, id, reasonMerge, at); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ?, updated_at = ? WHERE id = ?`, to, at, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// moveCalendarLinks points calendar links to the merged place at the kept
// one. A link whose event already links to the kept place is a duplicate,
// soft-deleted with deleted_by merge.
func moveCalendarLinks(tx *sql.Tx, from, to string, at int64) (moved, duplicate int, err error) {
	rows, err := tx.Query(`SELECT l.id, EXISTS (SELECT 1 FROM calendar_event_links k WHERE k.calendar_id = l.calendar_id
			AND k.event_id = l.event_id AND k.target_type = 'place' AND k.target_id = ?)
		FROM calendar_event_links l WHERE l.target_type = 'place' AND l.target_id = ?`, to, from)
	if err != nil {
		return 0, 0, err
	}
	type link struct {
		id        string
		duplicate bool
	}
	var links []link
	for rows.Next() {
		var found link
		if err := rows.Scan(&found.id, &found.duplicate); err != nil {
			rows.Close()
			return 0, 0, err
		}
		links = append(links, found)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, found := range links {
		if found.duplicate {
			if _, err := tx.Exec(`UPDATE calendar_event_links SET deleted_at = CASE WHEN deleted_at = 0 THEN ? ELSE deleted_at END,
				deleted_by = CASE WHEN deleted_at = 0 THEN ? ELSE deleted_by END, updated_at = ? WHERE id = ?`,
				at, reasonMerge, at, found.id); err != nil {
				return 0, 0, err
			}
			duplicate++
			continue
		}
		if _, err := tx.Exec(`UPDATE calendar_event_links SET target_id = ?, updated_at = ? WHERE id = ?`, to, at, found.id); err != nil {
			return 0, 0, err
		}
		moved++
	}
	return moved, duplicate, nil
}

func rowsAffected(result sql.Result) (int, error) {
	count, err := result.RowsAffected()
	return int(count), err
}
