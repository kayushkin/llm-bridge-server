package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Persons            int64 `json:"persons"`
	Identities         int64 `json:"identities"`
	Relationships      int64 `json:"relationships"`
	Suggestions        int64 `json:"suggestions"`
	SuggestionHandles  int64 `json:"suggestion_handles"`
	IdentityRefusals   int64 `json:"identity_refusals"`
	HandleActivity     int64 `json:"handle_activity"`
	HandleReviews      int64 `json:"handle_reviews"`
	HandleReviewClaims int64 `json:"handle_review_claims"`
	Revisions          int64 `json:"revisions"`
	// LegacySuggestionIdentities is how many rows of the standalone store's
	// legacy suggestion_identities table were read. Each joins
	// suggestion_handles unless that already holds it, as the standalone
	// store's own migration copied them.
	LegacySuggestionIdentities int64 `json:"legacy_suggestion_identities"`
}

// Summary names each count, and nothing of the records.
func (counts ImportCounts) Summary() string {
	return fmt.Sprintf("persons=%d identities=%d relationships=%d suggestions=%d suggestion_handles=%d legacy_suggestion_identities=%d identity_refusals=%d handle_activity=%d handle_reviews=%d handle_review_claims=%d revisions=%d",
		counts.Persons, counts.Identities, counts.Relationships, counts.Suggestions, counts.SuggestionHandles, counts.LegacySuggestionIdentities,
		counts.IdentityRefusals, counts.HandleActivity, counts.HandleReviews, counts.HandleReviewClaims, counts.Revisions)
}

// standaloneTables are the tables of the standalone store's SQLite file the
// import reads, with their columns as they ran live: schema.sql and the three
// columns its migrate() added to identities. persons_fts is not read: the
// search is rebuilt from the rows.
var standaloneTables = map[string][]string{
	"persons":               {"id", "seq", "display_name", "birthday", "how_met", "tags", "principal_id", "created_at", "updated_at", "deleted_at"},
	"identities":            {"id", "seq", "person_id", "kind", "value", "label", "created_at", "updated_at", "linked_by", "reason", "linked_at"},
	"relationships":         {"id", "seq", "from_person_id", "to_person_id", "kind", "note", "created_at"},
	"suggestions":           {"id", "seq", "source", "payload", "status", "person_id", "created_at", "decided_at"},
	"suggestion_identities": {"suggestion_id", "kind", "value"},
	"suggestion_handles":    {"suggestion_id", "kind", "value"},
	"identity_refusals":     {"id", "seq", "kind", "value", "person_id", "refused_by", "reason", "created_at"},
	"handle_reviews":        {"kind", "value", "reviewed_by", "reviewed_at"},
	"handle_review_claims":  {"kind", "value", "claimed_by", "claimed_at"},
	"handle_activity":       {"kind", "value", "source", "last_activity_at", "message_count", "direct_chat", "observed_at"},
	"revisions":             {"id", "seq", "person_id", "action", "snapshot", "created_at"},
}

// moduleTables are the module's tables, every one of which must be empty
// before an import.
var moduleTables = []string{"persons", "identities", "relationships", "suggestions", "suggestion_handles", "identity_refusals",
	"handle_reviews", "handle_review_claims", "handle_activity", "revisions"}

// ImportStandaloneDatabase copies every row of the standalone people-store's
// SQLite file at sqlitePath into the module behind db, every record owned by
// ownerPrincipalID, with every id, time and soft-deleted row as it was. It
// runs in one transaction, so it copies everything or nothing.
//
// It refuses an empty or malformed owner, a module that already holds any
// row, and a file it cannot copy whole: a table with a column it does not
// know, an id that is not the form its number makes, or a row pointing at a
// record the file does not hold. Each revision's snapshot gains the owner, as
// one taken now would carry it. The search is rebuilt from the copied
// identities, and each table numbers on after the highest number copied, so
// no id that chats, notes and links already carry is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (ImportCounts, error) {
	var counts ImportCounts
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return counts, err
	}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	for table, columns := range standaloneTables {
		if err := standalonesqlite.CheckColumns(ctx, source, "people-store", table, columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "people", moduleTables...); err != nil {
			return err
		}
		last := map[string]int64{}
		// numbered reads a row's id and seq, refuses an id its number does not
		// make, and keeps the highest number of the table.
		numbered := func(table, prefix string, row []any) (int64, error) {
			id, err := standalonesqlite.Text("id", row[0])
			if err != nil {
				return 0, err
			}
			seq, err := standalonesqlite.Integer("seq", row[1])
			if err != nil {
				return 0, err
			}
			if id != formatID(prefix, seq) {
				return 0, fmt.Errorf("id %q has number %d, whose id is %s; the import keeps ids by their number and will not change one", id, seq, formatID(prefix, seq))
			}
			last[table] = max(last[table], seq)
			return seq, nil
		}
		reference := func(prefix, column string, value any) (int64, error) {
			id, err := standalonesqlite.Text(column, value)
			if err != nil {
				return 0, err
			}
			seq, err := postgresstorage.ParseRecordID(prefix, id)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", column, err)
			}
			return seq, nil
		}
		tables := []struct {
			table standalonesqlite.Table
			count *int64
		}{
			{standalonesqlite.Table{
				Name: "persons", Columns: standaloneTables["persons"], OrderBy: "seq",
				Insert: `INSERT INTO persons (seq, owner_principal_id, display_name, birthday, how_met, tags, principal_id, created_at, updated_at, deleted_at)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10)`,
				Convert: func(row []any) ([]any, error) {
					seq, err := numbered("persons", personIDPrefix, row)
					if err != nil {
						return nil, err
					}
					tags, err := standaloneTags(row[5])
					if err != nil {
						return nil, err
					}
					return []any{seq, ownerPrincipalID, row[2], row[3], row[4], tags, row[6], row[7], row[8], row[9]}, nil
				},
			}, &counts.Persons},
			{standalonesqlite.Table{
				Name: "identities", Columns: standaloneTables["identities"], OrderBy: "seq",
				Insert: `INSERT INTO identities (seq, owner_principal_id, person_seq, kind, value, label, created_at, updated_at, linked_by, reason, linked_at)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				Convert: func(row []any) ([]any, error) {
					seq, err := numbered("identities", identityIDPrefix, row)
					if err != nil {
						return nil, err
					}
					person, err := reference(personIDPrefix, "person_id", row[2])
					if err != nil {
						return nil, err
					}
					return []any{seq, ownerPrincipalID, person, row[3], row[4], row[5], row[6], row[7], row[8], row[9], row[10]}, nil
				},
			}, &counts.Identities},
			{standalonesqlite.Table{
				Name: "relationships", Columns: standaloneTables["relationships"], OrderBy: "seq",
				Insert: `INSERT INTO relationships (seq, owner_principal_id, from_person_seq, to_person_seq, kind, note, created_at)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				Convert: func(row []any) ([]any, error) {
					seq, err := numbered("relationships", relationshipIDPrefix, row)
					if err != nil {
						return nil, err
					}
					from, err := reference(personIDPrefix, "from_person_id", row[2])
					if err != nil {
						return nil, err
					}
					to, err := reference(personIDPrefix, "to_person_id", row[3])
					if err != nil {
						return nil, err
					}
					return []any{seq, ownerPrincipalID, from, to, row[4], row[5], row[6]}, nil
				},
			}, &counts.Relationships},
			{standalonesqlite.Table{
				Name: "suggestions", Columns: standaloneTables["suggestions"], OrderBy: "seq",
				Insert: `INSERT INTO suggestions (seq, owner_principal_id, source, payload, status, person_seq, created_at, decided_at)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				Convert: func(row []any) ([]any, error) {
					seq, err := numbered("suggestions", suggestionIDPrefix, row)
					if err != nil {
						return nil, err
					}
					personID, err := standalonesqlite.Text("person_id", row[5])
					if err != nil {
						return nil, err
					}
					var person sql.NullInt64
					if personID != "" {
						if person.Int64, err = postgresstorage.ParseRecordID(personIDPrefix, personID); err != nil {
							return nil, fmt.Errorf("person_id: %w", err)
						}
						person.Valid = true
					}
					return []any{seq, ownerPrincipalID, row[2], row[3], row[4], person, row[6], row[7]}, nil
				},
			}, &counts.Suggestions},
			{standalonesqlite.Table{
				Name: "suggestion_handles", Columns: standaloneTables["suggestion_handles"], OrderBy: "suggestion_id, kind, value",
				Insert: `INSERT INTO suggestion_handles (owner_principal_id, suggestion_seq, kind, value) VALUES ($1, $2, $3, $4)`,
				Convert: func(row []any) ([]any, error) {
					suggestion, err := reference(suggestionIDPrefix, "suggestion_id", row[0])
					if err != nil {
						return nil, err
					}
					return []any{ownerPrincipalID, suggestion, row[1], row[2]}, nil
				},
			}, &counts.SuggestionHandles},
			{standalonesqlite.Table{
				Name: "suggestion_identities", Columns: standaloneTables["suggestion_identities"], OrderBy: "suggestion_id, kind, value",
				Insert: `INSERT INTO suggestion_handles (owner_principal_id, suggestion_seq, kind, value) VALUES ($1, $2, $3, $4)
					ON CONFLICT (suggestion_seq, kind, value) DO NOTHING`,
				Convert: func(row []any) ([]any, error) {
					suggestion, err := reference(suggestionIDPrefix, "suggestion_id", row[0])
					if err != nil {
						return nil, err
					}
					return []any{ownerPrincipalID, suggestion, row[1], row[2]}, nil
				},
			}, &counts.LegacySuggestionIdentities},
			{standalonesqlite.Table{
				Name: "identity_refusals", Columns: standaloneTables["identity_refusals"], OrderBy: "seq",
				Insert: `INSERT INTO identity_refusals (seq, owner_principal_id, kind, value, person_seq, refused_by, reason, created_at)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				Convert: func(row []any) ([]any, error) {
					seq, err := numbered("identity_refusals", refusalIDPrefix, row)
					if err != nil {
						return nil, err
					}
					person, err := reference(personIDPrefix, "person_id", row[4])
					if err != nil {
						return nil, err
					}
					return []any{seq, ownerPrincipalID, row[2], row[3], person, row[5], row[6], row[7]}, nil
				},
			}, &counts.IdentityRefusals},
			{standalonesqlite.Table{
				Name: "handle_activity", Columns: standaloneTables["handle_activity"], OrderBy: "kind, value",
				Insert: `INSERT INTO handle_activity (owner_principal_id, kind, value, source, last_activity_at, message_count, direct_chat, observed_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				Convert: func(row []any) ([]any, error) {
					directChat, err := standalonesqlite.Boolean("direct_chat", row[5])
					if err != nil {
						return nil, err
					}
					return []any{ownerPrincipalID, row[0], row[1], row[2], row[3], row[4], directChat, row[6]}, nil
				},
			}, &counts.HandleActivity},
			{standalonesqlite.Table{
				Name: "handle_reviews", Columns: standaloneTables["handle_reviews"], OrderBy: "kind, value",
				Insert: `INSERT INTO handle_reviews (owner_principal_id, kind, value, reviewed_by, reviewed_at) VALUES ($1, $2, $3, $4, $5)`,
				Convert: func(row []any) ([]any, error) {
					return append([]any{ownerPrincipalID}, row...), nil
				},
			}, &counts.HandleReviews},
			{standalonesqlite.Table{
				Name: "handle_review_claims", Columns: standaloneTables["handle_review_claims"], OrderBy: "kind, value",
				Insert: `INSERT INTO handle_review_claims (owner_principal_id, kind, value, claimed_by, claimed_at) VALUES ($1, $2, $3, $4, $5)`,
				Convert: func(row []any) ([]any, error) {
					return append([]any{ownerPrincipalID}, row...), nil
				},
			}, &counts.HandleReviewClaims},
			{standalonesqlite.Table{
				Name: "revisions", Columns: standaloneTables["revisions"], OrderBy: "seq",
				Insert: `INSERT INTO revisions (seq, owner_principal_id, person_seq, action, snapshot, created_at)
					OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
				Convert: func(row []any) ([]any, error) {
					seq, err := numbered("revisions", revisionIDPrefix, row)
					if err != nil {
						return nil, err
					}
					person, err := reference(personIDPrefix, "person_id", row[2])
					if err != nil {
						return nil, err
					}
					snapshot, err := snapshotWithOwner(row[4], ownerPrincipalID)
					if err != nil {
						return nil, err
					}
					return []any{seq, ownerPrincipalID, person, row[3], snapshot, row[5]}, nil
				},
			}, &counts.Revisions},
		}
		for _, entry := range tables {
			copied, err := standalonesqlite.Copy(ctx, source, tx, entry.table)
			if err != nil {
				return err
			}
			*entry.count = copied
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET identity_search_text = COALESCE((
			SELECT string_agg(i.value || ' ' || i.label, ' ' ORDER BY i.seq) FROM identities i
			WHERE i.owner_principal_id = persons.owner_principal_id AND i.person_seq = persons.seq), '')`); err != nil {
			return fmt.Errorf("rebuilding the search: %w", err)
		}
		for _, table := range []string{"persons", "identities", "relationships", "suggestions", "identity_refusals", "revisions"} {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table, "seq", last[table]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// standaloneTags checks the stored tags are a JSON list of strings and
// returns them as written.
func standaloneTags(value any) (string, error) {
	raw, err := standalonesqlite.Text("tags", value)
	if err != nil {
		return "", err
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil || tags == nil {
		return "", fmt.Errorf("its tags are not a JSON list of strings")
	}
	return raw, nil
}

// snapshotWithOwner adds owner_principal_id to a revision's snapshot, which
// the standalone store took before records had owners, and keeps every other
// field as it was.
func snapshotWithOwner(value any, ownerPrincipalID string) (string, error) {
	raw, err := standalonesqlite.Text("snapshot", value)
	if err != nil {
		return "", err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
		return "", fmt.Errorf("its snapshot is not a JSON object")
	}
	if existing, present := fields["owner_principal_id"]; present && strings.TrimSpace(string(existing)) != `""` {
		return "", fmt.Errorf("its snapshot already names an owner")
	}
	encodedOwner, err := json.Marshal(ownerPrincipalID)
	if err != nil {
		return "", err
	}
	fields["owner_principal_id"] = encodedOwner
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
