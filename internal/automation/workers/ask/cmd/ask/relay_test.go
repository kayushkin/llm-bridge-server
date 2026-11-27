// Tests for cmd/ask's relay: runAsk, resolveInstance and buildPrompt.
//
// Ported from 1f734c29 (branch test/first-tests-for-ask, 2026-08-08) onto main,
// where main() had since gained the --session reuse path, the settings registry
// and bridge auth. The display-name and --from tests live in their own files.
//
// Two rules govern the assertions here, both learned by scoring earlier suites
// against deliberate sabotage rather than by reading them:
//
//   - Never reproduce a gate in the fixture. Where a value is a contract with
//     another program — the bus subject, the session Purpose, the herald- id
//     prefix — the assertion spells the wire literal out. Comparing the code's
//     constant against itself is green through any drift of that constant.
//
//   - Never assert a bare error. runAsk has nine failure sites and they all
//     return a non-nil error, so `err != nil` pins the signature and not the
//     behavior. Each failure test also asserts the discriminating side effect:
//     the instance endpoint that must not be reached, the connection that must
//     not be opened, the message that must not be published.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgestores"
)

// ---------------------------------------------------------------- fixtures

// recordingPublisher stands in for the NATS client and records what the binary
// tried to send, so the dispatch decision is observable.
type recordingPublisher struct {
	published []publishedMessage
	closed    int
	failWith  error
}

type publishedMessage struct {
	subject string
	data    any
}

func (p *recordingPublisher) Publish(subject string, data any) error {
	if p.failWith != nil {
		return p.failWith
	}
	p.published = append(p.published, publishedMessage{subject: subject, data: data})
	return nil
}

func (p *recordingPublisher) Close() { p.closed++ }

// connector counts how many times the binary asked for a connection, which is
// what makes "a dry run opens no connection" assertable.
type connector struct {
	pub      *recordingPublisher
	calls    int
	failWith error
}

func (c *connector) connect() (heraldPublisher, error) {
	c.calls++
	if c.failWith != nil {
		return nil, c.failWith
	}
	return c.pub, nil
}

func newConnector() *connector { return &connector{pub: &recordingPublisher{}} }

// instanceServer serves a /instances payload and counts the requests it got.
type instanceServer struct {
	*httptest.Server
	requests int
}

func newInstanceServer(t *testing.T, status int, body string) *instanceServer {
	t.Helper()
	s := &instanceServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests++
		if r.URL.Path != "/instances" {
			t.Errorf("ask asked for %q, want /instances", r.URL.Path)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// oneLocalClaudeCode is the payload of a correctly configured box: exactly one
// enabled claude_code instance on the local machine.
const oneLocalClaudeCode = `[
  {"id":"inst-1","name":"box","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}}
]`

func baseConfig(bridgeURL string) askConfig {
	return askConfig{
		from:      "approval-proposer",
		bridgeURL: bridgeURL,
		question:  "Approve the update?",
		now:       time.Date(2026, 8, 8, 12, 0, 0, 123456789, time.UTC),
		heraldModelRole: func() (string, string, error) {
			return "default", "claude-test", nil
		},
	}
}

// decodeInbound pulls the one published envelope back out.
func decodeInbound(t *testing.T, m publishedMessage) messages.BridgeInbound {
	t.Helper()
	in, ok := m.data.(messages.BridgeInbound)
	if !ok {
		t.Fatalf("published %T, want messages.BridgeInbound", m.data)
	}
	return in
}

// ------------------------------------------------- the published envelope

func TestRunAsk_PublishesOneMessageOnTheBridgeInboundSubject(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()

	if _, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard); err != nil {
		t.Fatalf("runAsk: %v", err)
	}

	if got := len(c.pub.published); got != 1 {
		t.Fatalf("published %d messages, want exactly 1", got)
	}
	// The wire literal, not messages.BridgeInboundSubject: the constant compared
	// against itself cannot see the subject drift away from what the adapter
	// subscribes to.
	if got := c.pub.published[0].subject; got != "bridge.inbound" {
		t.Errorf("published on subject %q, want %q", got, "bridge.inbound")
	}
	if c.pub.closed != 1 {
		t.Errorf("closed the connection %d times, want 1", c.pub.closed)
	}
}

func TestRunAsk_EnvelopeCarriesTheValuesLLMBridgeServerReadsBack(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()

	if _, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard); err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	in := decodeInbound(t, c.pub.published[0])

	// Every one of these is a contract with a program in another repo, so each
	// is spelled as the literal that program matches on.
	checks := []struct {
		field string
		got   string
		want  string
	}{
		{"Channel", in.Channel, "herald"},
		{"Orchestrator", in.Orchestrator, "claude-code"},
		{"Agent", in.Agent, "approval-proposer"},
		{"Create.Purpose", in.Create.Purpose, "herald"},
		{"Create.Origin", in.Create.Origin, "ask"},
		{"Create.Type", string(in.Create.Type), "herald"},
		{"Create.Harness", string(in.Create.Harness), "claude_code"},
		{"Create.InstanceID", in.Create.InstanceID, "inst-1"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if !in.Create.AutoStart {
		t.Error("Create.AutoStart = false; the relay turn would never start")
	}
	if !in.Timestamp.Equal(baseConfig(srv.URL).now) {
		t.Errorf("Timestamp = %v, want the run's clock %v", in.Timestamp, baseConfig(srv.URL).now)
	}
}

// The session Type is what trips isUnattendedSession in llm-bridge-server. If it
// drifts, every tool call in a relay turn parks on the permission prehook with
// nobody to answer it, and the turn wedges — silently, and only in production.
func TestRunAsk_SessionTypeIsTheOneLLMBridgeServerTreatsAsUnattended(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()

	if _, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard); err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if got := string(decodeInbound(t, c.pub.published[0]).Create.Type); got != "herald" {
		t.Errorf("session Type = %q, want %q", got, "herald")
	}
}

func TestRunAsk_SessionIDCarriesTheHeraldPrefixAndTheRunsClock(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := baseConfig(srv.URL)

	id, err := runAsk(cfg, c.connect, io.Discard)
	if err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	want := fmt.Sprintf("herald-%d", cfg.now.UnixNano())
	if id != want {
		t.Errorf("session id = %q, want %q", id, want)
	}
	if got := decodeInbound(t, c.pub.published[0]).BusSessionID; got != want {
		t.Errorf("BusSessionID = %q, want the returned id %q", got, want)
	}
}

// kanban-curator's idle-reaper is scoped to the autoworker- prefix, and ask's
// design notes lean on that to keep relay sessions out of it. A herald session
// waiting on a human looks exactly like an idle one, so if the prefixes ever
// converge the reaper closes the question the user has not answered yet.
func TestRunAsk_SessionIDIsNotReapableByTheAutoworkerIdleReaper(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()

	id, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard)
	if err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if strings.HasPrefix(id, "autoworker-") {
		t.Errorf("session id %q carries the autoworker- prefix; the curator's idle-reaper would close it", id)
	}
}

func TestRunAsk_DisplayNameDefaultsToTheQuestionAndTitleOverridesIt(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)

	c := newConnector()
	cfg := baseConfig(srv.URL)
	if _, err := runAsk(cfg, c.connect, io.Discard); err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if got := decodeInbound(t, c.pub.published[0]).Create.DisplayName; got != "Approve the update?" {
		t.Errorf("default DisplayName = %q, want the question", got)
	}

	c2 := newConnector()
	cfg2 := baseConfig(srv.URL)
	cfg2.title = "Approval needed"
	if _, err := runAsk(cfg2, c2.connect, io.Discard); err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if got := decodeInbound(t, c2.pub.published[0]).Create.DisplayName; got != "Approval needed" {
		t.Errorf("DisplayName = %q, want the --title value", got)
	}
}

// The default the flag carries is declared once at package level, and the test
// drives that declaration. A test that passed "herald" in from its own fixture
// would agree with itself while the flag default drifted to something else.
func TestRunAsk_DefaultFromTagIsTheOneTheFlagCarries(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := baseConfig(srv.URL)
	cfg.from = defaultFromTag

	if _, err := runAsk(cfg, c.connect, io.Discard); err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if got := decodeInbound(t, c.pub.published[0]).Agent; got != "herald" {
		t.Errorf("Agent tag from the flag default = %q, want %q", got, "herald")
	}
}

// -------------------------------------------------------------- dry run

func TestRunAsk_DryRunPublishesNothingAndOpensNoConnection(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := baseConfig(srv.URL)
	cfg.dryRun = true

	var out strings.Builder
	id, err := runAsk(cfg, c.connect, &out)
	if err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if id != "" {
		t.Errorf("dry run returned session id %q, want empty — no session was minted", id)
	}
	// The discriminating side effects. Counting published messages alone would
	// stay green if the connection were opened and left dangling.
	if c.calls != 0 {
		t.Errorf("dry run opened %d connections, want 0", c.calls)
	}
	if got := len(c.pub.published); got != 0 {
		t.Errorf("dry run published %d messages, want 0", got)
	}
	if srv.requests != 1 {
		t.Errorf("dry run made %d instance lookups, want 1 — it still resolves the host", srv.requests)
	}
	report := out.String()
	for _, want := range []string{"inst-1", "approval-proposer", "herald-", "Approve the update?"} {
		if !strings.Contains(report, want) {
			t.Errorf("dry-run report is missing %q:\n%s", want, report)
		}
	}
}

// ------------------------------------------------------ refusing to send

func TestRunAsk_ABlankQuestionIsRefusedBeforeAnythingIsResolved(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := baseConfig(srv.URL)
	cfg.question = ""

	_, err := runAsk(cfg, c.connect, io.Discard)
	if err == nil {
		t.Fatal("runAsk accepted a blank question")
	}
	if !strings.Contains(err.Error(), "question") {
		t.Errorf("error %q does not say which input was missing", err)
	}
	// runAsk has nine failure sites returning a non-nil error. These are what
	// say the run stopped at THIS one.
	if srv.requests != 0 {
		t.Errorf("resolved an instance (%d requests) despite having no question", srv.requests)
	}
	if c.calls != 0 {
		t.Errorf("opened %d connections despite having no question", c.calls)
	}
}

func TestRunAsk_ABlankFromTagIsRefusedBeforeAnythingIsResolved(t *testing.T) {
	for _, from := range []string{"", "   ", "\t\n"} {
		srv := newInstanceServer(t, 200, oneLocalClaudeCode)
		c := newConnector()
		cfg := baseConfig(srv.URL)
		cfg.from = from

		_, err := runAsk(cfg, c.connect, io.Discard)
		if err == nil {
			t.Fatalf("runAsk accepted --from %q", from)
		}
		if !strings.Contains(err.Error(), "from") {
			t.Errorf("error %q does not name the --from flag", err)
		}
		if srv.requests != 0 {
			t.Errorf("from=%q: resolved an instance (%d requests) anyway", from, srv.requests)
		}
		if c.calls != 0 {
			t.Errorf("from=%q: opened %d connections anyway", from, c.calls)
		}
	}
}

func TestRunAsk_AnUnresolvableInstanceStopsTheRunBeforeItConnects(t *testing.T) {
	srv := newInstanceServer(t, 200, `[]`)
	c := newConnector()

	_, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard)
	if err == nil {
		t.Fatal("runAsk published with no instance to host the session")
	}
	if !strings.Contains(err.Error(), "resolve instance") {
		t.Errorf("error %q does not say the instance lookup was what failed", err)
	}
	if c.calls != 0 {
		t.Errorf("opened %d connections with no instance resolved, want 0", c.calls)
	}
}

func TestRunAsk_AFailedConnectionStopsTheRunAndPublishesNothing(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	c.failWith = fmt.Errorf("no route to host")

	id, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard)
	if err == nil {
		t.Fatal("runAsk reported success with no connection")
	}
	if !strings.Contains(err.Error(), "nats connect") {
		t.Errorf("error %q does not say the connection was what failed", err)
	}
	if id != "" {
		t.Errorf("returned session id %q for a session that was never published", id)
	}
	if got := len(c.pub.published); got != 0 {
		t.Errorf("published %d messages without a connection", got)
	}
}

// A refused publish must not report a session id. The id is what a caller
// records and later looks up; handing one back for a session that does not
// exist turns a loud failure into a dangling reference.
func TestRunAsk_ARefusedPublishReturnsNoSessionID(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	c.pub.failWith = fmt.Errorf("jetstream unavailable")

	id, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard)
	if err == nil {
		t.Fatal("runAsk reported success on a refused publish")
	}
	if !strings.Contains(err.Error(), "publish") {
		t.Errorf("error %q does not say the publish was what failed", err)
	}
	if id != "" {
		t.Errorf("returned session id %q after the publish was refused", id)
	}
	if c.pub.closed != 1 {
		t.Errorf("closed the connection %d times on the failure path, want 1", c.pub.closed)
	}
}

// ------------------------------------------------------ resolveInstance

func TestResolveInstance_ByNameWhenTheNameIsUnique(t *testing.T) {
	srv := newInstanceServer(t, 200, `[
	  {"id":"inst-a","name":"alpha","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}},
	  {"id":"inst-b","name":"beta","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}}
	]`)

	got, err := resolveInstance(srv.URL, "beta")
	if err != nil {
		t.Fatalf("resolveInstance: %v", err)
	}
	if got != "inst-b" {
		t.Errorf("resolved %q, want inst-b", got)
	}
}

// A name is not an id. Two instances can share one, and picking either is
// silently routing the user's question to the wrong agent.
func TestResolveInstance_RefusesAnAmbiguousName(t *testing.T) {
	srv := newInstanceServer(t, 200, `[
	  {"id":"inst-a","name":"box","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}},
	  {"id":"inst-b","name":"box","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}}
	]`)

	got, err := resolveInstance(srv.URL, "box")
	if err == nil {
		t.Fatalf("resolveInstance picked %q from two instances sharing a name", got)
	}
	if !strings.Contains(err.Error(), "unique") {
		t.Errorf("error %q does not explain that the name must be unique", err)
	}
	if got != "" {
		t.Errorf("returned instance id %q alongside the error", got)
	}
}

func TestResolveInstance_ReportsAnUnknownName(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)

	_, err := resolveInstance(srv.URL, "nosuch")
	if err == nil {
		t.Fatal("resolveInstance accepted a name no instance carries")
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error %q does not name the instance that was asked for", err)
	}
}

func TestResolveInstance_WithNoNamePicksTheOneEnabledLocalClaudeCodeInstance(t *testing.T) {
	// Every other row is here to be rejected, each for a different reason.
	srv := newInstanceServer(t, 200, `[
	  {"id":"wrong-harness","name":"a","harness_type":"codex","enabled":true,"machine":{"transport":"local"}},
	  {"id":"disabled","name":"b","harness_type":"claude_code","enabled":false,"machine":{"transport":"local"}},
	  {"id":"remote","name":"c","harness_type":"claude_code","enabled":true,"machine":{"transport":"ssh"}},
	  {"id":"no-machine","name":"d","harness_type":"claude_code","enabled":true},
	  {"id":"the-one","name":"e","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}}
	]`)

	got, err := resolveInstance(srv.URL, "")
	if err != nil {
		t.Fatalf("resolveInstance: %v", err)
	}
	if got != "the-one" {
		t.Errorf("resolved %q, want the-one — the only enabled local claude_code row", got)
	}
}

// Each rejection reason gets its own fixture. One payload holding all four and
// a single assertion would stay green with three of the four gates deleted,
// because the remaining one still rejects everything else in the list.
func TestResolveInstance_EachRejectionReasonOnItsOwn(t *testing.T) {
	cases := []struct {
		reason  string
		payload string
	}{
		{"a different harness type", `[{"id":"x","name":"a","harness_type":"codex","enabled":true,"machine":{"transport":"local"}}]`},
		{"disabled", `[{"id":"x","name":"a","harness_type":"claude_code","enabled":false,"machine":{"transport":"local"}}]`},
		{"a remote machine", `[{"id":"x","name":"a","harness_type":"claude_code","enabled":true,"machine":{"transport":"ssh"}}]`},
		{"no machine at all", `[{"id":"x","name":"a","harness_type":"claude_code","enabled":true}]`},
	}
	for _, tc := range cases {
		srv := newInstanceServer(t, 200, tc.payload)
		got, err := resolveInstance(srv.URL, "")
		if err == nil {
			t.Errorf("%s: resolved %q, want a refusal", tc.reason, got)
			continue
		}
		if !strings.Contains(err.Error(), "no enabled local claude_code instance") {
			t.Errorf("%s: error %q does not say what it looked for", tc.reason, err)
		}
	}
}

func TestResolveInstance_RefusesTwoLocalClaudeCodeInstances(t *testing.T) {
	srv := newInstanceServer(t, 200, `[
	  {"id":"inst-a","name":"a","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}},
	  {"id":"inst-b","name":"b","harness_type":"claude_code","enabled":true,"machine":{"transport":"local"}}
	]`)

	got, err := resolveInstance(srv.URL, "")
	if err == nil {
		t.Fatalf("resolveInstance picked %q from two candidates", got)
	}
	if !strings.Contains(err.Error(), "--instance") {
		t.Errorf("error %q does not tell the caller how to disambiguate", err)
	}
}

func TestResolveInstance_ANonOKStatusIsReportedWithItsBody(t *testing.T) {
	srv := newInstanceServer(t, 500, "instance store unavailable")

	_, err := resolveInstance(srv.URL, "")
	if err == nil {
		t.Fatal("resolveInstance accepted an HTTP 500")
	}
	// The body is the only thing that says WHY, so it has to survive.
	if !strings.Contains(err.Error(), "instance store unavailable") {
		t.Errorf("error %q dropped the server's explanation", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not carry the status code", err)
	}
}

func TestResolveInstance_AnUndecodableBodyIsReportedAsSuch(t *testing.T) {
	srv := newInstanceServer(t, 200, `{"not":"an array"}`)

	_, err := resolveInstance(srv.URL, "")
	if err == nil {
		t.Fatal("resolveInstance accepted a body it could not decode")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error %q does not say the body was undecodable", err)
	}
}

func TestResolveInstance_AnUnreachableServerIsReported(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	url := srv.URL
	srv.Close() // nothing is listening now

	if _, err := resolveInstance(url, ""); err == nil {
		t.Fatal("resolveInstance reported success against a closed server")
	}
}

// ----------------------------------------------------------- buildPrompt

// The relay turn must end on the question so llm-bridge-server's awaiting_user
// heuristic fires and the session shows as waiting. A prompt that answers the
// question itself is the failure this framing exists to prevent.
func TestBuildPrompt_FramesTheAgentAsARelayAndCarriesTheQuestion(t *testing.T) {
	got := buildPrompt("approval-proposer", "Approve the update?", "")

	for _, want := range []string{"approval-proposer", "Approve the update?", "MESSAGE:"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Do not answer it yourself") {
		t.Errorf("prompt does not stop the agent answering its own question:\n%s", got)
	}
}

func TestBuildPrompt_OnReplyInstructionAppearsOnlyWhenOneWasGiven(t *testing.T) {
	withInstruction := buildPrompt("a", "Q?", "Move card 42 to Queued.")
	if !strings.Contains(withInstruction, "Move card 42 to Queued.") {
		t.Errorf("prompt dropped the --on-reply instruction:\n%s", withInstruction)
	}
	if !strings.Contains(withInstruction, "WHEN THE USER REPLIES") {
		t.Errorf("prompt does not defer the action to the reply turn:\n%s", withInstruction)
	}

	without := buildPrompt("a", "Q?", "")
	if !strings.Contains(without, "No further action is expected") {
		t.Errorf("prompt with no --on-reply does not say so:\n%s", without)
	}

	// Whitespace is not an instruction.
	blank := buildPrompt("a", "Q?", "   \n\t ")
	if !strings.Contains(blank, "No further action is expected") {
		t.Errorf("a whitespace-only --on-reply was treated as an instruction:\n%s", blank)
	}
}

// ---------------------------------------------------------- --session reuse

// reuseConfig is a run that delivers into an existing herald session.
// reminder-coordinator passes the id an earlier ask printed, so a nag lands in
// the morning digest's session instead of opening a second one.
func reuseConfig(bridgeURL string) askConfig {
	cfg := baseConfig(bridgeURL)
	cfg.reuseSession = "herald-1754654400000000000"
	return cfg
}

// A Send with no Create is what makes the adapter route to the bridge session it
// already made for this bus id. A Create here opens a second session with the
// same title, which is the duplicate the flag exists to prevent.
func TestRunAsk_ReuseDeliversASendIntoTheNamedSessionAndCreatesNothing(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := reuseConfig(srv.URL)

	id, err := runAsk(cfg, c.connect, io.Discard)
	if err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if id != "herald-1754654400000000000" {
		t.Errorf("returned session id %q, want the reused id — the caller records it for the next reuse", id)
	}
	if got := len(c.pub.published); got != 1 {
		t.Fatalf("published %d messages, want exactly 1", got)
	}
	if got := c.pub.published[0].subject; got != "bridge.inbound" {
		t.Errorf("published on subject %q, want %q", got, "bridge.inbound")
	}
	in := decodeInbound(t, c.pub.published[0])
	if in.BusSessionID != "herald-1754654400000000000" {
		t.Errorf("BusSessionID = %q, want the reused id", in.BusSessionID)
	}
	if in.Create != nil {
		t.Errorf("reuse carried a Create (%+v); the adapter would open a second session", in.Create)
	}
	if in.Send == nil || !strings.Contains(in.Send.Message, "Approve the update?") {
		t.Errorf("reuse Send = %+v, want the relay prompt carrying the question", in.Send)
	}
	checks := []struct{ field, got, want string }{
		{"Channel", in.Channel, "herald"},
		{"Orchestrator", in.Orchestrator, "claude-code"},
		{"Agent", in.Agent, "approval-proposer"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// The session already has a host; asking the bridge for one is wasted and,
	// with the bridge down, would refuse a message that could have gone through.
	if srv.requests != 0 {
		t.Errorf("reuse made %d instance lookups, want 0", srv.requests)
	}
	if c.pub.closed != 1 {
		t.Errorf("closed the connection %d times, want 1", c.pub.closed)
	}
}

// -instance and -title only mean something when a session is created. Taking
// them silently with -session would let a caller believe they had effect.
func TestRunAsk_ReuseRefusesTheCreateTimeFlags(t *testing.T) {
	cases := []struct {
		flag  string
		setUp func(*askConfig)
	}{
		{"-instance", func(c *askConfig) { c.instanceName = "box" }},
		{"-title", func(c *askConfig) { c.title = "Approval needed" }},
	}
	for _, tc := range cases {
		srv := newInstanceServer(t, 200, oneLocalClaudeCode)
		c := newConnector()
		cfg := reuseConfig(srv.URL)
		tc.setUp(&cfg)

		id, err := runAsk(cfg, c.connect, io.Discard)
		if err == nil {
			t.Errorf("%s with -session: accepted, want a refusal", tc.flag)
			continue
		}
		if !strings.Contains(err.Error(), tc.flag) || !strings.Contains(err.Error(), "-session") {
			t.Errorf("%s with -session: error %q does not name both flags", tc.flag, err)
		}
		if id != "" {
			t.Errorf("%s with -session: returned session id %q alongside the refusal", tc.flag, id)
		}
		if c.calls != 0 || srv.requests != 0 {
			t.Errorf("%s with -session: opened %d connections and made %d lookups before refusing, want 0 and 0", tc.flag, c.calls, srv.requests)
		}
	}
}

// The reuse path checks the question and the --from tag like the create path
// does: an unreadable message has nothing to relay, whichever session it goes to.
func TestRunAsk_ReuseStillRefusesAnUnreadableQuestionAndFromTag(t *testing.T) {
	for _, tc := range []struct {
		what  string
		setUp func(*askConfig)
		want  string
	}{
		{"question", func(c *askConfig) { c.question = "\x1b\x07" }, "question"},
		{"from", func(c *askConfig) { c.from = "her\nald" }, "--from"},
	} {
		srv := newInstanceServer(t, 200, oneLocalClaudeCode)
		c := newConnector()
		cfg := reuseConfig(srv.URL)
		tc.setUp(&cfg)

		_, err := runAsk(cfg, c.connect, io.Discard)
		if err == nil {
			t.Errorf("reuse accepted an unreadable %s", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("reuse with a bad %s: error %q does not say %q", tc.what, err, tc.want)
		}
		if c.calls != 0 {
			t.Errorf("reuse with a bad %s: opened %d connections, want 0", tc.what, c.calls)
		}
	}
}

func TestRunAsk_ReuseDryRunPublishesNothingAndOpensNoConnection(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := reuseConfig(srv.URL)
	cfg.dryRun = true

	var out strings.Builder
	id, err := runAsk(cfg, c.connect, &out)
	if err != nil {
		t.Fatalf("runAsk: %v", err)
	}
	if id != "" {
		t.Errorf("dry run returned session id %q, want empty", id)
	}
	if c.calls != 0 || len(c.pub.published) != 0 {
		t.Errorf("dry run opened %d connections and published %d messages, want 0 and 0", c.calls, len(c.pub.published))
	}
	report := out.String()
	for _, want := range []string{"herald-1754654400000000000", "(reused)", "approval-proposer", "Approve the update?"} {
		if !strings.Contains(report, want) {
			t.Errorf("dry-run report is missing %q:\n%s", want, report)
		}
	}
}

func TestRunAsk_ReuseReportsAFailedConnectionAndARefusedPublish(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)

	down := newConnector()
	down.failWith = fmt.Errorf("no route to host")
	id, err := runAsk(reuseConfig(srv.URL), down.connect, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "nats connect") {
		t.Errorf("reuse with no connection: err = %v, want a nats connect failure", err)
	}
	if id != "" {
		t.Errorf("reuse with no connection returned session id %q", id)
	}

	refused := newConnector()
	refused.pub.failWith = fmt.Errorf("jetstream unavailable")
	id, err = runAsk(reuseConfig(srv.URL), refused.connect, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "publish") {
		t.Errorf("reuse with a refused publish: err = %v, want a publish failure", err)
	}
	if id != "" {
		t.Errorf("reuse with a refused publish returned session id %q", id)
	}
	if refused.pub.closed != 1 {
		t.Errorf("closed the connection %d times on the failure path, want 1", refused.pub.closed)
	}
}

// ------------------------------------------------------------ model role

// A new herald session names the herald_model_role role as its own model, so
// the bridge's per-harness default never decides what it runs on.
func TestRunAsk_NewSessionNamesTheHeraldModelRole(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	if _, err := runAsk(baseConfig(srv.URL), c.connect, io.Discard); err != nil {
		t.Fatal(err)
	}
	in := decodeInbound(t, c.pub.published[0])
	if string(in.Create.HarnessConfig) != `{"model":"default"}` {
		t.Errorf("harness_config = %s, want the role as the session's model", in.Create.HarnessConfig)
	}
}

// A role that cannot be read or that claude_code cannot run opens nothing.
func TestRunAsk_AnUnusableModelRolePublishesNothing(t *testing.T) {
	srv := newInstanceServer(t, 200, oneLocalClaudeCode)
	c := newConnector()
	cfg := baseConfig(srv.URL)
	cfg.heraldModelRole = func() (string, string, error) {
		return "", "", fmt.Errorf("role \"balanced\" starts a session on gpt-test, and harness claude_code runs only anthropic models")
	}
	_, err := runAsk(cfg, c.connect, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "runs only anthropic") {
		t.Fatalf("err = %v, want the harness refusal", err)
	}
	if c.calls != 0 {
		t.Errorf("opened %d connections for a session that must not start", c.calls)
	}
	cfg.heraldModelRole = nil
	if _, err := runAsk(cfg, c.connect, io.Discard); err == nil || c.calls != 0 {
		t.Errorf("no role reader: err = %v, connections = %d", err, c.calls)
	}
}

// readHeraldModelRole reads the scheduler's setting and checks it against
// claude_code with model-store and the bridge, all three at their paths under
// the one bridge address.
func TestReadHeraldModelRole(t *testing.T) {
	role := "default"
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/scheduler/settings":
			fmt.Fprintf(w, `{"service":"scheduler","settings":[{"key":"herald_model_role","value_type":"model_role","value":%q}]}`, role)
		case "/models/api/roles/default":
			fmt.Fprint(w, `{"id":"claude-test","provider":"anthropic"}`)
		case "/models/api/roles/balanced":
			fmt.Fprint(w, `{"id":"gpt-test","provider":"openai"}`)
		case "/harnesses":
			fmt.Fprint(w, `[{"name":"claude_code","supported_providers":["anthropic"]}]`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer bridge.Close()
	checker := sessionmodelrole.New(bridgestores.ModelsURL(bridge.URL), bridge.URL)

	gotRole, gotModel, err := readHeraldModelRole(context.Background(), bridgestores.SchedulerURL(bridge.URL), checker)
	if err != nil || gotRole != "default" || gotModel != "claude-test" {
		t.Fatalf("got %q %q %v, want default claude-test", gotRole, gotModel, err)
	}
	role = "balanced"
	if _, _, err := readHeraldModelRole(context.Background(), bridgestores.SchedulerURL(bridge.URL), checker); err == nil || !strings.Contains(err.Error(), "herald_model_role") {
		t.Errorf("err = %v, want a refusal naming the setting", err)
	}
}
