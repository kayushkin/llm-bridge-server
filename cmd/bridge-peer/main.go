// Command bridge-peer is how an agent talks to the bridge servers connected
// to its own: list them, send one a request (a proposed session, which waits
// for an administrator there), a notice (changes ready to take) or a patch (a
// change to one repo, as git format-patch files), attach files to a message,
// download a message's files, and read the messages this server has sent and
// received. It also lists another server's instances and runs a session
// there (bridge-peer run), new or continued, printing the reply; that goes
// through the connection proxy with the session agent token
// (LLM_BRIDGE_PRINCIPAL_TOKEN), since the proxy refuses the posting token.
// Files travel only as attachments: the receiving server fetches
// their bytes over the connection and keeps them only when their SHA-256
// matches, so nothing has to be pasted into a session as text. It never approves or
// declines a message; that is a person's decision, on the dash. On a server
// with an orchestrator, an approved request or patch is handed to the
// orchestrator's session, whose agent chooses where it runs with
// bridge-peer place.
//
// It signs in with the session's posting token (LLM_BRIDGE_SESSION_POSTING_TOKEN),
// which reaches these routes for an agent whose session runs with no principal
// or as an administrator, or with LLMBRIDGE_SERVICE_TOKEN and --as, naming the
// administrator the message is recorded as.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
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
  bridge-peer request <server name or connection id> --title "…" [--supersedes <message id>]… [--attach <file>]… "<prompt>"
  bridge-peer notice <server name or connection id> --repo <repo> --commit <sha> [--repo … --commit …] [--in-reply-to <message id>] [--supersedes <message id>]… [--attach <file>]… "<summary>"
  bridge-peer status <server name or connection id> --in-reply-to <message id> --result succeeded|failed|declined [--attach <file>]… "<detail>"
  bridge-peer patch <server name or connection id> --repo <repo name> [--base origin/main] [--summary "…"] [--supersedes <message id>]…
      (run inside the repo: it sends git format-patch <base>..HEAD as one attached file)
  bridge-peer fetch <message id> [--out <directory>]
  bridge-peer messages [--pending | --open] [--server <name or connection id>] [--limit N]
      (--open: this server's requests, notices and patches still waiting for an answer;
       request, notice and patch print the open ones to that server before sending,
       and --supersedes <id> says the new message replaces one, which the other server
       then declines if it is still waiting there)
  bridge-peer instances <server name or connection id>
  bridge-peer place <message id> --instance <instance id> --reason "<why that instance>"
      (the orchestrator's session starts an approved request or patch handed to it, as its own chat
       on an instance of this server; a patch only on an instance of this server's own machine)
  bridge-peer run <server name or connection id> (--instance <its instance id> | --session <its session id>) [--timeout 15m] "<message>"
      (starts a session there, or continues one, waits for the reply and prints it; the session stays there)`)
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
	open := flags.Bool("open", false, "only this server's requests, notices and patches still waiting for the other server")
	onServer := flags.String("server", "", "only messages on this connected server")
	limit := flags.Int("limit", 20, "most messages to print")
	base := flags.String("base", "origin/main", "a patch's base: the commit the other server already has")
	summary := flags.String("summary", "", "a patch's summary (default: its commit subjects)")
	outDirectory := flags.String("out", ".", "where fetch writes a message's files")
	result := flags.String("result", "", "a status's result: succeeded, failed or declined")
	instance := flags.String("instance", "", "run: the other server's instance to start a new session on; place: this server's instance the message's session starts on")
	reason := flags.String("reason", "", "place: why that instance")
	session := flags.String("session", "", "run: the other server's session to continue")
	timeout := flags.Duration("timeout", 15*time.Minute, "run: give up waiting for the reply after this long")
	var repos, commits, attachments, supersedes repeated
	flags.Var(&supersedes, "supersedes", "the id, on this server, of an earlier request, notice or patch this one replaces (repeat)")
	flags.Var(&attachments, "attach", "a file to send with the message (repeat)")
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
		attachmentIDs, err := c.uploadAll(out, attachments)
		if err != nil {
			return err
		}
		return c.send(out, connection, msg.PeerMessageKindRequest, msg.PeerMessageBody{Title: *title, Prompt: positional[1]}, *inReplyTo, attachmentIDs, supersedes)
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
		attachmentIDs, err := c.uploadAll(out, attachments)
		if err != nil {
			return err
		}
		return c.send(out, connection, msg.PeerMessageKindNotice, msg.PeerMessageBody{Summary: positional[1], Commits: pairs}, *inReplyTo, attachmentIDs, supersedes)
	case "status":
		if len(positional) != 2 || *inReplyTo == "" || *result == "" {
			return usage()
		}
		connection, err := c.resolveServer(positional[0])
		if err != nil {
			return err
		}
		attachmentIDs, err := c.uploadAll(out, attachments)
		if err != nil {
			return err
		}
		return c.send(out, connection, msg.PeerMessageKindStatus, msg.PeerMessageBody{Result: msg.PeerMessageStatusResult(*result), Detail: positional[1]}, *inReplyTo, attachmentIDs, nil)
	case "patch":
		if len(positional) != 1 || len(repos) != 1 || len(commits) != 0 {
			return usage()
		}
		connection, err := c.resolveServer(positional[0])
		if err != nil {
			return err
		}
		return c.sendPatch(out, connection, repos[0], *base, *summary, *inReplyTo, supersedes)
	case "fetch":
		if len(positional) != 1 {
			return usage()
		}
		return c.fetch(out, positional[0], *outDirectory)
	case "place":
		if len(positional) != 1 || *instance == "" || strings.TrimSpace(*reason) == "" {
			return usage()
		}
		var placed msg.PeerMessage
		if err := c.call(http.MethodPost, "/peer-messages/"+url.PathEscape(positional[0])+"/place",
			msg.PeerMessagePlaceRequest{InstanceID: *instance, Reason: *reason}, http.StatusOK, &placed); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s placed on %s: session %s\n", placed.ID, *instance, placed.SessionID)
		return nil
	case "messages":
		if len(positional) != 0 || (*pending && *open) {
			return usage()
		}
		return listMessages(c, out, zone, *pending, *open, *onServer, *limit)
	case "instances":
		if len(positional) != 1 {
			return usage()
		}
		connection, err := c.resolveServer(positional[0])
		if err != nil {
			return err
		}
		return listInstances(context.Background(), c, out, connection)
	case "run":
		if len(positional) != 2 || (*instance == "") == (*session == "") {
			return usage()
		}
		connection, err := c.resolveServer(positional[0])
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
		defer cancelTimeout()
		return runOnServer(ctx, c, out, os.Stderr, connection, *instance, *session, positional[1])
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
	// principalToken is the session agent token, which the connection proxy
	// takes and the posting token is not.
	principalToken string
	http           *http.Client
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
		postingToken: os.Getenv(postingTokenEnvironmentVariable), principalToken: principalTokenOfThisSession(), http: &http.Client{Timeout: time.Minute}}
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
	c.authorize(request)
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

func (c *client) send(out io.Writer, connection connectionView, kind msg.PeerMessageKind, body msg.PeerMessageBody, inReplyTo string, attachmentIDs, supersedes []string) error {
	if kind != msg.PeerMessageKindStatus {
		if err := c.printOpenMessages(out, connection, supersedes); err != nil {
			return err
		}
	}
	request := msg.PeerMessageCreateRequest{Kind: kind, Body: body, InReplyTo: inReplyTo, AttachmentIDs: attachmentIDs, Supersedes: supersedes}
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
	if len(sent.Supersedes) > 0 {
		fmt.Fprintf(out, "It replaces %s; %s declines any of those still waiting there.\n", strings.Join(sent.Supersedes, ", "), connection.name())
	}
	for _, attachment := range sent.Attachments {
		fmt.Fprintf(out, "  %s %s (%d bytes, sha256 %s)\n", attachment.ID, attachment.Name, attachment.Size, attachment.SHA256)
	}
	if (kind == msg.PeerMessageKindRequest || kind == msg.PeerMessageKindPatch) && sent.State != msg.PeerMessageStateFailed {
		fmt.Fprintln(out, "It waits there for an administrator's approval; its status comes back as a message here (bridge-peer messages).")
	}
	return nil
}

// printOpenMessages shows what this server already asked of the other one
// and has had no answer to, before a new request, notice or patch goes, so
// the sender can tell a duplicate. It sends anyway: the sender decides.
func (c *client) printOpenMessages(out io.Writer, connection connectionView, supersedes []string) error {
	var open []msg.PeerMessage
	if err := c.call(http.MethodGet, "/peer-messages?open=true&connection_id="+url.QueryEscape(connection.Connection.ID), nil, http.StatusOK, &open); err != nil {
		return fmt.Errorf("read the messages still open on %s: %w", connection.name(), err)
	}
	var notReplaced []msg.PeerMessage
	for _, message := range open {
		if !slicesContain(supersedes, message.ID) {
			notReplaced = append(notReplaced, message)
		}
	}
	if len(notReplaced) == 0 {
		return nil
	}
	fmt.Fprintf(out, "Still open on %s, with no answer yet (sending this one too):\n", connection.name())
	for _, message := range notReplaced {
		fmt.Fprintf(out, "  %s  %s  %s  %s\n", message.ID, message.CreatedAt.Local().Format("Jan 02 15:04"), message.State, summaryOf(message))
	}
	fmt.Fprintln(out, "If this message replaces one of them, send it with --supersedes <id> instead, so the other server declines the old one.")
	return nil
}

func slicesContain(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func listMessages(c *client, out io.Writer, zone *time.Location, pending, open bool, onServer string, limit int) error {
	query := url.Values{}
	if open {
		query.Set("open", "true")
	}
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
		if len(m.Supersedes) > 0 {
			line += "  replaces " + strings.Join(m.Supersedes, ", ")
		}
		if m.SupersededBy != "" {
			line += "  replaced by " + m.SupersededBy
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
	case msg.PeerMessageKindPatch:
		return "patch: " + m.Body.Repo + " — " + strings.Join(strings.Fields(m.Body.Summary), " ")
	case msg.PeerMessageKindStatus:
		detail := strings.Join(strings.Fields(m.Body.Detail), " ")
		if len(detail) > 160 {
			detail = detail[:160] + "…"
		}
		return "status: " + string(m.Body.Result) + " — " + detail
	}
	return string(m.Kind)
}

// uploadAll stores each file on this server for the next message to send, and
// returns their ids in the order given. A file that cannot be read or is
// refused stops the send before any message goes out.
func (c *client) uploadAll(out io.Writer, paths []string) ([]string, error) {
	var ids []string
	for _, path := range paths {
		attachment, err := c.upload(path, filepath.Base(path))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "uploaded %s as %s (%d bytes, sha256 %s)\n", path, attachment.ID, attachment.Size, attachment.SHA256)
		ids = append(ids, attachment.ID)
	}
	return ids, nil
}

// upload sends one file's bytes to POST /peer-message-attachments.
func (c *client) upload(path, name string) (msg.PeerMessageAttachment, error) {
	file, err := os.Open(path)
	if err != nil {
		return msg.PeerMessageAttachment{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return msg.PeerMessageAttachment{}, err
	}
	query := url.Values{"name": {name}}
	request, err := http.NewRequest(http.MethodPost, c.base+"/peer-message-attachments?"+query.Encode(), file)
	if err != nil {
		return msg.PeerMessageAttachment{}, err
	}
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", "application/octet-stream")
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return msg.PeerMessageAttachment{}, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return msg.PeerMessageAttachment{}, err
	}
	if response.StatusCode != http.StatusCreated {
		return msg.PeerMessageAttachment{}, fmt.Errorf("upload %s: %d %s", path, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	var attachment msg.PeerMessageAttachment
	if err := json.Unmarshal(answer, &attachment); err != nil {
		return msg.PeerMessageAttachment{}, fmt.Errorf("upload %s: decode: %w", path, err)
	}
	return attachment, nil
}

// gitOutput runs git in the current directory and returns its standard output.
func gitOutput(arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

// sendPatch sends this repo's commits after base as one git format-patch
// file. It refuses uncommitted changes, which the patch would leave out
// without saying so, and a base with no commits after it.
func (c *client) sendPatch(out io.Writer, connection connectionView, repo, base, summary, inReplyTo string, supersedes []string) error {
	if status, err := gitOutput("status", "--porcelain"); err != nil {
		return err
	} else if strings.TrimSpace(status) != "" {
		return fmt.Errorf("this repo has uncommitted changes, which a patch would leave out; commit them first:\n%s", status)
	}
	baseCommit, err := gitOutput("rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return err
	}
	baseCommit = strings.TrimSpace(baseCommit)
	subjects, err := gitOutput("log", "--reverse", "--format=%s", baseCommit+"..HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(subjects) == "" {
		return fmt.Errorf("HEAD has no commits after %s (%s): nothing to send", base, baseCommit)
	}
	mbox, err := gitOutput("format-patch", "--stdout", baseCommit+"..HEAD")
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "bridge-peer-patch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, repo+".patch")
	if err := os.WriteFile(path, []byte(mbox), 0o600); err != nil {
		return err
	}
	if strings.TrimSpace(summary) == "" {
		summary = strings.TrimSpace(subjects)
	}
	attachmentIDs, err := c.uploadAll(out, []string{path})
	if err != nil {
		return err
	}
	return c.send(out, connection, msg.PeerMessageKindPatch, msg.PeerMessageBody{Repo: repo, BaseCommit: baseCommit, Summary: summary}, inReplyTo, attachmentIDs, supersedes)
}

// fetch downloads every file of a message into directory and checks each
// against the SHA-256 the server recorded, removing a file that does not match.
func (c *client) fetch(out io.Writer, messageID, directory string) error {
	var message msg.PeerMessage
	if err := c.call(http.MethodGet, "/peer-messages/"+url.PathEscape(messageID), nil, http.StatusOK, &message); err != nil {
		return err
	}
	if len(message.Attachments) == 0 {
		return fmt.Errorf("%s carries no files", messageID)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	for _, attachment := range message.Attachments {
		target := filepath.Join(directory, filepath.Base(attachment.Name))
		if err := c.download(message.ID, attachment, target); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s -> %s (%d bytes, sha256 %s)\n", attachment.ID, target, attachment.Size, attachment.SHA256)
	}
	return nil
}

func (c *client) download(messageID string, attachment msg.PeerMessageAttachment, target string) error {
	request, err := http.NewRequest(http.MethodGet, c.base+"/peer-messages/"+url.PathEscape(messageID)+"/attachments/"+url.PathEscape(attachment.ID), nil)
	if err != nil {
		return err
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("download %s: %d %s", attachment.ID, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("download %s: %w (an existing file is never overwritten)", attachment.ID, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), response.Body)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(target)
		return fmt.Errorf("download %s: %v %v", attachment.ID, copyErr, closeErr)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != attachment.SHA256 || written != attachment.Size {
		os.Remove(target)
		return fmt.Errorf("download %s: got %d bytes with sha256 %s, want %d bytes with sha256 %s; removed it", attachment.ID, written, got, attachment.Size, attachment.SHA256)
	}
	return nil
}

// authorize puts this client's credential on a request.
func (c *client) authorize(request *http.Request) {
	if c.serviceToken != "" {
		request.Header.Set(serviceTokenHeader, c.serviceToken)
		request.Header.Set(principalIdentityHeader, c.asPrincipal)
		return
	}
	request.Header.Set("Authorization", "Bearer "+c.postingToken)
}
