// Package llmbridge is kanban-store's client for llm-bridge-server, the
// service that owns agents (through agent-store) and harness instances
// (through harness-store). It exists for two checks, both on one write path:
// before a board's default_agent_id or default_instance_id is stored, the id
// must be one llm-bridge-server hands out.
//
// The reasoning is the one internal/principalstore states for card
// assignments. A dispatcher reads these two fields to spawn a session; a wrong
// id there is not a dangling pointer a reader can discard but a board whose
// every dispatch fails, and the failure surfaces in a cron job's log rather
// than at the PATCH that caused it. The check is one GET with a short timeout,
// and a failed check fails the write: an unreachable llm-bridge-server is a
// 502, never a silently accepted row.
package llmbridge

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

	"github.com/kayushkin/kanban-store/internal/boundedtext"
)

// Client talks to one llm-bridge-server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client with the 3-second timeout the board-settings write path
// budgets for each existence check.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 3 * time.Second},
	}
}

// ErrNotFound is returned when llm-bridge-server has no record with the id.
var ErrNotFound = errors.New("not found in llm-bridge-server")

// CheckInstanceExists asks GET /instances/{id}. A 404 is ErrNotFound. A
// transport failure is returned as the transport's own error, unwrapped, and
// any other non-200 answer as an error naming the status and body.
func (c *Client) CheckInstanceExists(id string) error {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/instances/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	// The id is the caller's, so every message below names it bounded: the
	// path as shown, and the URL inside a transport error.
	shownPath := "/instances/" + url.PathEscape(boundedtext.Value(id))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			urlError.URL = c.BaseURL + shownPath
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(body))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusNotFound && text == unroutedNotFoundBody:
		// Go's ServeMux answers a path it has no route for with exactly this
		// body; llm-bridge-server answers a missing instance with its own text
		// ("instance not found"). Reading the router's body as "does not exist"
		// would refuse every real instance while LLM_BRIDGE_URL points at the
		// wrong service.
		return fmt.Errorf("llm-bridge-server GET %s answered %s %q, which is Go's answer for a route that does not exist, not for a missing record: check that LLM_BRIDGE_URL points at llm-bridge-server", shownPath, resp.Status, text)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: instance %s", ErrNotFound, boundedtext.Value(id))
	default:
		return fmt.Errorf("llm-bridge-server GET %s: %s — %s", shownPath, resp.Status, text)
	}
}

const unroutedNotFoundBody = "404 page not found"

// CheckAgentExists scans GET /agents for the numeric id. agent-store's
// single-agent route is keyed by slug, and the slug is renameable — a name, not
// an id — so the list is the only place the id can be looked up.
func (c *Client) CheckAgentExists(id string) error {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/agents", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("llm-bridge-server GET /agents: %s, body unreadable: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llm-bridge-server GET /agents: %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var agents []struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &agents); err != nil {
		return fmt.Errorf("llm-bridge-server GET /agents: body is not a JSON array of agents with integer ids: %w", err)
	}
	for index, agent := range agents {
		if agent.ID == nil {
			return fmt.Errorf("llm-bridge-server GET /agents: agent at index %d has no id", index)
		}
		if strconv.FormatInt(*agent.ID, 10) == id {
			return nil
		}
	}
	return fmt.Errorf("%w: none of %d agents has id %s (send agent-store's numeric id, not the slug)", ErrNotFound, len(agents), boundedtext.Value(id))
}
