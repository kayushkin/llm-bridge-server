// Package noteboarditems reads EVERY noteboard item, held ones included, and
// says when it could not.
//
// Both the autoworker and spend-ceiling-guard build internal/spendtree from this
// read, so they share it: spendtree exists so the two cannot disagree about a
// number, and they would if each fetched its own input for it.
//
// Each used to ask for `limit=10000` in one request and take whatever came.
// Measured 2026-08-29 the board held 10,309 items with `include_held` unset
// alone; measured 2026-09-27 it held 13,042. A short read is not an error — the
// request returns 200 and decodes — so the rows past the cut were silently
// missing from the tree. In the guard that prices ceilings too low. In the
// autoworker it is worse: spendtree.EffectiveCeiling stops at an ancestor that
// is not in the tree, so a card whose ceiling owner fell off the end looked like
// a card with no ceiling, and went out with no budget cap at all.
//
// Raising the constant would have bought the same defect a later date. So the
// read pages, and the only remaining bound is reported to the caller.
package issueitemsclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Item is the slice of a noteboard item the spend tree and its callers need.
type Item struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	ParentID      *string  `json:"parent_id"`
	HeldAt        *string  `json:"held_at"`
	AutoHoldAtUSD *float64 `json:"auto_hold_at_usd"`
}

// PageSize and MaxPages bound the paged read. Their product is the point at
// which the read gives up and says so: a bound that is reached silently is the
// defect this replaced.
const (
	PageSize = 1000
	MaxPages = 200
)

// FetchEveryItemIncludingHeld reads every noteboard item and reports whether it
// stopped at MaxPages before the end of the list.
//
// include_held is required: a held ancestor still owns its subtree's ceiling,
// and noteboard hides held items by default, so without it a parent edge
// dangles.
//
// Ordering is `created_at DESC, id`, a total order, so the window is stable
// across pages. An item created mid-read shifts rows toward later pages and can
// hand the same id back twice, which the dedupe below absorbs; an item DELETED
// mid-read shifts them the other way and can drop one, which it cannot. Both
// callers read again within minutes, and the next read sees the row.
func FetchEveryItemIncludingHeld(noteboardURL string) (items []Item, truncated bool, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	seen := map[string]bool{}
	for page := 0; page < MaxPages; page++ {
		url := fmt.Sprintf("%s/api/items?include_held=true&limit=%d&offset=%d",
			noteboardURL, PageSize, page*PageSize)
		resp, err := client.Get(url)
		if err != nil {
			return nil, false, fmt.Errorf("noteboard items: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, false, fmt.Errorf("noteboard items: %s", resp.Status)
		}
		var batch []Item
		decodeErr := json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, false, fmt.Errorf("decode noteboard items: %w", decodeErr)
		}
		for _, it := range batch {
			if seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			items = append(items, it)
		}
		// A page shorter than it asked for is the end of the list. This is the
		// normal exit, and it is the only one that returns truncated=false.
		if len(batch) < PageSize {
			return items, false, nil
		}
	}
	return items, true, nil
}
