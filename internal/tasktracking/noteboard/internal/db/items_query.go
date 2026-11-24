package db

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kayushkin/noteboard/model"
)

// itemsQueryOrder is each sort as SQL over items joined to the sent ids as
// `sent` (json_each: key is the position an id was sent at). Every order ends
// in sent.key, which no two rows share, so a page is the same page every time.
var itemsQueryOrder = map[model.ItemsQuerySort]string{
	model.ItemsQuerySortUpdatedAsc: "items.updated_at ASC, sent.key",
	model.ItemsQuerySortGiven:      "sent.key",
	model.ItemsQuerySortPriority:   "items.priority DESC, sent.key",
	// due_at is stored as RFC 3339 with whatever offset its writer sent: on
	// this host 30 rows end in Z and 17 in -07:00 (measured 2026-09-20). As
	// strings, 10:00-07:00 sorts before 12:00Z though it is five hours later,
	// so the instant is compared, not the text.
	model.ItemsQuerySortDueAt:     "items.due_at IS NULL, julianday(items.due_at) ASC, sent.key",
	model.ItemsQuerySortUpdatedAt: "items.updated_at DESC, sent.key",
	model.ItemsQuerySortCreatedAt: "items.created_at DESC, sent.key",
	model.ItemsQuerySortTitle:     "items.title COLLATE NOCASE ASC, sent.key",
}

// QueryItems answers which of the named items match, in order. See
// model.ItemsQuery for what it is for and why held items are answered.
func (s *Store) QueryItems(query *model.ItemsQuery) (*model.ItemsQueryResult, error) {
	result := &model.ItemsQueryResult{IDs: []string{}, MissingIDs: []string{}}
	if len(query.IDs) == 0 {
		return result, nil
	}
	sent, err := json.Marshal(query.IDs)
	if err != nil {
		return nil, err
	}

	// Which of the sent ids name no live item, whatever the filter says.
	missing, err := s.readOnlyDB.Query(`SELECT sent.value FROM json_each(?) AS sent
		WHERE NOT EXISTS (SELECT 1 FROM items WHERE items.id = sent.value AND items.deleted_at IS NULL)
		ORDER BY sent.key`, string(sent))
	if err != nil {
		return nil, err
	}
	for missing.Next() {
		var id string
		if err := missing.Scan(&id); err != nil {
			missing.Close()
			return nil, err
		}
		result.MissingIDs = append(result.MissingIDs, id)
	}
	if err := missing.Close(); err != nil {
		return nil, err
	}

	where := []string{"items.deleted_at IS NULL"}
	args := []any{string(sent)}
	for _, tag := range query.Tags {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(items.tags) WHERE json_each.value = ?)")
		args = append(args, tag)
	}
	if len(query.Priorities) > 0 {
		where = append(where, "items.priority IN ("+placeholders(len(query.Priorities))+")")
		for _, priority := range query.Priorities {
			args = append(args, priority)
		}
	}
	if len(query.Statuses) > 0 {
		where = append(where, "items.status IN ("+placeholders(len(query.Statuses))+")")
		for _, status := range query.Statuses {
			args = append(args, status)
		}
	}
	if query.DueBefore != nil {
		// Instants, not text: see itemsQueryOrder.
		where = append(where, "items.due_at IS NOT NULL AND julianday(items.due_at) < julianday(?)")
		args = append(args, query.DueBefore.UTC().Format(time.RFC3339))
	}
	if query.Q != "" {
		where = append(where, "instr(unicode_lower(items.title || char(10) || items.body || char(10) || items.id), ?) > 0")
		args = append(args, strings.ToLower(query.Q))
	}
	if query.CreatedFrom != nil {
		where = append(where, "julianday(items.created_at) >= julianday(?)")
		args = append(args, query.CreatedFrom.UTC().Format(time.RFC3339Nano))
	}
	if query.CreatedTo != nil {
		where = append(where, "julianday(items.created_at) <= julianday(?)")
		args = append(args, query.CreatedTo.UTC().Format(time.RFC3339Nano))
	}
	// CROSS JOIN, not JOIN, and it is not style. SQLite is free to reorder a
	// JOIN and, given an index on deleted_at, it made items the outer loop —
	// which re-parses the whole JSON list of ids once per item. On this host
	// that was 7,830 items times a 371 KB list: the query ran for minutes at
	// 100% CPU (measured 2026-09-20). SQLite never reorders a CROSS JOIN, so
	// the ids are walked once and each item is found by its primary key.
	// TestItemsQueryWalksTheIdsOnce pins the plan.
	from := itemsQueryFrom + strings.Join(where, " AND ")

	if err := s.readOnlyDB.QueryRow(`SELECT COUNT(*)`+from, args...).Scan(&result.Total); err != nil {
		return nil, err
	}

	sort := query.Sort
	if sort == "" {
		sort = model.ItemsQuerySortGiven
	}
	page := `SELECT ` + qualifiedItemCols + from + ` ORDER BY ` + itemsQueryOrder[sort]
	if query.Limit > 0 {
		page += ` LIMIT ? OFFSET ?`
		args = append(args, query.Limit, query.Offset)
	} else if query.Offset > 0 {
		page += ` LIMIT -1 OFFSET ?`
		args = append(args, query.Offset)
	}
	rows, err := s.readOnlyDB.Query(page, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		result.IDs = append(result.IDs, item.ID)
		if query.IncludeItems {
			result.Items = append(result.Items, item)
		}
	}
	return result, rows.Err()
}

const itemsQueryFrom = ` FROM json_each(?) AS sent CROSS JOIN items ON items.id = sent.value WHERE `

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// qualifiedItemCols is itemCols with every column named as items.<column>,
// because json_each also has columns called id, type and key.
var qualifiedItemCols = func() string {
	columns := strings.Split(itemCols, ", ")
	for i, column := range columns {
		columns[i] = "items." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}()
