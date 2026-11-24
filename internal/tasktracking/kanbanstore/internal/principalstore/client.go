// Package principalstore is kanban-store's client for principal-store, the
// registry that owns who a principal is. It exists for two questions asked
// before a card assignment is written: whether the principal being assigned
// exists and is not disabled, and, for a board with an assignment pool, which
// members of the pool's group are available right now.
//
// kanban-store is otherwise deliberately dumb. It never resolves a card link's
// entity_ref, never proxies to the service an entity belongs to, and publishes
// /api/entity-types precisely so that clients resolve refs themselves. Card
// assignment is the one exception, and it is an exception on purpose. A link to
// a session that does not exist is a dangling pointer a reader can notice and
// discard; an assignment to a principal that does not exist is a silently wrong
// row forever — nothing downstream re-checks who is assigned, the reverse
// lookup (GET /api/assignments?principal_id=) would report work for someone who
// is not there, and the card would look owned when it is not. The check is one
// GET with a short timeout, and a failed check fails the write: an unreachable
// principal-store is a 502, never a silently accepted row.
package principalstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one principal-store.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client with the 3-second timeout the assignment write path
// budgets for the existence check.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 3 * time.Second},
	}
}

// Principal is the part of principal-store's record the existence check reads.
// DisabledAt is principal-store's unix-seconds stamp; 0 means the principal is
// active, and principal-store never deletes a row, so this is the only removal.
// KindContact is principal-store's kind for the outside person a ticket came
// from. Spelled here because this store reads it on the ticket write path; the
// vocabulary itself is principal-store's, served at GET /kinds.
const KindContact = "contact"

// KindHuman is principal-store's kind for a person of this deployment. Spelled
// here because a time entry names who did the work, and a group or an outside
// contact does not work hours.
const KindHuman = "human"

type Principal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// DisplayName is carried for rendering a board without a second fetch per
	// card. Nothing joins on it — the id is the handle — and it is stale the
	// moment principal-store renames the principal.
	DisplayName string `json:"display_name"`
	DisabledAt  int64  `json:"disabled_at"`
	// IsAdministrator is principal-store's one fact about a person that this
	// store acts on: an administrator sees and changes every board, granted or
	// not. See internal/api/principal_access.go.
	IsAdministrator bool `json:"is_administrator"`
}

// Disabled reports whether principal-store has retired this principal.
func (p *Principal) Disabled() bool { return p.DisabledAt != 0 }

// ErrNotFound is returned when principal-store answers 404 for the id.
var ErrNotFound = errors.New("principal not found in principal-store")

// Get fetches one principal by id. A 404 is ErrNotFound. A transport failure
// (unreachable, timeout) is returned as the transport's own error, unwrapped,
// and any other non-200 answer as an error naming the status and body, so a
// caller can surface exactly what went wrong instead of a paraphrase.
func (c *Client) Get(id string) (*Principal, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/principals/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("principal-store GET %s: %s — %s", req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	var p Principal
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("principal-store GET %s: undecodable body: %w", req.URL.Path, err)
	}
	return &p, nil
}

// ListAvailableMembers asks principal-store which active members of a group
// are available at the instant given, by each member's own declared week and
// time off (GET /principals/{group}/members?available_at=). An empty list is
// an answer — nobody is working — not an error. Any status other than 200 is
// an error naming it, so a board's pool never reads a failed call as an
// empty pool.
func (c *Client) ListAvailableMembers(groupID string, at time.Time) ([]Principal, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/principals/"+url.PathEscape(groupID)+"/members?available_at="+strconv.FormatInt(at.Unix(), 10), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("principal-store GET %s: %s — %s", req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	var members []Principal
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return nil, fmt.Errorf("principal-store GET %s: undecodable body: %w", req.URL.Path, err)
	}
	return members, nil
}
