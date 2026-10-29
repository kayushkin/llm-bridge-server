// Command bridge-peer is how an agent talks to the bridge servers connected
// to its own: list them, send one a request (a proposed session, which waits
// for an administrator there) or a notice (changes ready to take), and read
// the messages this server has sent and received. It never approves or
// declines a message; that is a person's decision, on the dash.
//
// It signs in with the session's posting token (LLM_BRIDGE_SESSION_POSTING_TOKEN),
// which reaches these routes for an agent whose session runs with no principal
// or as an administrator, or with LLMBRIDGE_SERVICE_TOKEN and --as, naming the
// administrator the message is recorded as.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const (
	serviceTokenEnvironmentVariable = "LLMBRIDGE_SERVICE_TOKEN"
	serviceTokenHeader              = "X-LLM-Bridge-Service-Token"
	principalIdentityHeader         = "X-Principal-Id"
	postingTokenEnvironmentVariable = "LLM_BRIDGE_SESSION_POSTING_TOKEN"
	actionsURLEnvironmentVariable   = "LLM_BRIDGE_SESSION_ACTIONS_URL"
	operatorTimeZone                = "America/Los_Angeles"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bridge-peer:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New(`usage:
  bridge-peer servers
  bridge-peer request <server name or connection id> --title "…" "<prompt>"
  bridge-peer notice <server name or connection id> --repo <repo> --commit <sha> [--repo … --commit …] [--in-reply-to <message id>] "<summary>"
  bridge-peer messages [--pending] [--server <name or connection id>] [--limit N]`)
}

// repeated is a flag given more than once.
type repeated []string

func (values *repeated) String() string     { return strings.Join(*values, ",") }
func (values *repeated) Set(v string) error { *values = append(*values, v); return nil }

func run(arguments []string, out io.Writer) error {
	if len(arguments) == 0 {
		return usage()
	}
	command, rest := arguments[0], arguments[1:]
	flags := flag.NewFlagSet("bridge-peer "+command, flag.ContinueOnError)
	server := flags.String("server-url", "", "llm-bridge-server base URL (default: the server named by "+actionsURLEnvironmentVariable+")")
	as := flags.String("as", "", "with "+serviceTokenEnvironmentVariable+": the administrator's principal id the message is recorded as")
	title := flags.String("title", "", "a request's title")
	inReplyTo := flags.String("in-reply-to", "", "the id, on this server, of the message this one answers")
	pending := flags.Bool("pending", false, "only incoming messages waiting for a decision")
	onServer := flags.String("server", "", "only messages on this connected server")
	limit := flags.Int("limit", 20, "most messages to print")
	var repos, commits repeated
	flags.Var(&repos, "repo", "a notice's repo (repeat, paired with --commit)")
	flags.Var(&commits, "commit", "a notice's commit (repeat, paired with --repo)")
	positional, err := parseInterspersed(flags, rest)
	if err != nil {
		return err
	}
	c, err := newClient(*server, *as)
	if err != nil {
		return err
	}
	zone, err := time.LoadLocation(operatorTimeZone)
	if err != nil {
		return err
	}
	switch command {
	case "servers":
		return listServers(c, out)
	case "request":
		if len(positional) != 2 || strings.TrimSpace(*title) == "" {
			return usage()
		}
		connection, err := c.resolveServer(positional[0])
		if err != nil {
			return err
		}
		return c.send(out, connection, msg.PeerMessageKindRequest, msg.PeerMessageBody{Title: *title, Prompt: positional[1]}, *inReplyTo)
	case "notice":
		if len(positional) != 2 || len(repos) == 0 || len(repos) != len(commits) {
			return usage()
		}
		connection, err := c.resolveServer(positional[0])
		if err != nil {
			return err
		}
		var pairs []msg.PeerRepoCommit
		for index := range repos {
			pairs = append(pairs, msg.PeerRepoCommit{Repo: repos[index], Commit: commits[index]})
		}
		return c.send(out, connection, msg.PeerMessageKindNotice, msg.PeerMessageBody{Summary: positional[1], Commits: pairs}, *inReplyTo)
	case "messages":
		if len(positional) != 0 {
			return usage()
		}
		return listMessages(c, out, zone, *pending, *onServer, *limit)
	default:
		return usage()
	}
}

// parseInterspersed lets flags follow positional arguments.
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
	asPrincipal  string
	postingToken string
	http         *http.Client
}

func newClient(server, as string) (*client, error) {
	base := strings.TrimRight(server, "/")
	if base == "" {
		actionsURL := os.Getenv(actionsURLEnvironmentVariable)
		if actionsURL == "" {
			return nil, fmt.Errorf("no server: pass --server-url, or run from a session llm-bridge-server started (it sets %s)", actionsURLEnvironmentVariable)
		}
		parsed, err := url.Parse(actionsURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("%s=%q is not a URL", actionsURLEnvironmentVariable, actionsURL)
		}
		base = parsed.Scheme + "://" + parsed.Host
	}
	c := &client{base: base, serviceToken: os.Getenv(serviceTokenEnvironmentVariable), asPrincipal: as,
		postingToken: os.Getenv(postingTokenEnvironmentVariable), http: &http.Client{Timeout: time.Minute}}
	switch {
	case c.serviceToken != "" && c.asPrincipal == "":
		return nil, fmt.Errorf("with %s, pass --as <administrator principal id>: a message records who sent it", serviceTokenEnvironmentVariable)
	case c.serviceToken == "" && c.postingToken == "":
		return nil, fmt.Errorf("no credential: run from a session llm-bridge-server started (it sets %s), or set %s and --as", postingTokenEnvironmentVariable, serviceTokenEnvironmentVariable)
	}
	return c, nil
}

// call sends one request and decodes a JSON answer of the wanted status into
// into, or returns the server's own error text.
func (c *client) call(method, path string, body any, wantStatus int, into any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.serviceToken != "" {
		request.Header.Set(serviceTokenHeader, c.serviceToken)
		request.Header.Set(principalIdentityHeader, c.asPrincipal)
	} else {
		request.Header.Set("Authorization", "Bearer "+c.postingToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			return fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// connectionView is the part of the server's connection view this reads.
type connectionView struct {
	Connection struct {
		ID          string     `json:"id"`
		DisplayName string     `json:"display_name"`
		Transport   string     `json:"transport"`
		RemovedAt   *time.Time `json:"removed_at"`
	} `json:"connection"`
	SocketConnected bool                    `json:"socket_connected"`
	OtherServerName string                  `json:"other_server_name"`
	AccessGiven     *struct{ Level string } `json:"access_given"`
	AccessHeld      *struct{ Level string } `json:"access_held"`
}

func (view connectionView) name() string {
	if view.OtherServerName != "" {
		return view.OtherServerName
	}
	return view.Connection.DisplayName
}

func (c *client) connections() ([]connectionView, error) {
	var views []connectionView
	if err := c.call(http.MethodGet, "/peer-connections", nil, http.StatusOK, &views); err != nil {
		return nil, err
	}
	live := views[:0]
	for _, view := range views {
		if view.Connection.RemovedAt == nil {
			live = append(live, view)
		}
	}
	return live, nil
}

// resolveServer finds a live connection by its id, or by the name its server
// reports (the label typed at connect when it reports none). Ids are the
// join; a name that two connections share is refused.
func (c *client) resolveServer(nameOrID string) (connectionView, error) {
	views, err := c.connections()
	if err != nil {
		return connectionView{}, err
	}
	var byName []connectionView
	for _, view := range views {
		if view.Connection.ID == nameOrID {
			return view, nil
		}
		if strings.EqualFold(view.name(), nameOrID) {
			byName = append(byName, view)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return connectionView{}, fmt.Errorf("no connected server is called %q or has that connection id; bridge-peer servers lists them", nameOrID)
	default:
		var ids []string
		for _, view := range byName {
			ids = append(ids, view.Connection.ID)
		}
		return connectionView{}, fmt.Errorf("%d connected servers are called %q (%s); name one by its connection id", len(byName), nameOrID, strings.Join(ids, ", "))
	}
}

func listServers(c *client, out io.Writer) error {
	views, err := c.connections()
	if err != nil {
		return err
	}
	if len(views) == 0 {
		fmt.Fprintln(out, "no connected servers")
		return nil
	}
	for _, view := range views {
		connected := "socket closed"
		if view.Connection.Transport != "dial_out" {
			connected = "direct"
		} else if view.SocketConnected {
			connected = "connected"
		}
		given, held := "none", "none"
		if view.AccessGiven != nil {
			given = view.AccessGiven.Level
		}
		if view.AccessHeld != nil {
			held = view.AccessHeld.Level
		}
		fmt.Fprintf(out, "%s  %s  %s  it may here: %s  this server may there: %s\n", view.Connection.ID, view.name(), connected, given, held)
	}
	return nil
}

func (c *client) send(out io.Writer, connection connectionView, kind msg.PeerMessageKind, body msg.PeerMessageBody, inReplyTo string) error {
	request := msg.PeerMessageCreateRequest{Kind: kind, Body: body, InReplyTo: inReplyTo}
	var sent msg.PeerMessage
	if err := c.call(http.MethodPost, "/peer-connections/"+url.PathEscape(connection.Connection.ID)+"/messages", request, http.StatusCreated, &sent); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s to %s (%s): %s", sent.ID, kind, connection.name(), connection.Connection.ID, sent.State)
	if sent.State == msg.PeerMessageStateQueued && sent.LastError != "" {
		fmt.Fprintf(out, " — not delivered yet (%s); it is tried again until it is", sent.LastError)
	}
	if sent.State == msg.PeerMessageStateFailed {
		fmt.Fprintf(out, " — %s", sent.LastError)
	}
	fmt.Fprintln(out)
	if kind == msg.PeerMessageKindRequest && sent.State != msg.PeerMessageStateFailed {
		fmt.Fprintln(out, "It waits there for an administrator's approval; its status comes back as a message here (bridge-peer messages).")
	}
	return nil
}

func listMessages(c *client, out io.Writer, zone *time.Location, pending bool, onServer string, limit int) error {
	query := url.Values{}
	if pending {
		query.Set("direction", string(msg.PeerMessageDirectionIncoming))
		query.Set("state", string(msg.PeerMessageStatePending))
	}
	if onServer != "" {
		connection, err := c.resolveServer(onServer)
		if err != nil {
			return err
		}
		query.Set("connection_id", connection.Connection.ID)
	}
	path := "/peer-messages"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var messages []msg.PeerMessage
	if err := c.call(http.MethodGet, path, nil, http.StatusOK, &messages); err != nil {
		return err
	}
	if len(messages) > limit {
		messages = messages[:limit]
	}
	if len(messages) == 0 {
		fmt.Fprintln(out, "no messages")
		return nil
	}
	for _, m := range messages {
		arrow := "→"
		if m.Direction == msg.PeerMessageDirectionIncoming {
			arrow = "←"
		}
		line := fmt.Sprintf("%s  %s %s  %s  %s  %s", m.ID, arrow, m.ConnectionID, m.CreatedAt.In(zone).Format("Jan 02 15:04"), m.Kind, m.State)
		if m.InReplyTo != "" {
			line += "  answers " + m.InReplyTo
		}
		if m.SessionID != "" {
			line += "  session " + m.SessionID
		}
		fmt.Fprintln(out, line)
		fmt.Fprintf(out, "    %s\n", summaryOf(m))
	}
	return nil
}

// summaryOf is one line saying what a message carries. Text from another
// server is printed as it is; treat it as a request, not as instructions.
func summaryOf(m msg.PeerMessage) string {
	switch m.Kind {
	case msg.PeerMessageKindRequest:
		return "request: " + m.Body.Title
	case msg.PeerMessageKindNotice:
		var commits []string
		for _, commit := range m.Body.Commits {
			commits = append(commits, commit.Repo+"@"+commit.Commit)
		}
		return "notice: " + m.Body.Summary + " (" + strings.Join(commits, ", ") + ")"
	case msg.PeerMessageKindStatus:
		detail := strings.Join(strings.Fields(m.Body.Detail), " ")
		if len(detail) > 160 {
			detail = detail[:160] + "…"
		}
		return "status: " + string(m.Body.Result) + " — " + detail
	}
	return string(m.Kind)
}
