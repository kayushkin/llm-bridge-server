package items

import (
	"encoding/json"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

// itemsQueryOrder is each sort as SQL over items joined to the sent ids as
// `sent` (sent.position is the place an id was sent at, from 1). Every order
// ends in sent.position, which no two rows share, so a page is the same page
// every time.
var itemsQueryOrder = map[model.ItemsQuerySort]string{
	model.ItemsQuerySortUpdatedAsc: "items.updated_at ASC, sent.position",
	model.ItemsQuerySortGiven:      "sent.position",
	model.ItemsQuerySortPriority:   "items.priority DESC, sent.position",
	// due_at is an instant, so 10:00-07:00 sorts after 12:00Z, as it is five
	// hours later; the offset its writer sent is kept beside it and never read
	// here.
	model.ItemsQuerySortDueAt:     "items.due_at IS NULL, items.due_at ASC, sent.position",
	model.ItemsQuerySortUpdatedAt: "items.updated_at DESC, sent.position",
	model.ItemsQuerySortCreatedAt: "items.created_at DESC, sent.position",
	// Case is ignored in ASCII letters only, and the rest compared by bytes,
	// as SQLite's NOCASE did.
	model.ItemsQuerySortTitle: `lower(items.title COLLATE "C") ASC, sent.position`,
}

// sentIDs is the ids a query names, as rows: value is the id and position its
// place in the list sent, from 1.
const sentIDs = `jsonb_array_elements_text($1::jsonb) WITH ORDINALITY AS sent(value, position)`

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
	missing, err := s.db.Query(`SELECT sent.value FROM `+sentIDs+`
		WHERE NOT EXISTS (SELECT 1 FROM items WHERE items.id = sent.value AND items.deleted_at IS NULL)
		ORDER BY sent.position`, string(sent))
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

	arguments := &queryArguments{}
	arguments.add(string(sent))
	where := []string{"items.deleted_at IS NULL"}
	for _, tag := range query.Tags {
		where = append(where, hasTag(arguments, tag))
	}
	if len(query.Priorities) > 0 {
		placeholders := make([]string, 0, len(query.Priorities))
		for _, priority := range query.Priorities {
			placeholders = append(placeholders, arguments.add(priority))
		}
		where = append(where, "items.priority IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(query.Statuses) > 0 {
		placeholders := make([]string, 0, len(query.Statuses))
		for _, status := range query.Statuses {
			placeholders = append(placeholders, arguments.add(status))
		}
		where = append(where, "items.status IN ("+strings.Join(placeholders, ",")+")")
	}
	if query.DueBefore != nil {
		where = append(where, "items.due_at IS NOT NULL AND items.due_at < "+arguments.add(query.DueBefore.UTC()))
	}
	if query.Q != "" {
		// A literal substring: strpos reads no wildcard, so % and _ are text.
		// pg_c_utf8 lowers every letter by Unicode's simple case mapping, as
		// strings.ToLower does to the query, whatever the database's locale.
		where = append(where, "strpos(lower((items.title || E'\\n' || items.body || E'\\n' || items.id) COLLATE pg_c_utf8), "+arguments.add(strings.ToLower(query.Q))+") > 0")
	}
	if query.CreatedFrom != nil {
		where = append(where, "items.created_at >= "+arguments.add(query.CreatedFrom.UTC()))
	}
	if query.CreatedTo != nil {
		where = append(where, "items.created_at <= "+arguments.add(query.CreatedTo.UTC()))
	}
	from := ` FROM ` + sentIDs + ` JOIN items ON items.id = sent.value WHERE ` + strings.Join(where, " AND ")

	if err := s.db.QueryRow(`SELECT COUNT(*)`+from, arguments.values...).Scan(&result.Total); err != nil {
		return nil, err
	}

	sort := query.Sort
	if sort == "" {
		sort = model.ItemsQuerySortGiven
	}
	page := `SELECT ` + qualifiedItemCols + from + ` ORDER BY ` + itemsQueryOrder[sort]
	if query.Limit > 0 {
		page += ` LIMIT ` + arguments.add(query.Limit)
	}
	if query.Offset > 0 {
		page += ` OFFSET ` + arguments.add(query.Offset)
	}
	items, err := queryItems(s.db, page, arguments.values...)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		result.IDs = append(result.IDs, item.ID)
		if query.IncludeItems {
			result.Items = append(result.Items, item)
		}
	}
	return result, nil
}

// qualifiedItemCols is itemCols with every column named as items.<column>,
// because sent has a column called value too.
var qualifiedItemCols = func() string {
	columns := strings.Split(itemCols, ", ")
	for i, column := range columns {
		columns[i] = "items." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}()
