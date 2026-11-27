// ask spawns a "herald" session that relays a question or alert to the user in
// the dashboard and then waits for their reply.
//
// It is the reusable engagement primitive for agents and scheduled jobs that
// need to pull the user into the loop on something they didn't open themselves —
// e.g. the approval proposer asking "approve this update?", or a job raising an
// alert. The spawned agent presents the message and ends its turn with the
// question, which trips llm-bridge-server's awaiting_user heuristic so the
// session shows up in the dash session list with the amber "?" status. When the
// user replies, the bridge resumes the same agent (claude --resume) with full
// context to carry out the optional follow-up action.
//
// Design notes:
//   - Runs on the machine's existing claude_code instance (resolved by harness
//     type + local machine, or --instance by name), on the model-store role in
//     the scheduler's herald_model_role setting, checked against what
//     claude_code can run before the session is created. There is NO dedicated CC
//     instance — one per machine is enough; "herald" is a role, marked by the
//     herald- session-id prefix and Purpose="herald", not a separate instance.
//   - Type=herald: llm-bridge-server's isUnattendedSession treats herald like
//     autonomous, so the relay agent's tool calls auto-allow (keeping deny
//     guardrails) instead of parking on the prehook and wedging the turn — no
//     permission_mode=bypass needed. The herald- prefix keeps these out of the
//     kanban-curator idle-reaper (scoped to autoworker-).
//   - Attribution is structured: --from sets the message's Agent tag (the
//     sender shown in the UI). The prompt does not roleplay a persona.
//   - Session reuse: --session <herald-...> delivers the message as a new turn
//     in an existing herald session instead of opening another one. The caller
//     keeps the id ask printed and passes it back; reminder-coordinator uses
//     this so a nag firing after the morning digest lands in the digest's
//     session rather than a second "Needs you" entry with the same title.
//
// Usage:
//
//	ask --from approval-proposer \
//	    --on-reply "If yes, move kanban card <id> from 'Needs approval' to 'Queued'; if no, append their reason to the card body." \
//	    "Approve updating llm-bridge-gemini to add a retry on transient stream errors?"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	natsbus "github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgeauth"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/schedulersettings"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/textcut"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Wire values another program reads back, and the --from default, declared once
// at package level. A test drives these declarations rather than passing its own
// copy in: a fixture that supplies the value under test agrees with itself and
// stays green through the value's drift.
const (
	// defaultFromTag is the Agent (sender) tag when a caller names no agent.
	defaultFromTag = "herald"
	// heraldSessionPrefix marks a relay session. kanban-curator's idle-reaper is
	// scoped to the autoworker- prefix, so a herald session is left alone.
	heraldSessionPrefix = "herald-"
	// heraldChannel is the bus channel a relay session is published on.
	heraldChannel = "herald"
	// heraldPurpose is the session Purpose llm-bridge-server stores.
	heraldPurpose = "herald"
	// askOrigin names this binary as the session's creator, and names its NATS
	// connection.
	askOrigin = "ask"
	// orchestratorClaudeCode is the orchestrator the relay session runs under.
	orchestratorClaudeCode = "claude-code"
)

// askConfig carries everything one run needs, so the run is a function of its
// arguments rather than of package state and process flags.
type askConfig struct {
	instanceName string
	from         string
	title        string
	onReply      string
	reuseSession string
	bridgeURL    string
	question     string
	dryRun       bool
	now          time.Time
	// heraldModelRole returns the model-store role a new herald session starts
	// on, already checked against what claude_code can run, and the model it
	// resolves to. It is called only when a new session is opened.
	heraldModelRole func() (role, model string, err error)
}

// heraldPublisher is the seam a test stands in at to observe the dispatch
// decision — the half of this binary that spends money on a session.
type heraldPublisher interface {
	Publish(subject string, data any) error
	Close()
}

// connectFunc opens the publisher. It is a function rather than a value so a
// dry run never opens a connection, which is what the flag has always meant.
type connectFunc func() (heraldPublisher, error)

func main() {
	settings, err := schedulerconfig.Ask.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	var (
		instanceName = flag.String("instance", "", "Harness instance NAME to host the session. Empty = resolve the local machine's claude_code instance.")
		from         = flag.String("from", defaultFromTag, "Originating agent — sets the message's Agent (sender) tag. Use the real agent name, e.g. approval-proposer. Must already be one line of nothing but text: a name carrying control, bidi or line-breaking characters is refused, not rewritten, because ask will not invent an attribution.")
		title        = flag.String("title", "", "Session display name. Put on one line with control characters removed, but kept at whatever length you give it. Defaults to a truncated form of the question.")
		onReply      = flag.String("on-reply", "", "Instruction for what the agent should do once the user replies. Empty = just relay the alert; no action expected.")
		reuseSession = flag.String("session", "", "Existing herald bus session id to deliver into as a new turn, instead of opening a new session. The adapter routes it to the bridge session it created for that id; -instance and -title are meaningless here and rejected.")
		bridgeURL    = flag.String("bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL (for instance resolution).")
		dryRun       = flag.Bool("dry-run", false, "Print the resolved instance and prompt; do not publish.")
	)
	flag.Parse()

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so the scheduler API, noteboard and
	// kanban-store calls below are untouched.
	if err := bridgeauth.StampRequestsToBridge(*bridgeURL, settings.String(schedulerconfig.SettingBridgeServiceToken)); err != nil {
		log.Fatal(err)
	}

	cfg := askConfig{
		instanceName: *instanceName,
		from:         *from,
		title:        *title,
		onReply:      *onReply,
		reuseSession: *reuseSession,
		bridgeURL:    *bridgeURL,
		question:     strings.TrimSpace(strings.Join(flag.Args(), " ")),
		dryRun:       *dryRun,
		now:          time.Now().UTC(),
		heraldModelRole: func() (string, string, error) {
			return readHeraldModelRole(context.Background(),
				settings.String(schedulerconfig.SettingSchedulerURL),
				sessionmodelrole.New(settings.String(schedulerconfig.SettingModelStoreURL), *bridgeURL))
		},
	}
	natsURL := settings.String(schedulerconfig.SettingNATSURL)
	connect := func() (heraldPublisher, error) {
		return natsbus.Connect(natsbus.Options{URL: natsURL, Name: askOrigin})
	}

	sessionID, err := runAsk(cfg, connect, os.Stdout)
	if err != nil {
		log.Fatalf("ask: %v", err)
	}
	if sessionID != "" {
		fmt.Println(sessionID)
	}
}

// runAsk relays one question: into the existing herald session cfg.reuseSession
// names, or into a new one on the resolved instance. It returns the bus session
// id the message went to. A dry run writes what it would have sent to out,
// returns an empty id and opens no connection.
//
// Every failure is returned rather than fatal, so the caller decides — main
// still exits non-zero on all of them.
func runAsk(cfg askConfig, connect connectFunc, out io.Writer) (string, error) {
	if !hasReadableText(cfg.question) {
		return "", fmt.Errorf("a question/alert message is required (positional argument), and it must contain readable text — a message of only whitespace and control characters has nothing to relay and nothing to name the session with")
	}
	if err := validateAgentName(cfg.from); err != nil {
		return "", err
	}

	prompt := buildPrompt(cfg.from, cfg.question, cfg.onReply)

	// Reuse: deliver into an existing herald session as a new turn. The adapter
	// keeps a persisted bus_session_id → bridge session mapping and reuses it
	// for a Send with no Create, so the message lands in the same chat session
	// the earlier ask opened. Create-time knobs cannot apply to a session that
	// already exists, so passing one is an error, not something to ignore.
	if cfg.reuseSession != "" {
		if cfg.instanceName != "" {
			return "", fmt.Errorf("-instance sets where a NEW session is hosted; it cannot apply with -session")
		}
		if cfg.title != "" {
			return "", fmt.Errorf("-title names a NEW session at creation; it cannot apply with -session")
		}
		if cfg.dryRun {
			fmt.Fprintf(out, "session:  %s (reused)\nfrom:     %s\n\n--- prompt ---\n%s\n", cfg.reuseSession, cfg.from, prompt)
			return "", nil
		}
		err := publishInbound(connect, messages.BridgeInbound{
			BusSessionID: cfg.reuseSession,
			Agent:        cfg.from,
			Orchestrator: orchestratorClaudeCode,
			Channel:      heraldChannel,
			Timestamp:    cfg.now,
			Send: &msg.SendMessageRequest{
				Message: prompt,
			},
		})
		if err != nil {
			return "", err
		}
		return cfg.reuseSession, nil
	}

	instanceID, err := resolveInstance(cfg.bridgeURL, cfg.instanceName)
	if err != nil {
		return "", fmt.Errorf("resolve instance: %w", err)
	}
	if cfg.heraldModelRole == nil {
		return "", fmt.Errorf("no herald model role reader: a new session would run on whatever the bridge's claude_code default is")
	}
	modelRole, model, err := cfg.heraldModelRole()
	if err != nil {
		return "", fmt.Errorf("herald model role: %w", err)
	}

	displayName, titleHeldNoReadableText := resolveDisplayName(cfg.title, cfg.question)
	if titleHeldNoReadableText {
		// %q rather than %s on purpose: the reason this title was refused is that
		// it is made of control characters, and printing it raw would write that
		// ESC to the terminal reading the warning.
		log.Printf("ask: --title %q holds no readable text once control characters are removed — naming the session from the question instead", cfg.title)
	}

	if cfg.dryRun {
		fmt.Fprintf(out, "instance: %s\nrole:     %s (%s)\nfrom:     %s\nsession:  %s<ns>\ndisplay:  %s\n\n--- prompt ---\n%s\n",
			instanceID, modelRole, model, cfg.from, heraldSessionPrefix, displayName, prompt)
		return "", nil
	}

	sessionID := fmt.Sprintf("%s%d", heraldSessionPrefix, cfg.now.UnixNano())
	create := &msg.CreateSessionRequest{
		Harness:     msg.HarnessClaudeCode,
		InstanceID:  instanceID,
		DisplayName: displayName,
		Purpose:     heraldPurpose,
		// herald is unattended for the relay turn (no human resolves a tool
		// prehook until the user replies), so llm-bridge-server's
		// isUnattendedSession auto-allows tool calls while keeping deny
		// guardrails — no permission_mode=bypass needed.
		Type:      msg.SessionTypeHerald,
		Origin:    askOrigin,
		AutoStart: true,
	}
	if err := sessionmodelrole.SetModelRole(create, modelRole); err != nil {
		return "", err
	}
	err = publishInbound(connect, messages.BridgeInbound{
		BusSessionID: sessionID,
		Agent:        cfg.from,
		Orchestrator: orchestratorClaudeCode,
		Channel:      heraldChannel,
		Timestamp:    cfg.now,
		Create:       create,
		Send: &msg.SendMessageRequest{
			Message: prompt,
		},
	})
	if err != nil {
		return "", err
	}
	return sessionID, nil
}

// readHeraldModelRole reads the herald_model_role setting from the scheduler
// and checks that claude_code can run the model it resolves to. It returns the
// role and that model.
func readHeraldModelRole(ctx context.Context, schedulerURL string, checker sessionmodelrole.Checker) (string, string, error) {
	role, err := schedulersettings.ReadModelRole(ctx, schedulerURL, schedulerconfig.SettingHeraldModelRole)
	if err != nil {
		return "", "", err
	}
	model, err := checker.Check(role, msg.HarnessClaudeCode)
	if err != nil {
		return "", "", fmt.Errorf("%s %q: %w", schedulerconfig.SettingHeraldModelRole, role, err)
	}
	return role, model.ID, nil
}

// publishInbound opens one connection, publishes the request on the bridge's
// inbound subject, and closes the connection. ask is a one-shot; there is
// nothing else to clean up.
func publishInbound(connect connectFunc, in messages.BridgeInbound) error {
	pub, err := connect()
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer pub.Close()
	if err := pub.Publish(messages.BridgeInboundSubject, in); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// buildPrompt frames the spawned agent as a relay: present the message
// (attributed to `from`), end the turn with the question so the awaiting_user
// heuristic fires, then act on the reply only if an on-reply instruction exists.
// No persona roleplay — attribution rides the Agent tag, not the prose.
func buildPrompt(from, question, onReply string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Relay the following message from `+"`%s`"+` to the user, then wait. This turn: present the message in full and end on the question itself (a line finishing with "?") so it clearly awaits their answer. Do not answer it yourself or take any action yet.

MESSAGE:
`, from)
	b.WriteString(question)
	b.WriteString("\n")
	if strings.TrimSpace(onReply) != "" {
		b.WriteString("\nWHEN THE USER REPLIES (next turn only — not now):\n")
		b.WriteString(strings.TrimSpace(onReply))
		b.WriteString("\nCarry that out, then briefly confirm what you did.\n")
	} else {
		b.WriteString("\nWHEN THE USER REPLIES (next turn only — not now): acknowledge their answer. No further action is expected.\n")
	}
	return b.String()
}

type instanceView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	HarnessType string `json:"harness_type"`
	Enabled     bool   `json:"enabled"`
	Machine     *struct {
		Transport string `json:"transport"`
	} `json:"machine"`
}

// resolveInstance returns the instance id to host the session. With a name, it
// resolves by display name (must be unique). Without a name, it resolves the
// local machine's single enabled claude_code instance — one CC instance per
// machine is the rule, so there should be exactly one. Fails loudly on zero or
// ambiguous matches so a question never silently routes to the wrong agent.
func resolveInstance(bridgeURL, name string) (string, error) {
	url := strings.TrimRight(bridgeURL, "/") + "/instances"
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /instances: HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var instances []instanceView
	if err := json.Unmarshal(body, &instances); err != nil {
		return "", fmt.Errorf("decode instances: %w", err)
	}

	var matches []string
	if name != "" {
		for _, i := range instances {
			if i.Name == name {
				matches = append(matches, i.ID)
			}
		}
		switch len(matches) {
		case 0:
			return "", fmt.Errorf("no instance named %q", name)
		case 1:
			return matches[0], nil
		default:
			return "", fmt.Errorf("%d instances named %q — name must be unique", len(matches), name)
		}
	}

	// No name: the local machine's claude_code instance.
	for _, i := range instances {
		if i.HarnessType != string(msg.HarnessClaudeCode) || !i.Enabled {
			continue
		}
		if i.Machine == nil || i.Machine.Transport != "local" {
			continue
		}
		matches = append(matches, i.ID)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no enabled local claude_code instance found (pass --instance <name>)")
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d local claude_code instances — pass --instance <name> to disambiguate", len(matches))
	}
}

// displayNameRuneLimit is how much of the question becomes the session's display
// name when no --title is given. It is a count of CHARACTERS, not bytes: nothing
// downstream bounds the name's size (llm-bridge-server stores display_name as a
// TEXT column), so the only thing this number decides is how much of the name a
// human reads in the dash session list.
const displayNameRuneLimit = 60

// displayNameFromQuestion shapes a question into a session display name: strip
// what is not text, put it on one line, then cut to displayNameRuneLimit
// characters.
//
// The result is persisted on the created session and shown in the dash, and
// nothing downstream re-derives it, so a value damaged here is damaged for good.
// It also travels as JSON over the bus, and encoding/json replaces invalid bytes
// with U+FFFD and reports no error — which is why cutting mid-rune here was
// silent rather than loud. That laundering does NOT cover what spaceOutNonText
// removes: those characters are valid UTF-8, so json passes them through with a
// nil error, and DEL, the C1 set and the bidi controls travel the wire raw and
// unescaped. A value that is invalid gets laundered; a value that is valid and
// merely unwanted arrives intact.
func displayNameFromQuestion(question string) string {
	return textcut.TruncateToRuneLimit(shapeIntoOneLineOfText(question), displayNameRuneLimit)
}

// displayNameFromTitle shapes an explicit --title into a session display name. It
// makes the value well formed — no control or bidi characters, one line — and
// then stops. It deliberately does NOT cut to displayNameRuneLimit.
//
// A --title used to be stored exactly as typed, which meant every property this
// file establishes about a display name held only for the half of them derived
// from a question: a title could carry an ESC, a U+202E, a raw newline, and any
// length at all into the dash session list.
//
// The three steps are not one decision, and this is where they part:
//
//   - Removing what is not text, and putting the name on one line, are about the
//     value being a well-formed single-line name AT ALL. An ESC is an in-band
//     command to whatever renders the row; a bidi override makes the row read as
//     something nobody sent; a newline means the value is not one row. No caller
//     asks for those, and a caller cannot express "I want two lines" in a surface
//     that has one. So both apply however the name was arrived at.
//
//   - The 60-character cut is a reading budget, and only a DERIVED name has one to
//     spend. Deriving a name from a question must decide how much of the question
//     to take — that choice is the derivation. A title is not an excerpt of
//     anything: the caller stated the name they want, in full, and display_name is
//     the only copy of it (llm-bridge-server stores it as TEXT with no bound, so
//     nothing downstream re-derives or retains what a cut here would discard).
//     Shortening it would be this program spending a display budget on somebody
//     else's authored value, and losing the remainder for good. How wide a row
//     renders is the dash's decision, made where the row is drawn and made without
//     destroying anything.
//
// So: strip and collapse always, cut only what this program itself wrote.
func displayNameFromTitle(title string) string {
	return shapeIntoOneLineOfText(title)
}

// resolveDisplayName decides what the session is called: the explicit --title when
// it holds readable text, otherwise a name derived from the question. Both branches
// are shaped, which is the whole point of it — the choice used to be made on the
// raw title, so the shaping could be skipped by supplying one.
//
// The second return reports a title that was given and then shaped away to nothing
// — a title of only control characters, say. That is not the same as no title, and
// substituting the derived name without saying so would hide a caller's mistake
// behind a name that looks deliberate. It is not fatal: the question is still
// perfectly relayable, and refusing to deliver an alert over the spelling of its
// name would be the worse failure for a tool whose job is to reach the user.
func resolveDisplayName(title, question string) (displayName string, titleHeldNoReadableText bool) {
	if shaped := displayNameFromTitle(title); shaped != "" {
		return shaped, false
	}
	return displayNameFromQuestion(question), title != ""
}

// validateAgentName reports why from is not usable as the originating-agent tag,
// or nil if it is. It REFUSES a malformed name rather than repairing one, which
// is the opposite of what the same characters get in a display name two functions
// below, and the difference is not an inconsistency.
//
// A display name is a label this program is willing to author: when --title
// shapes away to nothing there is a second source for it — the question — so
// shaping it costs the caller nothing they cannot get back, and refusing to
// deliver an alert over the spelling of its label would be the worse failure.
//
// --from is not a label. It names WHO the message is attributed to, it is the
// only copy of that fact (nothing downstream re-derives it, and it lands in no
// column any query could join back to an agent), and it is what the user reads
// when deciding whether to act on an alert at all. So the two repairs available
// here are both worse than refusing:
//
//   - Rewriting it hands back an agent name nobody chose. "herald\x1bfoo" shaped
//     is "herald foo", an agent that does not exist, attached to a message that
//     will be acted on.
//   - Substituting the default would attribute the message to a different agent
//     outright, which is the same failure with the seam hidden.
//
// Refusing costs one caller one obvious fix. Every caller of ask is a program,
// not a person at a prompt, and a program that put an ESC in its own name has a
// bug that this is the cheapest place to learn about.
//
// The rule is therefore: an agent name must be one line of nothing but text. It
// is spelled as a comparison against the shaping rather than as a predicate over
// runes so that it cannot drift from what the display name calls well formed —
// the same reason hasReadableText is defined the way it is. Computing the repair
// and then declining to apply it is also what lets the error name the exact
// value the caller should have passed.
//
// This is deliberately stricter than "contains no control characters", and the
// extra ground it covers is the reason: a newline is whitespace, not a control
// character by isNonText's rule, so a --from of "her\nald" carries no non-text
// at all and still is not a name — it makes the Agent tag two lines and splits
// the backtick span buildPrompt wraps it in, so the relayed prompt reads as an
// unterminated code span rather than as an attribution.
func validateAgentName(from string) error {
	if !hasReadableText(from) {
		return fmt.Errorf("--from %q holds no readable text: it names the agent this message is attributed to, and there is no second place to recover that name from", from)
	}
	// %q on both, and for the same reason the refused --title is printed quoted:
	// the values worth refusing here are the ones that would write an escape
	// sequence to the terminal reading the error.
	if shaped := shapeIntoOneLineOfText(from); shaped != from {
		return fmt.Errorf("--from %q is not one line of text — it would have to be rewritten to %q to become one. Pass the agent's real name: ask refuses to invent an attribution, because the name is what the user reads when deciding whether to act on the alert", from, shaped)
	}
	return nil
}

// shapeIntoOneLineOfText removes what is not text and puts what remains on a single
// line. It is the shaping every display name gets, whatever it was derived from,
// and it is one function so that the two paths and the blank-message guard cannot
// drift apart into three opinions about what a well-formed name is.
func shapeIntoOneLineOfText(s string) string {
	return collapseWhitespaceRuns(spaceOutNonText(s))
}

// hasReadableText reports whether s holds anything a person could read once it
// has been shaped into a display name.
//
// main uses this as its blank-message guard, and it is defined as the shaping
// steps themselves rather than as a predicate over runes, because the two must
// agree by construction. strings.TrimSpace alone is not enough any more: it tests
// unicode.IsSpace, which is false for every character spaceOutNonText removes, so
// a question of "\x00\x00" or a lone U+202E is not blank by TrimSpace and would
// pass a TrimSpace guard on its way to an empty display name.
func hasReadableText(s string) bool {
	return shapeIntoOneLineOfText(s) != ""
}

// spaceOutNonText rewrites as a space every character that is not text: the
// control characters that are not whitespace, and the bidi formatting controls.
//
// Both classes are taken from Unicode's own tables rather than from a list of
// characters somebody remembered, which is the same rule collapseWhitespaceRuns
// follows for whitespace. Together they are 71 runes:
//
//   - unicode.IsControl(r) && !unicode.IsSpace(r) — 59 runes, the C0 and C1 sets
//     less the six that are whitespace. These are not text at all; they are
//     in-band commands to whatever renders the string. An ESC or a C1 CSI
//     reaching a terminal session list is an escape sequence somebody typed into
//     a question.
//   - unicode.Bidi_Control — 12 runes, the embeddings, overrides and isolates.
//     These reorder the text around them, so a U+202E in a display name reverses
//     the rest of the row it lands in and the row reads as something nobody sent.
//
// It deliberately does NOT take all 170 of category Cf. Cf contains
// unicode.Join_Control (U+200C ZWNJ and U+200D ZWJ), which Arabic, Persian and
// the Indic scripts need to shape correctly and which joins an emoji family into
// one glyph. Dropping those would corrupt legitimate text to remove characters
// that render nothing on their own and reorder nothing around them. Bidi_Control
// and Join_Control are both wholly inside Cf, and it is the reordering half that
// is the problem.
//
// Replacing with a space rather than dropping is the choice this function makes,
// and the two differ in output: "alpha\x00beta" becomes "alpha beta" here and
// would become "alphabeta" under a drop. Dropping is the one operation the rest
// of this shaping never performs — it invents a token that appeared nowhere in
// the input, and a reader cannot see where the seam was. Replacing can at worst
// split one word the user typed into two halves they also typed, which is visible
// and recoverable. It runs BEFORE collapseWhitespaceRuns, so a character next to
// existing whitespace disappears into that run instead of widening it.
//
// The question itself is left alone and only the display name is shaped: the
// prompt is relayed as a message for an agent to present, not written to a
// terminal, and editing what the user actually asked is a larger decision than
// naming a session safely.
func spaceOutNonText(s string) string {
	return strings.Map(func(r rune) rune {
		if isNonText(r) {
			return ' '
		}
		return r
	}, s)
}

// isNonText reports whether r is a control or bidi-formatting character, i.e. one
// of the characters spaceOutNonText removes. See its comment for why these two
// classes and not the whole of category Cf.
func isNonText(r rune) bool {
	return (unicode.IsControl(r) && !unicode.IsSpace(r)) || unicode.Is(unicode.Bidi_Control, r)
}

// collapseWhitespaceRuns rewrites every run of whitespace in s as a single space
// and drops the runs at either end.
//
// "One line" is not only about \n. This used to replace \n and nothing else, so a
// question typed on a CRLF machine kept its bare \r, and \t, \v, \f, U+0085,
// U+00A0 and U+3000 all traveled into a stored display name untouched. Splitting
// on unicode.IsSpace covers every one of them, and covers them by the same rule
// rather than by an ever-growing list of characters somebody remembered.
//
// It also collapses runs of ordinary spaces, and that is a deliberate behavior
// change rather than a side effect worth hiding: you cannot turn "a\r\nb" into
// one space without deciding what two adjacent whitespace characters mean. A
// display name is one row of a list, where a run of spaces renders as a gap that
// says nothing and spends a budget of 60 characters saying it.
//
// The result is empty only if s held no text. main rejects such a question before
// this is called, and it rejects it with hasReadableText, which is defined as this
// function composed with spaceOutNonText — so the guard and the shaping cannot
// disagree about what counts as blank. It used to guard with strings.TrimSpace,
// which was equivalent only while whitespace was the sole thing removed here.
func collapseWhitespaceRuns(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
