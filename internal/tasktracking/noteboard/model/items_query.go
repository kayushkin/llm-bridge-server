package model

import (
	"fmt"
	"strings"
	"time"
)

// ItemsQuerySort is an order POST /api/items/query can answer in.
type ItemsQuerySort string

const (
	// ItemsQuerySortGiven keeps the order the ids were sent in. It is the
	// default, because the caller that names the ids usually owns an order of
	// its own: kanban-store sends a column's cards in the order they sit.
	ItemsQuerySortGiven ItemsQuerySort = "given"
	// ItemsQuerySortPriority is highest priority first.
	ItemsQuerySortPriority ItemsQuerySort = "priority"
	// ItemsQuerySortDueAt is soonest due first, and items with no due date last.
	ItemsQuerySortDueAt ItemsQuerySort = "due_at"
	// ItemsQuerySortUpdatedAt is most recently changed first.
	ItemsQuerySortUpdatedAt ItemsQuerySort = "updated_at"
	// ItemsQuerySortCreatedAt is newest first.
	ItemsQuerySortCreatedAt ItemsQuerySort = "created_at"
	// ItemsQuerySortTitle is by title, ignoring case.
	ItemsQuerySortTitle      ItemsQuerySort = "title"
	ItemsQuerySortUpdatedAsc ItemsQuerySort = "updated_asc"
)

// ItemsQuerySorts is the vocabulary, served by GET /api/items/query-options.
var ItemsQuerySorts = []ItemsQuerySort{
	ItemsQuerySortGiven, ItemsQuerySortPriority, ItemsQuerySortDueAt,
	ItemsQuerySortUpdatedAt, ItemsQuerySortCreatedAt, ItemsQuerySortTitle, ItemsQuerySortUpdatedAsc,
}

// MaxItemsQueryIDs bounds one query. The largest kanban board here held 9,503
// cards when this was written; this leaves room and still refuses a mistake.
const MaxItemsQueryIDs = 50000

// MaxItemsQueryLimit bounds one page.
const MaxItemsQueryLimit = 1000

// ItemsQueryOptions is GET /api/items/query-options: what a caller must not
// hardcode.
type ItemsQueryOptions struct {
	Sorts    []ItemsQuerySort `json:"sorts"`
	MaxIDs   int              `json:"max_ids"`
	MaxLimit int              `json:"max_limit"`
}

// ItemsQuery is POST /api/items/query: of THESE items, which match, in what
// order. It exists for a caller that owns a set of item ids and none of what
// the items say — kanban-store knows which cards sit in a column and nothing of
// their tags or priority — so that filtering and sorting such a set is one call
// here and not one read per item there.
//
// An item must match every filter that is set. Held items are answered: the
// caller named the ids, exactly as it would to GET each one, and the hold gate
// is for discovery, which this is not. Deleted items are not.
type ItemsQuery struct {
	IDs []string `json:"ids"`
	// Q is a literal, case-insensitive substring of title, body or id.
	Q           string     `json:"q,omitempty"`
	CreatedFrom *time.Time `json:"created_from,omitempty"`
	CreatedTo   *time.Time `json:"created_to,omitempty"`
	// Tags keeps items carrying ALL of these, matched exactly.
	Tags []string `json:"tags,omitempty"`
	// Priorities keeps items whose priority is ANY of these.
	Priorities []int `json:"priorities,omitempty"`
	// Statuses keeps items whose status is ANY of these.
	Statuses []string `json:"statuses,omitempty"`
	// DueBefore keeps items due before this moment. An item with no due date
	// is not due before anything.
	DueBefore *time.Time `json:"due_before,omitempty"`

	// Sort is one of ItemsQuerySorts; empty is `given`. Ties always fall back
	// to the order the ids were sent in, so the answer is the same every time.
	Sort ItemsQuerySort `json:"sort,omitempty"`
	// Limit and Offset page the matches, after the filter and the sort. A zero
	// limit answers them all.
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
	// IncludeItems answers the matching page's items as well as their ids.
	IncludeItems bool `json:"include_items,omitempty"`
}

func (q *ItemsQuery) Validate() error {
	if len(q.IDs) > MaxItemsQueryIDs {
		return fmt.Errorf("ids has %d entries; one query takes at most %d", len(q.IDs), MaxItemsQueryIDs)
	}
	for _, id := range q.IDs {
		if id == "" {
			return fmt.Errorf("ids has an empty entry")
		}
	}
	for _, tag := range q.Tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("tags has an empty entry")
		}
	}
	for _, status := range q.Statuses {
		// noteboard stores status as the caller wrote it and keeps no list of
		// valid ones, so neither does this filter.
		if strings.TrimSpace(status) == "" {
			return fmt.Errorf("statuses has an empty entry")
		}
	}
	if q.Sort != "" {
		known := false
		for _, sort := range ItemsQuerySorts {
			known = known || sort == q.Sort
		}
		if !known {
			return fmt.Errorf("sort %q is not one of %v (GET /api/items/query-options)", q.Sort, ItemsQuerySorts)
		}
	}
	if q.Limit < 0 || q.Limit > MaxItemsQueryLimit {
		return fmt.Errorf("limit must be between 0 and %d", MaxItemsQueryLimit)
	}
	if q.Offset < 0 {
		return fmt.Errorf("offset must not be negative")
	}
	return nil
}

// ItemsQueryResult is the answer.
type ItemsQueryResult struct {
	// Total is how many of the ids matched, before paging: what makes "showing
	// 25 of 310" true.
	Total int `json:"total"`
	// IDs is the matching page, in order.
	IDs []string `json:"ids"`
	// Items is the same page as items, in the same order, when asked for.
	Items []*Item `json:"items,omitempty"`
	// MissingIDs are the ids sent that name no live item — deleted, or never
	// there. They are reported whatever the filter, so a caller can tell "did
	// not match" from "is gone", which kanban-store shows as an orphaned card.
	MissingIDs []string `json:"missing_ids"`
}
