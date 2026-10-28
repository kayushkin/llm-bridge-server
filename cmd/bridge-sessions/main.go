// Command bridge-sessions finds and reads past sessions on the llm-bridge-server
// that started the calling agent, read-only:
//
//	bridge-sessions search "<text>" [--limit N] [--harness codex]
//	bridge-sessions list [--limit N] [--harness codex]
//	bridge-sessions show <session id> [--last N]
//
// It signs in the way bridge-agent does: LLMBRIDGE_SERVICE_TOKEN when the
// environment holds one (an internal service), otherwise the session posting
// token every harness child is given (LLM_BRIDGE_SESSION_POSTING_TOKEN), which
// reads as the agent's own session's principal. The server is the one named
// by LLM_BRIDGE_SESSION_ACTIONS_URL, which the server sets in every harness
// child, unless --server says otherwise. Times print in the operator's zone.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	serviceTokenEnvironmentVariable = "LLMBRIDGE_SERVICE_TOKEN"
	serviceTokenHeader              = "X-LLM-Bridge-Service-Token"
	postingTokenEnvironmentVariable = "LLM_BRIDGE_SESSION_POSTING_TOKEN"
	actionsURLEnvironmentVariable   = "LLM_BRIDGE_SESSION_ACTIONS_URL"
	operatorTimeZone                = "America/Los_Angeles"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bridge-sessions:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New(`usage:
  bridge-sessions search "<text>" [--limit N] [--harness codex]
  bridge-sessions list [--limit N] [--harness codex]
  bridge-sessions show <session id> [--last N]`)
}

func run(arguments []string, out io.Writer) error {
	if len(arguments) == 0 {
		return usage()
	}
	command, rest := arguments[0], arguments[1:]
	flags := flag.NewFlagSet("bridge-sessions "+command, flag.ContinueOnError)
	server := flags.String("server", "", "llm-bridge-server base URL (default: the server named by "+actionsURLEnvironmentVariable+")")
	limit := flags.Int("limit", 20, "most sessions to print")
	harness := flags.String("harness", "", "only sessions of this harness (claude_code, codex, …)")
	last := flags.Int("last", 0, "show: only the last N turns (0: all)")
	positional, err := parseInterspersed(flags, rest)
	if err != nil {
		return err
	}
	client, err := newClient(*server)
	if err != nil {
		return err
	}
	zone, err := time.LoadLocation(operatorTimeZone)
	if err != nil {
		return fmt.Errorf("load %s: %w", operatorTimeZone, err)
	}
	switch command {
	case "search":
		if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
			return usage()
		}
		return search(client, out, zone, positional[0], *limit, *harness)
	case "list":
		if len(positional) != 0 {
			return usage()
		}
		return list(client, out, zone, *limit, *harness)
	case "show":
		if len(positional) != 1 {
			return usage()
		}
		return show(client, out, zone, positional[0], *last)
	default:
		return usage()
	}
}

// parseInterspersed lets flags follow the positional arguments, as in
// `bridge-sessions search "acme" --limit 5`.
func parseInterspersed(flags *flag.FlagSet, arguments []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(arguments); err != nil {
			return nil, err
		}
		arguments = flags.Args()
		if len(arguments) == 0 {
			return positional, nil
		}
		positional = append(positional, arguments[0])
		arguments = arguments[1:]
	}
}

type client struct {
	base         string
	serviceToken string
	postingToken string
	http         *http.Client
}

func newClient(server string) (*client, error) {
	base := strings.TrimRight(server, "/")
	if base == "" {
		actionsURL := os.Getenv(actionsURLEnvironmentVariable)
		if actionsURL == "" {
			return nil, fmt.Errorf("no server: pass --server, or run from a session llm-bridge-server started (it sets %s)", actionsURLEnvironmentVariable)
		}
		parsed, err := url.Parse(actionsURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("%s=%q is not a URL", actionsURLEnvironmentVariable, actionsURL)
		}
		base = parsed.Scheme + "://" + parsed.Host
	}
	c := &client{base: base, serviceToken: os.Getenv(serviceTokenEnvironmentVariable), postingToken: os.Getenv(postingTokenEnvironmentVariable), http: &http.Client{Timeout: 5 * time.Minute}}
	if c.serviceToken == "" && c.postingToken == "" {
		return nil, fmt.Errorf("no credential: run from a session llm-bridge-server started (it sets %s), or set %s", postingTokenEnvironmentVariable, serviceTokenEnvironmentVariable)
	}
	return c, nil
}

// get decodes a JSON answer into into, or returns the server's own error text.
func (c *client) get(path string, into any) error {
	request, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.serviceToken != "" {
		request.Header.Set(serviceTokenHeader, c.serviceToken)
	} else {
		request.Header.Set("Authorization", "Bearer "+c.postingToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

// session is the part of the server's session record this prints.
type session struct {
	SessionID   string `json:"session_id"`
	Harness     string `json:"harness"`
	DisplayName string `json:"display_name"`
	WorkingDir  string `json:"working_dir"`
	State       string `json:"state"`
	UpdatedAt   string `json:"updated_at"`
}

func printSession(out io.Writer, zone *time.Location, s session, matches int) {
	when := s.UpdatedAt
	if parsed, err := time.Parse(time.RFC3339Nano, s.UpdatedAt); err == nil {
		when = parsed.In(zone).Format("Jan 02 15:04")
	}
	title := strings.Join(strings.Fields(s.DisplayName), " ")
	if len([]rune(title)) > 90 {
		title = string([]rune(title)[:89]) + "…"
	}
	folder := s.WorkingDir
	if folder == "" {
		folder = "-"
	}
	line := fmt.Sprintf("%s  %-11s  %s  %-9s  %s  [%s]", s.SessionID, s.Harness, when, s.State, title, folder)
	if matches > 0 {
		line += fmt.Sprintf("  (%d matches)", matches)
	}
	fmt.Fprintln(out, line)
}

func list(c *client, out io.Writer, zone *time.Location, limit int, harness string) error {
	var sessions []session
	// The server returns the newest first; ask for more when filtering by
	// harness, so the limit applies after the filter.
	fetch := limit
	if harness != "" {
		fetch = limit * 10
	}
	if err := c.get(fmt.Sprintf("/sessions?limit=%d", fetch), &sessions); err != nil {
		return err
	}
	printed := 0
	for _, s := range sessions {
		if harness != "" && s.Harness != harness {
			continue
		}
		printSession(out, zone, s, 0)
		printed++
		if printed == limit {
			break
		}
	}
	if printed == 0 {
		fmt.Fprintln(out, "no sessions")
	}
	return nil
}

type searchHit struct {
	SessionID  string `json:"session_id"`
	MatchCount int    `json:"match_count"`
}

func search(c *client, out io.Writer, zone *time.Location, text string, limit int, harness string) error {
	var hits []searchHit
	fetch := limit
	if harness != "" {
		fetch = limit * 10
	}
	if err := c.get("/sessions/search?limit="+fmt.Sprint(fetch)+"&q="+url.QueryEscape(text), &hits); err != nil {
		return err
	}
	printed := 0
	for _, hit := range hits {
		var s session
		if err := c.get("/sessions/"+url.PathEscape(hit.SessionID), &s); err != nil {
			// log-store keeps sessions the bridge no longer has; say so rather
			// than drop the hit.
			fmt.Fprintf(out, "%s  (%d matches; the server has no session by this id: %v)\n", hit.SessionID, hit.MatchCount, err)
			continue
		}
		if harness != "" && s.Harness != harness {
			continue
		}
		printSession(out, zone, s, hit.MatchCount)
		printed++
		if printed == limit {
			break
		}
	}
	if len(hits) == 0 {
		fmt.Fprintln(out, "no sessions match")
	}
	return nil
}

// transcript is the part of GET /sessions/{id}/messages this prints.
type transcript struct {
	Model struct {
		Turns []struct {
			ID       string   `json:"id"`
			Role     string   `json:"role"`
			Ts       string   `json:"ts"`
			EntryIDs []string `json:"entryIds"`
		} `json:"turns"`
		Entries map[string]struct {
			Role     string `json:"role"`
			Kind     string `json:"kind"`
			Text     string `json:"text"`
			EventID  int64  `json:"eventId"`
			ToolName string `json:"toolName"`
		} `json:"entries"`
	} `json:"model"`
}

func show(c *client, out io.Writer, zone *time.Location, sessionID string, last int) error {
	var s session
	if err := c.get("/sessions/"+url.PathEscape(sessionID), &s); err != nil {
		return err
	}
	printSession(out, zone, s, 0)
	var t transcript
	if err := c.get("/sessions/"+url.PathEscape(sessionID)+"/messages?limit=1000", &t); err != nil {
		return err
	}
	turns := t.Model.Turns
	if last > 0 && len(turns) > last {
		turns = turns[len(turns)-last:]
	}
	for _, turn := range turns {
		entryIDs := append([]string(nil), turn.EntryIDs...)
		sort.SliceStable(entryIDs, func(i, j int) bool {
			return t.Model.Entries[entryIDs[i]].EventID < t.Model.Entries[entryIDs[j]].EventID
		})
		when := turn.Ts
		if parsed, err := time.Parse(time.RFC3339Nano, turn.Ts); err == nil {
			when = parsed.In(zone).Format("Jan 02 15:04")
		}
		fmt.Fprintf(out, "\n--- %s %s\n", turn.Role, when)
		for _, id := range entryIDs {
			entry := t.Model.Entries[id]
			switch entry.Kind {
			case "text", "result":
				if strings.TrimSpace(entry.Text) != "" {
					fmt.Fprintf(out, "%s: %s\n", entry.Role, entry.Text)
				}
			case "tool_call":
				name := entry.ToolName
				if name == "" {
					name = "tool"
				}
				fmt.Fprintf(out, "[%s call]\n", name)
			case "error":
				fmt.Fprintf(out, "[error] %s\n", entry.Text)
			}
		}
	}
	return nil
}
