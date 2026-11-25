// kanban-classifier is the cheap-model curator that creates and updates
// kanban cards on the "Agent runs" board from interactive llm-bridge sessions.
//
// What it reads, and how often:
//
//	Only sessions a person drives (type interactive), only once a turn has
//	fully ended, and once per finished response. It never reads a oneshot
//	model call, its own included, nor an autonomous, system, herald or
//	external session. See interactiveSessionsOnly, turnInFlight and
//	newestFinishedResponseID: between them they are the whole rule.
//
// Cadence:
//
//	Registered as a scheduler shell job running every 15 minutes. On each
//	tick, pages bridge-server's /sessions/summary newest first until the
//	lookback cutoff, skips sessions whose newest finished response is the one
//	the check signature already records, and classifies the rest with
//	Anthropic Haiku from a bounded page of each session's newest turns.
//
// Decisions per session:
//
//   - trackable=false → set kanban:do-not-track tag on the session, never
//     classify again unless its event count grows by ≥doNotTrackReevalEvents.
//   - existing_cards updates → move card to matching column on the Agent
//     runs board, append summary to body. Bridge state has NO automatic
//     mapping to columns; the model owns column placement.
//   - new_tasks → create a new card per task on the board, link to the
//     session, land in the column matching the model's reported state.
//
// Signature is stored as a single entity_tag on session/<bridge_id>:
//
//	kanban-check-sig:result=<entry id>:events=<n>
//
// <entry id> is log-store's id for the result entry that closed the response
// last classified, and it alone decides whether there is anything new to read.
// <n> is the session's stored event count (the page's validator), kept so a
// do-not-track session can be re-opened after a large delta. Signatures written
// before 2026-09-18 (events=<n>:state=<s>:t=<minute>, and msg=<n> before that)
// name no response, so they count as "not yet classified" — see
// responseAlreadyClassified.
//
// Old sigs are deleted before a new one is written. A session reaching a
// terminal bridge state (completed/error) is marked with
// kanban-final-classified after the next classify and never reprocessed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/bridgeauth"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/bridgesession"
	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanvocab"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/oneshot"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/schedulerjobs"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/schedulersettings"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/textcut"
)

const (
	defaultBoardName     = kanbanvocab.BoardAgentRuns
	tagDoNotTrack        = "kanban-do-not-track"
	tagFinalClassified   = "kanban-final-classified"
	tagCheckSigPrefix    = "kanban-check-sig:"
	cardLinkSession      = "session"
	cardLinkSource       = "kanban-source"
	cardSourceClassifier = "classifier"
	maxClassifyPerTick   = 10
	// digestCharBudget is the size of one classifier prompt in characters
	// (~4 chars/token, so ~4k tokens). Characters, not tokens, because that
	// is what the builder can actually measure.
	digestCharBudget = 16000
	// Ceiling on the digest share the already-filed cards may take, and share
	// of what is left after that for recent assistant turns. The rest goes to
	// user prompts. The cards section only ever consumes what it needs, so a
	// generous ceiling costs nothing on a session with few cards and buys
	// complete card identity on one with many — which is exactly the session
	// that duplicates.
	existingCardsBudgetShare  = 0.55
	assistantTurnsBudgetShare = 0.35
	terminalStateCompleted    = "completed"
	terminalStateError        = "error"
	candidateLookback         = 7 * 24 * time.Hour
	// doNotTrackReevalEvents is how many new events re-open a do-not-track
	// session. It replaced "20 new messages" when the classifier stopped
	// downloading every message to count them; measured 2026-09-16, sessions
	// ran 148–228 events per prompt, so 20 messages (~10 prompts) is ~2,000.
	doNotTrackReevalEvents = 2000
	// digestTurns is how many of a session's newest prompt turns the digest
	// is built from. The digest keeps at most ~10 assistant turns and a 16k
	// character budget, so a longer transcript was downloaded and thrown away.
	digestTurns = 15

	// The scheduler job -register creates. -register posts it only when no job
	// of this name exists and never rewrites one, so a wrong value here stays
	// until someone PATCHes the live job by hand.
	schedulerJobName     = "kanban-classifier-tick"
	schedulerJobSchedule = "*/15 * * * *"
	// A tick walks every candidate session, fetches its messages from
	// bridge-server and makes one model call each, so it legitimately outruns
	// the scheduler's 5-minute default. Job 14 runs with 900 on this host; the
	// payload below did not send it, so -register on a fresh host would have
	// created the job with the default and had it killed mid-write.
	schedulerJobTimeoutSeconds = 900
)

// errSettingsRefused marks a parseFlags error that came from the environment:
// the settings registry refused a value. main prints it and exits 1, where the
// flag package has already printed its own errors and exits 2.
var errSettingsRefused = errors.New("settings refused")

// parseFlags builds the flag set and parses args into a config.
//
// The flag set lives here rather than in main() so that a test can reach the
// defaults. Registered inside main() they were unreachable — nothing in a test
// can call main() — so the twelve values this binary actually starts with had
// never been read by anything. The three test files already in this package
// build their own config fixtures, so every value they assert is one the test
// chose, not one that ships.
//
// Three of the twelve turn a tick into a no-op, and none of them fails.
// -register exits after touching the scheduler. -dry-run pays the full model
// bill and writes no card. -dump-digest prints every candidate's digest and
// classifies none of them — the worst of the three, because it fills the run
// history with plausible output rather than silence. Each leaves a process that
// runs every 15 minutes, logs a complete pass and exits 0, which from outside is
// a box with no active sessions.
//
// The flag set is a fresh FlagSet rather than the package-level
// flag.CommandLine: a test calling this twice would panic on the redefined
// flag, and that reads as a broken fixture rather than as the seam being
// unusable. ContinueOnError is what makes it callable at all — ExitOnError
// would take the test binary down on a bad argument instead of returning.
//
// environment is an argument because six defaults are read at
// flag-registration time; a test that exported KANBAN_STORE_URL to observe one
// would race every other test in the package for a process-wide variable. It
// is read through the binary's settings registry (internal/config), which
// refuses a misspelled KANBAN_CLASSIFIER_ variable.
// homeDirectory is an argument for the same reason: the registered command is
// home-relative and a test should not have to move HOME out from under the
// process to assert what it resolves to.
func parseFlags(args []string, environment servicesettings.Environment, homeDirectory string) (config, error) {
	settings, err := schedulerconfig.KanbanClassifier.NewSettingsRegistry(environment)
	if err != nil {
		return config{}, fmt.Errorf("%w: %v", errSettingsRefused, err)
	}
	fs := flag.NewFlagSet("kanban-classifier", flag.ContinueOnError)
	var c config
	fs.BoolVar(&c.RegisterJob, "register", false, "Register/upsert this binary as a scheduler cron job and exit.")
	fs.BoolVar(&c.DryRun, "dry-run", false, "Classify but do not write to kanban-store.")
	fs.StringVar(&c.OneSession, "session", "", "Classify exactly one session id and exit (no candidate filtering).")
	fs.StringVar(&c.BoardName, "board", defaultBoardName, "Kanban board name to write cards to.")
	fs.StringVar(&c.KanbanURL, "kanban-url", settings.String(schedulerconfig.SettingKanbanStoreURL), "kanban-store base URL")
	fs.StringVar(&c.BridgeURL, "bridge-url", settings.String(schedulerconfig.SettingBridgeURL), "llm-bridge-server base URL")
	fs.IntVar(&c.MaxClassify, "max", maxClassifyPerTick, "Max classifications per tick.")
	fs.IntVar(&c.DigestBudget, "digest-budget", digestCharBudget, "Digest size in characters (~4 chars per token).")
	fs.BoolVar(&c.DumpDigest, "dump-digest", false, "With -session: print the digest that would be sent and exit, without calling the model.")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	// Derived rather than registered: the dedupe gates are rationing for the
	// cron sweep, and naming a session by id is a human overruling them.
	c.ExplicitSession = c.OneSession != ""
	c.SchedulerURL = settings.String(schedulerconfig.SettingSchedulerURL)
	c.KanbanServiceToken = settings.String(schedulerconfig.SettingKanbanStoreServiceToken)
	c.BridgeServiceToken = settings.String(schedulerconfig.SettingBridgeServiceToken)
	c.BinaryPath = filepath.Join(homeDirectory, "bin", "kanban-classifier")
	return c, nil
}

// validate refuses the two configurations that run to a clean exit having
// classified nothing, every tick, for as long as they ship. Every other bad
// value in this config dies at the first HTTP call and says so.
//
// -dump-digest without -session is deliberately NOT refused. The flag's help
// text reads "with -session", but without one processSession still builds and
// prints each candidate's digest and returns false, so dumping the whole
// candidate sweep is a working debug path rather than a mistake.
func (c config) validate() error {
	if c.MaxClassify < 1 {
		// classifyTick checks `classified >= maxClassifications` before the first
		// session, so zero breaks the loop immediately. It does log "hit per-tick
		// cap (0)" — loud enough to find once someone looks, and indistinguishable
		// from a normal capped tick in a run history nobody reads.
		return fmt.Errorf("-max must be at least 1 (got %d): the per-tick cap is checked before the first session, so a tick with no budget classifies nothing", c.MaxClassify)
	}
	if c.DigestBudget < 1 {
		// buildDigest treats a non-positive budget as "use the default", so this
		// does not fail — it silently overrules the operator and sends a
		// full-size digest at full cost.
		return fmt.Errorf("-digest-budget must be at least 1 (got %d): buildDigest silently substitutes the default for a non-positive budget", c.DigestBudget)
	}
	return nil
}

func main() {
	os.Exit(run())
}

// run is the whole command. It returns the process exit status rather than
// calling os.Exit itself, so that every early return below settles the run's
// verdict through the same path and a new one cannot forget to. Copied in shape
// from cmd/email-classifier, which is where the verdict this returns was worked
// out; see internal/degradations for why a tick that fell short must not exit 0.
func run() int {
	// Error ignored exactly as before: an unset HOME leaves this empty and the
	// registered command resolves relative to the working directory, which is a
	// path that fails at exec time either way.
	homeDirectory, _ := os.UserHomeDir()

	cfg, err := parseFlags(os.Args[1:], servicesettings.ProcessEnvironment(), homeDirectory)
	if err != nil {
		if errors.Is(err, errSettingsRefused) {
			log.Fatal(err)
		}
		// Both codes are the ones this binary used before the flag set moved out
		// of main(): flag.ExitOnError exited 2 on a bad argument after printing
		// the error and the usage message, and 0 on -h. Parse has already printed
		// both, so nothing a caller sees moved.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}

	// llm-bridge-server gates every route; stamp this process's calls to it
	// with the service token. Host-scoped, so noteboard, kanban-store and the
	// scheduler API are untouched.
	if err := bridgeauth.StampRequestsToBridge(cfg.BridgeURL, cfg.BridgeServiceToken); err != nil {
		log.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}

	if cfg.RegisterJob {
		if err := registerSchedulerJob(cfg); err != nil {
			log.Fatalf("register: %v", err)
		}
		return 0
	}

	ctx := context.Background()

	// Collects the ways this run falls short of doing its whole job. Threaded
	// rather than global so the failure sites stay testable in isolation. The
	// subject matches the prefix the progress lines already carry, so a reader
	// of scheduler run history sees one voice.
	degraded := degradations.New("classifier")

	// The model role is the scheduler's stored setting, changed on its
	// settings page. A digest dump never reaches the model, so it needs none.
	if !cfg.DumpDigest {
		if cfg.ModelRole, err = schedulersettings.ReadModelRole(ctx, cfg.SchedulerURL, schedulerconfig.SettingKanbanClassifierModelRole); err != nil {
			log.Fatal(err)
		}
	}

	kc := kanbanclient.New(cfg.KanbanURL, cfg.KanbanServiceToken)
	board, err := kc.FindBoard(cfg.BoardName)
	if err != nil {
		log.Fatalf("find board %q: %v", cfg.BoardName, err)
	}
	if board == nil {
		// First-ever invocation: bootstrap the board so cards have somewhere to go.
		if err := kc.EnsureBoard(cfg.BoardName, kanbanvocab.AgentRunsColumns()); err != nil {
			log.Fatalf("bootstrap board: %v", err)
		}
		board = &kanbanclient.Board{ID: kc.BoardID, Name: cfg.BoardName}
	} else {
		// Cache columns onto the client.
		cols, err := kc.ListColumns(board.ID)
		if err != nil {
			log.Fatalf("list columns: %v", err)
		}
		kc.BoardID = board.ID
		kc.ColumnsByName = map[string]string{}
		for _, col := range cols {
			kc.ColumnsByName[col.Name] = col.ID
		}
	}
	if err := requireColumns(kc, []string{kanbanvocab.ColumnInProgress, kanbanvocab.ColumnBlocked, kanbanvocab.ColumnDone, kanbanvocab.ColumnFailed}); err != nil {
		log.Fatalf("%v", err)
	}

	if cfg.OneSession != "" {
		sess, err := getSession(cfg.BridgeURL, cfg.OneSession)
		if err != nil {
			log.Fatalf("get session %s: %v", cfg.OneSession, err)
		}
		processSession(ctx, cfg, kc, sess, degraded)
		return degraded.ExitCode()
	}

	candidates, err := selectCandidates(cfg)
	if err != nil {
		log.Fatalf("select candidates: %v", err)
	}
	log.Printf("[classifier] candidates=%d", len(candidates))

	classified := classifyTick(candidates, cfg.MaxClassify, func(sess session) bool {
		return processSession(ctx, cfg, kc, sess, degraded)
	})
	log.Printf("[classifier] done: classified=%d", classified)
	// Fail loud: a tick that could not classify must not report success. This
	// exact loop once logged "classifier failed" every 15 minutes for a dead
	// credential while the scheduler recorded success on every run. A failed
	// model call is one of the shortfalls the recorder holds.
	return degraded.ExitCode()
}

// ============================ The per-tick spend cap ============================

// classifyTick walks the candidates in the order given and classifies at most
// maxClassifications of them, returning how many it classified.
//
// This is the ceiling on what one tick can spend, and it is the only one of the
// classifier's skip gates that is not per-session: the others each save one
// model call for one session, while this one bounds the whole tick.
// selectCandidates returns everything updated in the last seven days, so with
// the cap gone a single tick classifies every session on the box instead of ten
// — ninety-six times a day.
//
// It lives out of run() so a test can call it and a sabotage case can break it.
//
// classifyOneSession reports whether it actually classified the session, as
// opposed to skipping it at one of the per-session gates. Only a classification
// consumes budget — the cap counts paid model calls, not sessions looked at.
func classifyTick(candidates []session, maxClassifications int, classifyOneSession func(session) bool) int {
	classified := 0
	for i, sess := range candidates {
		if classified >= maxClassifications {
			// Count the candidates never looked at, not everything the tick
			// declined to classify. A candidate a gate skipped was decided, not
			// deferred: it cost nothing and next tick will skip it again for
			// nothing. Counting from `classified` instead of from the loop
			// position inflates this by exactly the number of sessions the
			// gates skipped, and this line is the operator's only signal that a
			// real backlog is building.
			log.Printf("[classifier] hit per-tick cap (%d); deferring %d sessions to next tick",
				maxClassifications, len(candidates)-i)
			break
		}
		if classifyOneSession(sess) {
			classified++
		}
	}
	return classified
}

type config struct {
	KanbanURL string
	BridgeURL string
	// ModelRole is the model-store role classify asks for. run reads it from
	// the scheduler's stored settings; it is not a flag.
	ModelRole    string
	BoardName    string
	DryRun       bool
	DigestBudget int
	DumpDigest   bool

	// RegisterJob and OneSession are the two mode switches. Both exit before the
	// candidate sweep runs.
	RegisterJob bool
	OneSession  string

	// MaxClassify bounds how many model calls one tick pays for.
	MaxClassify int

	// SchedulerURL and BinaryPath are used only by -register. They are parsed
	// with everything else so the registration payload is assertable without
	// reaching the environment.
	SchedulerURL string
	BinaryPath   string

	// The tokens this binary presents as an internal service. Both stores
	// gate every route.
	KanbanServiceToken string
	BridgeServiceToken string

	// ExplicitSession is set when an operator named a single session on the
	// command line. The dedupe gates below exist to ration the cron sweep, not
	// to overrule a human who asked for this exact session by id, so they are
	// bypassed — loudly — when this is set.
	ExplicitSession bool
}

// ============================ Candidate selection ============================

type session struct {
	SessionID   string `json:"session_id"`
	State       string `json:"state"`
	Type        string `json:"type"`
	Purpose     string `json:"purpose"`
	DisplayName string `json:"display_name"`
	Harness     string `json:"harness"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func selectCandidates(cfg config) ([]session, error) {
	// Newest first, and the lookback cutoff ends the walk: the summary is ordered
	// by updated_at, so the first older row means every later one is older too.
	recent, err := listSessionsUpdatedSince(cfg.BridgeURL, time.Now().Add(-candidateLookback))
	if err != nil {
		return nil, err
	}
	candidates, skippedByType := interactiveSessionsOnly(recent)
	log.Printf("[classifier] skipped %d sessions that are not interactive: %s",
		len(recent)-len(candidates), describeSkippedTypes(skippedByType))
	return candidates, nil
}

// interactiveSessionsOnly keeps the sessions a person drives and reports how
// many of each other type it dropped.
//
// It is an allowlist of one type on purpose. The rule it replaced dropped
// purpose "oneshot" and kept everything else, and everything else was the
// autoworker, the dispatcher, delegates, the renamer and subagents: on
// 2026-09-17, the first day that rule was live, 527 of the day's 943 model calls
// read autoworker and scheduler sessions. Before it there was no rule at all, and
// the classifier read its own oneshot calls — 16,787 of 17,085 calls on
// 2026-09-14 were the classifier classifying a classifier call. A new session
// type, or a new purpose, is now out until someone decides it is in.
//
// The match is on the type the bridge assigned, never on the name, the purpose
// or the prompt. A row with no type is not interactive. It filters the board
// only: the dropped sessions are still stored and still counted wherever usage
// is reported.
func interactiveSessionsOnly(sessions []session) (kept []session, skippedByType map[string]int) {
	kept = make([]session, 0, len(sessions))
	skippedByType = map[string]int{}
	for _, sess := range sessions {
		if msg.SessionType(sess.Type) != msg.SessionTypeInteractive {
			skippedByType[sess.Type]++
			continue
		}
		kept = append(kept, sess)
	}
	return kept, skippedByType
}

// describeSkippedTypes renders the dropped counts in a stable order for the log.
func describeSkippedTypes(skippedByType map[string]int) string {
	if len(skippedByType) == 0 {
		return "none"
	}
	types := make([]string, 0, len(skippedByType))
	for sessionType := range skippedByType {
		types = append(types, sessionType)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, sessionType := range types {
		name := sessionType
		if name == "" {
			name = "(no type)"
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, skippedByType[sessionType]))
	}
	return strings.Join(parts, " ")
}

// turnInFlight reports whether the session is part-way through a turn, so
// there is no final output to read yet. The active states are llm-bridge's own
// list. A session parked on a permission prompt is mid-turn too: IsBlockedOnUser
// covers it, and awaiting_user is taken back out because that state means the
// turn ended and the reply is the person's to give.
func turnInFlight(state msg.SessionState) bool {
	return state.IsActive() || (state.IsBlockedOnUser() && state != msg.SessionAwaitingUser)
}

// ============================ Process one session ============================

// processSession runs the full pipeline for one session. Returns true if a
// classifier call was made (vs. dedupe-skipped or final-classified).
// processSession classifies one candidate and applies the result, reporting
// whether it got as far as spending a model call.
//
// That bool answers "did this tick do work", which is not the same question as
// "did this tick do its job", and the difference is what card `b4130d06` was
// filed about: every failure below returns false, and a run made entirely of
// false still printed `classified=0` and exited 0. The recorder is the second
// answer. Every path that gives up on a candidate, and every write that does not
// land, records why — so a tick that classified nothing because the API refused
// it is distinguishable from a tick that classified nothing because there was
// nothing to do.
func processSession(ctx context.Context, cfg config, kc *kanbanclient.Client, sess session, degraded *degradations.Recorder) bool {
	tags, err := kc.ListEntityTags(cardLinkSession, sess.SessionID)
	if err != nil {
		log.Printf("[%s] list tags: %v", sess.SessionID, err)
		degraded.Record("session %s not classified: list tags failed", sess.SessionID)
		return false
	}
	tagSet := tagSet(tags)

	// Permanent skip: previously classified as terminal.
	if tagSet[tagFinalClassified] {
		if !cfg.ExplicitSession {
			return false
		}
		log.Printf("[%s] already final-classified — proceeding anyway because this session was named explicitly",
			sess.SessionID)
	}

	// A turn that has not ended has no final output. The next tick after it
	// settles reads it, once.
	if turnInFlight(msg.SessionState(sess.State)) {
		if !cfg.ExplicitSession {
			return false
		}
		log.Printf("[%s] a turn is in flight (state %s) — proceeding anyway because this session was named explicitly",
			sess.SessionID, sess.State)
	}

	page, err := getSessionPage(cfg.BridgeURL, sess.SessionID)
	if err != nil {
		log.Printf("[%s] get messages: %v", sess.SessionID, err)
		degraded.Record("session %s not classified: reading its messages failed", sess.SessionID)
		return false
	}
	msgs, eventCount := page.messages, page.eventCount

	if page.newestFinishedResponseID == "" {
		if !cfg.ExplicitSession {
			return false
		}
		log.Printf("[%s] no finished response in its newest %d turns — proceeding anyway because this session was named explicitly",
			sess.SessionID, digestTurns)
	}

	if !cfg.ExplicitSession && saidNothingSince(page.newestEntryAt, time.Now().Add(-candidateLookback)) {
		return false
	}

	// do-not-track: only re-evaluate after a big delta.
	if tagSet[tagDoNotTrack] {
		priorN, known := priorEventCount(tagSet)
		if !known {
			// A signature from before event counts: nothing to measure a delta
			// against. Record today's count and leave the decision standing, rather
			// than re-classifying every do-not-track session at once.
			if !cfg.ExplicitSession {
				writeSig(kc, sess, tagSet, buildSig(page.newestFinishedResponseID, eventCount), degraded)
				return false
			}
		} else if eventCount-priorN < doNotTrackReevalEvents {
			if !cfg.ExplicitSession {
				return false
			}
			log.Printf("[%s] do-not-track and Δevents=%d < %d — proceeding anyway because this session was named explicitly",
				sess.SessionID, eventCount-priorN, doNotTrackReevalEvents)
		} else {
			log.Printf("[%s] do-not-track but Δevents=%d ≥ %d — re-evaluating",
				sess.SessionID, eventCount-priorN, doNotTrackReevalEvents)
		}
	}

	// One model call per finished response: bail if this one was already read.
	curSig := buildSig(page.newestFinishedResponseID, eventCount)
	priorSig, priorSigCount := priorSigTag(tagSet)
	if priorSigCount > 1 {
		log.Printf("[%s] WARNING: %d check signatures on one session, want 1 — using the newest (%s); "+
			"an earlier tick failed to delete the one it replaced",
			sess.SessionID, priorSigCount, priorSig)
	}
	if responseAlreadyClassified(priorSig, page.newestFinishedResponseID) {
		if !cfg.ExplicitSession {
			return false
		}
		log.Printf("[%s] newest finished response already classified (%s) — proceeding anyway because this session was named explicitly",
			sess.SessionID, priorSig)
	}

	// Look up existing cards already linked to this session.
	cards, err := kc.ListCardsForEntity(cardLinkSession, sess.SessionID)
	if err != nil {
		log.Printf("[%s] list cards: %v", sess.SessionID, err)
		degraded.Record("session %s not classified: listing its cards failed", sess.SessionID)
		return false
	}

	digest := buildDigest(sess, msgs, cards, cfg.DigestBudget)
	if cfg.DumpDigest {
		fmt.Printf("%s\n", digest)
		log.Printf("[%s] dump-digest: %d chars, %d existing card(s)", sess.SessionID, len(digest), len(cards))
		return false
	}
	report, err := classify(ctx, cfg, digest)
	if errors.Is(err, errTruncatedReport) {
		// Acting on a truncated report is worse than not acting: its missing
		// existing_cards look like "no opinion" (so cards silently stay in the
		// column they were first filed in) and any new_tasks it did manage to
		// emit get filed against a list of existing cards we know is short, so
		// the dedupe in applyNew is running blind. Refuse the whole report.
		//
		// The signature is still written. The input is deterministic, so a
		// retry would truncate identically and bill for it every tick; the
		// session is instead left alone until it actually changes.
		log.Printf("[%s] WARNING: %d existing card(s) NOT updated — %v. "+
			"This session needs more than max_tokens=%d to classify; raise it or narrow what the model must report "+
			"(see noteboard todo 55aab37a). Re-run: kanban-classifier -session %s -dry-run",
			sess.SessionID, len(cards), err, maxOutputTokens, sess.SessionID)
		degraded.Record("session %s: report truncated, %d existing card(s) not updated", sess.SessionID, len(cards))
		if !cfg.DryRun {
			writeSig(kc, sess, tagSet, curSig, degraded)
		}
		return true
	}
	if err != nil {
		log.Printf("[%s] classifier failed: %v — leaving sig unchanged for retry", sess.SessionID, err)
		degraded.Record("session %s not classified: the model call failed", sess.SessionID)
		return false
	}
	log.Printf("[%s] trackable=%v existing=%d new=%d summary=%q",
		sess.SessionID, report.Trackable, len(report.ExistingCards), len(report.NewTasks),
		textcut.TruncateToRuneLimit(report.SessionSummary, 100))

	if cfg.DryRun {
		return true
	}

	if !report.Trackable {
		applyDoNotTrack(kc, sess, degraded)
		writeSig(kc, sess, tagSet, curSig, degraded) // records the event count, the baseline the reeval delta measures from
		return true
	}

	// A session that has cards but comes back with an opinion on none of them
	// has had none of them updated, whether or not it also filed new work — the
	// original form of this check only fired when new_tasks was non-empty, so
	// the commonest case (existing=0, new=0) passed in silence.
	//
	// Truncation is handled above and cannot reach here, so the remaining
	// causes are a digest that does not carry [EXISTING CARDS] or a model that
	// read them and declined to answer. -dump-digest distinguishes the two.
	if len(cards) > 0 && len(report.ExistingCards) == 0 {
		log.Printf("[%s] WARNING: %d existing card(s) went unaddressed (filed %d new task(s)) — "+
			"confirm the digest carries [EXISTING CARDS] with: kanban-classifier -session %s -dump-digest",
			sess.SessionID, len(cards), len(report.NewTasks), sess.SessionID)
		degraded.Record("session %s: %d existing card(s) went unaddressed by the model", sess.SessionID, len(cards))
	}

	applyExisting(kc, sess, report, degraded)
	applyNew(kc, sess, cards, report, degraded)

	writeSig(kc, sess, tagSet, curSig, degraded)

	// If bridge state is terminal, mark the session as final-classified.
	if sess.State == terminalStateCompleted || sess.State == terminalStateError {
		_ = kc.AddEntityTag(cardLinkSession, sess.SessionID, tagFinalClassified)
	}
	return true
}

// ============================ Tag helpers ============================

func tagSet(tags []kanbanclient.EntityTag) map[string]bool {
	out := map[string]bool{}
	for _, t := range tags {
		out[t.Tag] = true
	}
	return out
}

// priorSigTag returns the newest check signature on a session, and how many it
// found. The caller that knows the session warns when there is more than one.
//
// Only one should exist, because writeSig deletes the others; a second one means
// a delete failed on an earlier tick. Taking whichever one a map range reaches
// first let the skip gate flip between ticks, since Go randomises map order: the
// same unchanged session skipped on one tick and bought a model call on the next.
//
// Newest is the one with the highest event count, since a session's events only
// ever grow. A signature with no readable count (the msg= format) sorts below any
// that has one, and equal counts fall back to string order so the pick never
// depends on the map. Newest-wins is also the direction that cannot cause a wrong
// skip: a stale pick can only fail to match and classify once more.
//
// Ported from fe081af, which ordered by a t= stamp the signature no longer has.
func priorSigTag(tagSet map[string]bool) (sig string, found int) {
	for tag := range tagSet {
		if !strings.HasPrefix(tag, tagCheckSigPrefix) {
			continue
		}
		found++
		if found == 1 || newerSig(tag, sig) {
			sig = tag
		}
	}
	return sig, found
}

// newerSig reports whether signature a was written after signature b.
func newerSig(a, b string) bool {
	countA, knownA := sigEventCount(a)
	countB, knownB := sigEventCount(b)
	if knownA != knownB {
		return knownA
	}
	if countA != countB {
		return countA > countB
	}
	return a > b
}

// priorEventCount parses the events= component from the prior sig. known is
// false when there is no prior sig or it predates event counts (msg=).
func priorEventCount(tagSet map[string]bool) (n int, known bool) {
	sig, _ := priorSigTag(tagSet)
	return sigEventCount(sig)
}

// sigEventCount reads the events= component of one signature. known is false
// when it is absent or is not a whole number: strconv.Atoi refuses "12abc",
// which fmt.Sscanf read as 12.
func sigEventCount(sig string) (n int, known bool) {
	if !strings.HasPrefix(sig, tagCheckSigPrefix) {
		return 0, false
	}
	for _, part := range strings.Split(strings.TrimPrefix(sig, tagCheckSigPrefix), ":") {
		if value, found := strings.CutPrefix(part, "events="); found {
			n, err := strconv.Atoi(value)
			return n, err == nil
		}
	}
	return 0, false
}

func buildSig(finishedResponseID string, eventCount int) string {
	return fmt.Sprintf("%sresult=%s:events=%d", tagCheckSigPrefix, finishedResponseID, eventCount)
}

// responseAlreadyClassified reports whether the prior signature records this
// finished response, which is the whole of "once per response".
//
// The event count used to decide, with the state. Neither is about a response:
// llm-bridge appends events to a session after its turn has ended (the derived
// state the turn-end signal pass writes is one), and a session moves between
// idle, awaiting_user and completed with nothing new said. Each of those read as
// "changed" and bought another model call on the same final message.
//
// A signature from before 2026-09-18 names no response, so it cannot say this one
// was read, and the answer is false: such a session is classified once more and
// then carries a signature that can. So is a session with no signature at all.
// An empty id is never "already classified" — there is nothing it could match.
func responseAlreadyClassified(priorSig, finishedResponseID string) bool {
	if finishedResponseID == "" {
		return false
	}
	recorded, ok := classifiedResponseID(priorSig)
	return ok && recorded == finishedResponseID
}

// classifiedResponseID reads the result= field of a signature. ok is false for
// no signature, an unreadable one, and the formats that predate the field.
func classifiedResponseID(sig string) (id string, ok bool) {
	if !strings.HasPrefix(sig, tagCheckSigPrefix) {
		return "", false
	}
	for _, field := range strings.Split(strings.TrimPrefix(sig, tagCheckSigPrefix), ":") {
		if value, found := strings.CutPrefix(field, "result="); found && value != "" {
			return value, true
		}
	}
	return "", false
}

// saidNothingSince reports whether a session's newest entry is older than
// cutoff. The summary walk picks candidates by the row's updated_at, which any
// write to the row advances: the 2026-09-16 cost backfill put 2,536
// never-classified sessions from May and June inside the seven-day lookback.
// The lookback is about the conversation, so the newest entry has the last
// word. A page with no entry time is not judged here.
func saidNothingSince(newestEntryAt, cutoff time.Time) bool {
	return !newestEntryAt.IsZero() && newestEntryAt.Before(cutoff)
}

// writeSig deletes the prior check-sig tags (if any) and adds the new one.
//
// A failure to write the signature is a shortfall and not a cosmetic one: the
// signature is the whole dedupe gate, so a session whose tag did not land is
// classified again on the next tick and every tick after it, at the price of a
// model call each time.
func writeSig(kc *kanbanclient.Client, sess session, oldTags map[string]bool, sig string, degraded *degradations.Recorder) {
	for tag := range oldTags {
		if strings.HasPrefix(tag, tagCheckSigPrefix) && tag != sig {
			// A delete that fails leaves two signatures behind. priorSigTag reads
			// the newest, so the gate still works, but the next write is the only
			// thing that will retry the delete, and before this nothing said so.
			if err := kc.DeleteEntityTag(cardLinkSession, sess.SessionID, tag); err != nil {
				log.Printf("[%s] delete superseded check sig %q: %v", sess.SessionID, tag, err)
				degraded.Record("session %s: superseded check signature not deleted, it now carries more than one", sess.SessionID)
			}
		}
	}
	if err := kc.AddEntityTag(cardLinkSession, sess.SessionID, sig); err != nil {
		log.Printf("[%s] add check sig: %v", sess.SessionID, err)
		degraded.Record("session %s: check signature not written, it will be re-classified and re-billed next tick", sess.SessionID)
	}
}

// applyDoNotTrack tags a session so it is never classified again.
//
// It does NOT record the message count the session was tagged at, though it used to
// take one. writeSig does, on the line after every call site, by writing events=N
// into the check-sig tag — and priorEventCount reads it back out to measure the
// doNotTrackReevalEvents delta. The coupling is real and invisible, so keep the writeSig
// call next to the applyDoNotTrack call.
func applyDoNotTrack(kc *kanbanclient.Client, sess session, degraded *degradations.Recorder) {
	if err := kc.AddEntityTag(cardLinkSession, sess.SessionID, tagDoNotTrack); err != nil {
		log.Printf("[%s] add do-not-track: %v", sess.SessionID, err)
		degraded.Record("session %s: do-not-track tag not written, it will be re-classified next tick", sess.SessionID)
	}
}

// ============================ Apply classifier output ============================

// provenanceTrailer renders the line every classifier-written card body ends
// with. It exists once because both write paths need it and the update path
// rebuilds the body from scratch: while only applyNew stamped the session id,
// every state update erased it. Measured 2026-08-07 — 19 of the 21 open
// classifier todos whose body named no session had one until an update
// overwrote it, recoverable from their noteboard revisions.
//
// The authoritative link is the id-keyed `session` card_link this same code
// writes to kanban-store, and nothing joins on this line. It is the
// human-readable echo of that link, for a reader of the board — which is
// exactly why it has to survive an update rather than only a create.
func provenanceTrailer(action, sessionID, state string) string {
	return fmt.Sprintf("_%s classifier @ %s · session=%s · state=%s_",
		action, time.Now().UTC().Format(time.RFC3339), sessionID, state)
}

// applyExisting moves and rewrites the cards the model had an opinion about.
//
// Every failure here is a card the board still shows in the wrong column, which
// is worse than an unclassified session: the session at least looks unfiled,
// while a card left behind looks current. Measured on job 14's run history
// 2026-08-29, one card had been failing to move with `404 Not Found` on eight of
// the last sixty ticks — every one of those runs recorded `success`.
func applyExisting(kc *kanbanclient.Client, sess session, report *Report, degraded *degradations.Recorder) {
	for _, ec := range report.ExistingCards {
		colName := stateToColumn(ec.State)
		colID, ok := kc.ColumnsByName[colName]
		if !ok {
			log.Printf("[%s] column %q missing for existing card update; skipping", sess.SessionID, colName)
			degraded.Record("session %s: card %s not moved, column %q is missing from the board", sess.SessionID, ec.CardID, colName)
			continue
		}
		moveResult, err := kc.MoveCard(ec.CardID, kc.BoardID, colID, 0)
		if err != nil {
			log.Printf("[%s] move card %s → %s: %v", sess.SessionID, ec.CardID, colName, err)
			degraded.Record("session %s: card %s not moved to %q", sess.SessionID, ec.CardID, colName)
			continue
		}
		if moveResult.AutoStatusError != "" {
			log.Printf("[%s] moved card %s to %s but its noteboard status was not set to match: %s", sess.SessionID, ec.CardID, colName, moveResult.AutoStatusError)
		}
		body := ec.Summary
		if ec.Blocker != "" {
			body += "\n\n**Blocker:** " + ec.Blocker
		}
		body += "\n\n" + provenanceTrailer("updated by", sess.SessionID, ec.State) + "\n"
		if err := kc.PatchCardBody(ec.CardID, body); err != nil {
			log.Printf("[%s] patch existing card %s: %v", sess.SessionID, ec.CardID, err)
			degraded.Record("session %s: card %s moved but its body was not updated", sess.SessionID, ec.CardID)
		}
	}
}

func applyNew(kc *kanbanclient.Client, sess session, existingCards []kanbanclient.EntityCardEntry, report *Report, degraded *degradations.Recorder) {
	filed := map[string]bool{}
	for _, ec := range existingCards {
		if title, _ := ec.Item["title"].(string); title != "" {
			filed[normalizeTitle(title)] = true
		}
	}

	for _, nt := range report.NewTasks {
		// Last line of defence against re-filing work that is already on the
		// board. The digest is what actually prevents this; an exact title
		// repeat means even that failed.
		if filed[normalizeTitle(nt.Title)] {
			log.Printf("[%s] skipping new task %q — a card with that title is already linked to this session",
				sess.SessionID, textcut.TruncateToRuneLimit(nt.Title, 80))
			continue
		}
		filed[normalizeTitle(nt.Title)] = true

		colName := stateToColumn(nt.State)
		body := nt.Summary
		if nt.Blocker != "" {
			body += "\n\n**Blocker:** " + nt.Blocker
		}
		body += "\n\n" + provenanceTrailer("created by", sess.SessionID, nt.State) + "\n"
		args := kanbanclient.CreateCardArgs{
			ColumnName: colName,
			// 80 is the hard cut and matches the schema's maxLength; the prompt's
			// "≤60 chars" is advice, not this number badly copied. See the note on
			// systemPrompt. prompt_vocabulary_test.go holds the two together.
			// Cut in characters, because maxLength counts code points: a byte cut
			// would shorten a non-ASCII title that obeyed the bound.
			Title:     textcut.TruncateToRuneLimit(nt.Title, 80),
			Body:      body,
			Tags:      []string{cardSourceClassifier, sess.Harness, sess.Purpose},
			SessionID: "", // classifier adds its own session link with label "classifier" below
			Provider:  "",
		}
		cardID, err := kc.CreateCard(args)
		if err != nil {
			log.Printf("[%s] create new task card: %v", sess.SessionID, err)
			degraded.Record("session %s: new task %q was not filed", sess.SessionID, textcut.TruncateToRuneLimit(nt.Title, 60))
			continue
		}
		// The card exists at this point, so neither failure below loses the
		// work — but both lose the JOIN. An unlinked card is invisible to the
		// dedupe on the next tick, which is how one task becomes six cards.
		if err := kc.AddCardLink(cardID, cardLinkSession, sess.SessionID, "classifier"); err != nil {
			log.Printf("[%s] link new card to session: %v", sess.SessionID, err)
			degraded.Record("session %s: card %s was filed but not linked to the session, so the dedupe cannot see it", sess.SessionID, cardID)
		}
		if err := kc.AddCardLink(cardID, cardLinkSource, cardSourceClassifier, ""); err != nil {
			log.Printf("[%s] mark source on new card: %v", sess.SessionID, err)
			degraded.Record("session %s: card %s was filed without its source link", sess.SessionID, cardID)
		}
	}
}

// normalizeTitle folds case and punctuation so two spellings of the same
// title compare equal. It is deliberately exact on words: a fuzzy match here
// would suppress genuinely new tasks that merely read alike.
func normalizeTitle(title string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func stateToColumn(state string) string {
	switch state {
	case "in_progress":
		return kanbanvocab.ColumnInProgress
	case "blocked":
		return kanbanvocab.ColumnBlocked
	case "done":
		return kanbanvocab.ColumnDone
	case "abandoned":
		return kanbanvocab.ColumnFailed
	default:
		return kanbanvocab.ColumnInProgress
	}
}

// requireColumns reports the first lifecycle column the board is missing.
//
// It returns an error where it used to log.Fatalf: a function that ends the
// process has no seam a test can reach, so the check that decides whether this
// binary runs at all was the one thing in it nothing could score. run() still
// exits — the column set is not something a tick can work around — but it
// exits at a call site that says so.
func requireColumns(kc *kanbanclient.Client, names []string) error {
	for _, n := range names {
		if kc.ColumnsByName[n] == "" {
			return fmt.Errorf("column %q missing on board %s", n, kc.BoardID)
		}
	}
	return nil
}

// ============================ Bridge-server client ============================

// listSessionsUpdatedSince pages GET /sessions/summary, newest first, and stops
// at the first session last updated before `since`.
//
// This used to be GET /sessions, which has no bound: every column of every
// session, info blobs included, 64 MB in 2.6 s on 2026-09-16 — fetched every 15
// minutes to keep the ~0.1% updated inside the lookback window.
func listSessionsUpdatedSince(bridgeURL string, since time.Time) ([]session, error) {
	base := strings.TrimRight(bridgeURL, "/") + "/sessions/summary?limit=100"
	var out []session
	cursor := ""
	for {
		pageURL := base
		if cursor != "" {
			pageURL += "&before=" + url.QueryEscape(cursor)
		}
		page, err := getSummaryPage(pageURL)
		if err != nil {
			return nil, err
		}
		for _, row := range page.Sessions {
			updated, err := time.Parse(time.RFC3339, row.UpdatedAt)
			if err != nil {
				return nil, fmt.Errorf("session %s: updatedAt %q: %w", row.SessionID, row.UpdatedAt, err)
			}
			if updated.Before(since) {
				return out, nil
			}
			out = append(out, session{
				SessionID:   row.SessionID,
				State:       row.State,
				Type:        row.Type,
				Purpose:     row.Purpose,
				DisplayName: row.DisplayName,
				Harness:     row.Harness,
				CreatedAt:   row.CreatedAt,
				UpdatedAt:   row.UpdatedAt,
			})
		}
		if page.Next == "" {
			return out, nil
		}
		cursor = page.Next
	}
}

type summaryPage struct {
	Sessions []summaryRow `json:"sessions"`
	Next     string       `json:"next"`
}

// summaryRow is the part of one GET /sessions/summary row the sweep reads.
type summaryRow struct {
	SessionID   string `json:"sessionId"`
	State       string `json:"state"`
	Type        string `json:"type"`
	Purpose     string `json:"purpose"`
	DisplayName string `json:"displayName"`
	Harness     string `json:"harness"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func getSummaryPage(pageURL string) (summaryPage, error) {
	var page summaryPage
	resp, err := http.Get(pageURL)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return page, fmt.Errorf("list sessions: %s — %s", resp.Status, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return page, fmt.Errorf("decode sessions summary: %w", err)
	}
	return page, nil
}

func getSession(bridgeURL, id string) (session, error) {
	var s session
	sessionURL, err := bridgesession.URL(bridgeURL, id)
	if err != nil {
		return s, err
	}
	resp, err := http.Get(sessionURL)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s, fmt.Errorf("get session %s: %s — %s", id, resp.Status, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return s, err
	}
	if s.SessionID == "" {
		s.SessionID = id
	}
	return s, nil
}

type bridgeMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Events    []map[string]any `json:"events"`
	Timestamp string           `json:"timestamp"`
}

// sessionPage is what the classifier reads of one session: its newest turns as
// digest messages, and its whole-session event count for the signature.
type sessionPage struct {
	messages   []bridgeMessage
	eventCount int
	// newestEntryAt is the latest timestamp among the page's messages: when the
	// conversation last moved. Zero when the page carries no readable timestamp.
	newestEntryAt time.Time
	// newestFinishedResponseID is log-store's id for the result entry that closed
	// the newest finished response on the page. Empty when no turn on it finished.
	newestFinishedResponseID string
}

// turnPage is the part of GET /sessions/{id}/messages?limit= the digest reads.
type turnPage struct {
	Model struct {
		Turns []struct {
			EntryIDs []string `json:"entryIds"`
		} `json:"turns"`
		Entries map[string]struct {
			Role      string `json:"role"`
			Kind      string `json:"kind"`
			Text      string `json:"text"`
			ToolName  string `json:"toolName"`
			Ts        string `json:"ts"`
			Duplicate bool   `json:"duplicate"`
		} `json:"entries"`
		Validator struct {
			EventCount int `json:"eventCount"`
		} `json:"validator"`
	} `json:"model"`
}

// getSessionPage reads a session's newest digestTurns turns, tool payloads as
// previews.
//
// This used to be GET /sessions/{id}/messages with no limit: every event of the
// session materialized with its raw payload (306 MB for one session at worst),
// read only to count the messages and keep the last few for a 16k-character
// digest. The bounded page carries the session-wide event count in its validator.
func getSessionPage(bridgeURL, id string) (sessionPage, error) {
	messagesURL, err := bridgesession.SubresourceURL(bridgeURL, id, "messages")
	if err != nil {
		return sessionPage{}, err
	}
	pageURL := fmt.Sprintf("%s?limit=%d&payload=preview", messagesURL, digestTurns)
	resp, err := http.Get(pageURL)
	if err != nil {
		return sessionPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return sessionPage{}, fmt.Errorf("get messages: %s — %s", resp.Status, string(body))
	}
	var page turnPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return sessionPage{}, fmt.Errorf("decode messages page: %w", err)
	}
	messages := digestMessages(page)
	newestEntryAt, err := newestMessageTime(messages)
	if err != nil {
		return sessionPage{}, fmt.Errorf("session %s: %w", id, err)
	}
	return sessionPage{
		messages:                 messages,
		eventCount:               page.Model.Validator.EventCount,
		newestEntryAt:            newestEntryAt,
		newestFinishedResponseID: newestFinishedResponseID(page),
	}, nil
}

// newestFinishedResponseID is the id of the newest result entry that carries
// text, or "" when the page has none.
//
// A result entry is what log-store writes when a turn ends on its own terms, so
// it is the one marker of "this response is final". A turn the person cut short
// has none, and neither does one still running. A result with no text is a turn
// that answered nothing — /compact leaves one — and is not a response to read.
// The id is log-store's and is carried as it came; it is never parsed.
func newestFinishedResponseID(page turnPage) string {
	newest := ""
	for _, turn := range page.Model.Turns {
		for _, id := range turn.EntryIDs {
			e, ok := page.Model.Entries[id]
			if !ok || e.Duplicate {
				continue
			}
			if e.Role == "assistant" && e.Kind == "result" && e.Text != "" {
				newest = id
			}
		}
	}
	return newest
}

// newestMessageTime is the latest timestamp among msgs, zero when none carries
// one. A timestamp that does not parse is an error: read as "no time", it would
// make an old-format signature compare as changed for ever.
func newestMessageTime(msgs []bridgeMessage) (time.Time, error) {
	var newest time.Time
	for _, m := range msgs {
		if m.Timestamp == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m.Timestamp)
		if err != nil {
			return time.Time{}, fmt.Errorf("entry timestamp %q: %w", m.Timestamp, err)
		}
		if at.After(newest) {
			newest = at
		}
	}
	return newest, nil
}

// digestMessages flattens a turn page into the messages the digest sections read,
// in turn order: a prompt is a user message, assistant text or a result is an
// assistant message, and a tool call is an event on an assistant message. Hidden
// duplicates (the OTel copy of a prompt) are skipped.
func digestMessages(page turnPage) []bridgeMessage {
	var out []bridgeMessage
	for _, turn := range page.Model.Turns {
		for _, id := range turn.EntryIDs {
			e, ok := page.Model.Entries[id]
			if !ok || e.Duplicate {
				continue
			}
			switch {
			case e.Role == "user" && e.Kind == "text":
				out = append(out, bridgeMessage{Role: "user", Content: e.Text, Timestamp: e.Ts})
			case e.Role == "assistant" && (e.Kind == "text" || e.Kind == "result") && e.Text != "":
				out = append(out, bridgeMessage{Role: "assistant", Content: e.Text, Timestamp: e.Ts})
			case e.Kind == "tool_call" && e.ToolName != "":
				out = append(out, bridgeMessage{
					Role:      "assistant",
					Timestamp: e.Ts,
					Events:    []map[string]any{{"type": "tool_call", "tool_name": e.ToolName}},
				})
			}
		}
	}
	return out
}

// ============================ Digest builder ============================

// buildDigest renders one session as the classifier's prompt input, within
// digestCharBudget characters.
//
// Section ORDER IS LOAD-BEARING, and so is the per-section budgeting. The
// digest used to be assembled in conversation order and then chopped at the
// budget with a single slice, which put [EXISTING CARDS] last — so on any
// long session the whole section fell off the end. The model was then told
// "use new_tasks only for tasks not already represented by an existing card"
// while being shown no cards at all, and duly re-filed every task it had
// already filed, once per tick. One live session minted six near-identical
// cards for one task in ninety minutes that way.
//
// So the dedupe-critical section is emitted FIRST and gets its own reserve,
// and the conversational bulk is fitted into what remains — newest first,
// because the recent turns carry the "is it done / is it blocked" signal.
// Nothing is ever cut mid-section, and every drop is announced in the text
// so the model (and a human reading a dump) can see what is missing.
func buildDigest(sess session, msgs []bridgeMessage, existingCards []kanbanclient.EntityCardEntry, charBudget int) string {
	if charBudget <= 0 {
		charBudget = digestCharBudget
	}

	meta := buildMetaSection(sess, len(msgs))
	cards := buildExistingCardsSection(existingCards, budgetShare(charBudget, existingCardsBudgetShare))
	tools := buildToolActivitySection(msgs)

	remaining := charBudget - len(meta) - len(cards) - len(tools)
	if remaining < 0 {
		remaining = 0
	}
	turns := buildAssistantTurnsSection(msgs, budgetShare(remaining, assistantTurnsBudgetShare))
	prompts := buildUserPromptsSection(msgs, remaining-len(turns))

	return meta + cards + prompts + turns + tools
}

// budgetShare is `fraction` of total, as a character count.
func budgetShare(total int, fraction float64) int {
	if total <= 0 {
		return 0
	}
	return int(float64(total) * fraction)
}

func buildMetaSection(sess session, msgCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[SESSION META]\n")
	fmt.Fprintf(&b, "bridge_id: %s\n", sess.SessionID)
	fmt.Fprintf(&b, "display_name: %s\n", sess.DisplayName)
	fmt.Fprintf(&b, "harness: %s\n", sess.Harness)
	fmt.Fprintf(&b, "source: %s\n", sess.Purpose)
	fmt.Fprintf(&b, "state: %s\n", sess.State)
	fmt.Fprintf(&b, "created: %s\n", sess.CreatedAt)
	fmt.Fprintf(&b, "last_message: %s\n", sess.UpdatedAt)
	fmt.Fprintf(&b, "messages_shown: %d\n\n", msgCount)
	return b.String()
}

// buildExistingCardsSection renders the cards already linked to this session.
// Card identity (card_id + title) is what stops the model re-filing work, so
// it is never dropped to save space: bodies are shed first, and only if the
// titles alone still overflow are the oldest cards omitted — loudly.
func buildExistingCardsSection(existingCards []kanbanclient.EntityCardEntry, budget int) string {
	const header = "[EXISTING CARDS — already on the kanban for this session]\n"
	if len(existingCards) == 0 {
		return header + "(none)\n\n"
	}

	withBodies := make([]string, 0, len(existingCards))
	titlesOnly := make([]string, 0, len(existingCards))
	for _, ec := range existingCards {
		title, _ := ec.Item["title"].(string)
		status, _ := ec.Item["status"].(string)
		body, _ := ec.Item["body"].(string)
		// Digest space, so these bounds are byte budgets. This 80 is not the
		// schema's 80, which counts characters; the two only share a number.
		digestTitle := textcut.TruncateToByteBudgetOnRuneBoundary(title, 80)
		withBodies = append(withBodies, fmt.Sprintf("- card_id=%s title=%q status=%s\n  body=%q\n",
			ec.CardID, digestTitle, status, textcut.TruncateToByteBudgetOnRuneBoundary(body, 400)))
		titlesOnly = append(titlesOnly, fmt.Sprintf("- card_id=%s title=%q status=%s\n",
			ec.CardID, digestTitle, status))
	}

	entries := withBodies
	if totalLen(entries) > budgetForEntries(header, budget) {
		entries = titlesOnly
	}
	return renderSectionKeepingNewest(header, entries, budget, "older card(s) omitted")
}

func buildUserPromptsSection(msgs []bridgeMessage, budget int) string {
	const header = "[USER PROMPTS — verbatim, newest last]\n"
	entries := []string{}
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			entries = append(entries, fmt.Sprintf("%d. %s\n", len(entries)+1, textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 1500)))
		}
	}
	if len(entries) == 0 {
		return header + "(none)\n\n"
	}
	return renderSectionKeepingNewest(header, entries, budget, "earlier prompt(s) omitted")
}

func buildAssistantTurnsSection(msgs []bridgeMessage, budget int) string {
	const header = "[ASSISTANT TURNS — recent, with text content]\n"
	withText := []bridgeMessage{}
	for _, m := range msgs {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			withText = append(withText, m)
		}
	}
	if len(withText) == 0 {
		return header + "(no assistant text content)\n\n"
	}
	// Take the last ~10 turns, then fit whatever share of those the budget allows.
	if len(withText) > 10 {
		withText = withText[len(withText)-10:]
	}
	entries := make([]string, 0, len(withText))
	for i, m := range withText {
		entries = append(entries, fmt.Sprintf("  turn %d (%s):\n  %s\n\n", i+1, m.Timestamp, textcut.TruncateToByteBudgetOnRuneBoundary(m.Content, 600)))
	}
	return renderSectionKeepingNewest(header, entries, budget, "earlier turn(s) omitted")
}

func buildToolActivitySection(msgs []bridgeMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[TOOL ACTIVITY — last ~15 tool calls]\n")
	tools := []string{}
	for _, m := range msgs {
		for _, e := range m.Events {
			if e["type"] != "tool_call" {
				continue
			}
			name, _ := e["tool_name"].(string)
			if name == "" {
				if tc, ok := e["tool_call"].(map[string]any); ok {
					name, _ = tc["name"].(string)
				}
			}
			if name == "" {
				continue
			}
			tools = append(tools, name)
		}
	}
	if len(tools) > 15 {
		tools = tools[len(tools)-15:]
	}
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s\n", t)
	}
	if len(tools) == 0 {
		fmt.Fprintf(&b, "(none)\n")
	}
	b.WriteString("\n")
	return b.String()
}

// omissionMarker announces that the oldest omittedCount entries were dropped
// from a section, and is empty when nothing was dropped. Its length varies with
// the count it carries, which is why fitKeepingNewest recomputes it per
// candidate cut rather than reserving one worst case up front.
func omissionMarker(omittedCount int, omittedNoun string) string {
	if omittedCount <= 0 {
		return ""
	}
	return fmt.Sprintf("(… %d %s)\n", omittedCount, omittedNoun)
}

// fitKeepingNewest returns as many trailing entries as fit in budget, in
// order, with a marker naming how many leading ones were dropped. It keeps at
// least one entry even when that entry and its marker overflow — a section
// that silently renders empty is how the truncation bug hid for so long, and
// announcing the drop is worth the same again. That pair is the only way this
// returns more than budget, and it is reachable only on a budget smaller than
// one entry, which digestCharBudget is nowhere near.
//
// THE MARKER IS CHARGED TO THE BUDGET, and it has to be. It used not to be, so
// every section that dropped anything overran its share by the marker's length
// (~34 characters — the ellipsis is U+2026, three of them). Widening
// digestCharBudget does not fix that: the overrun is one marker per section
// that drops anything, so it scales with the number of sections, which is a
// property of the builder and not of the budget.
//
// It hid because a second bug canceled it out. Under the old raw byte cut
// every truncated prompt came out at exactly 1503 bytes and the arithmetic
// happened to land inside the budget; the rune-safe walk-back makes them 1500,
// one more entry fits, and the unbudgeted marker no longer has slack to hide in.
func fitKeepingNewest(entries []string, budget int, omittedNoun string) string {
	if len(entries) == 0 {
		return ""
	}
	first := len(entries)
	used := 0
	for i := len(entries) - 1; i >= 0; i-- {
		// Keeping entries[i:] costs those entries plus the marker announcing
		// the i still dropped — which is nothing at all once i reaches 0, so
		// the last entry taken pays the marker back.
		costOfKeeping := used + len(entries[i]) + len(omissionMarker(i, omittedNoun))
		if costOfKeeping > budget && i != len(entries)-1 {
			break
		}
		used += len(entries[i])
		first = i
	}
	var b strings.Builder
	b.WriteString(omissionMarker(first, omittedNoun))
	for _, e := range entries[first:] {
		b.WriteString(e)
	}
	return b.String()
}

// sectionTrailer ends every fitted digest section, so the next section's
// header starts after a blank line. It is part of what the section returns, so
// it is charged to the section's budget like anything else it emits.
const sectionTrailer = "\n"

// budgetForEntries is how much of a section's budget its entries may use, once
// the header and trailer the section always emits are paid for. Every caller
// goes through this rather than subtracting for itself: the header was already
// subtracted at all three call sites and the trailer at none of them, and one
// unbudgeted character per section is exactly the shape of defect this whole
// arithmetic exists to prevent.
func budgetForEntries(header string, sectionBudget int) int {
	return sectionBudget - len(header) - len(sectionTrailer)
}

// renderSectionKeepingNewest renders one fitted digest section — header, then
// as many of the newest entries as fit, then the trailer — in at most
// sectionBudget characters.
//
// buildDigest's whole-digest guarantee is only as strong as this one. It hands
// each section a share of the budget and then concatenates what comes back, so
// a section returning more than it was handed overruns the assembled digest by
// exactly that much, with nothing downstream to catch it. The lone exception is
// fitKeepingNewest's deliberate one: an entry too big to fit is emitted anyway
// rather than rendering the section empty.
func renderSectionKeepingNewest(header string, entries []string, sectionBudget int, omittedNoun string) string {
	return header + fitKeepingNewest(entries, budgetForEntries(header, sectionBudget), omittedNoun) + sectionTrailer
}

func totalLen(entries []string) int {
	n := 0
	for _, e := range entries {
		n += len(e)
	}
	return n
}

// ============================ Anthropic call ============================

// Report mirrors the report_classification tool's input schema.
type Report struct {
	Trackable      bool          `json:"trackable"`
	SessionSummary string        `json:"session_summary,omitempty"`
	ExistingCards  []ExistingRec `json:"existing_cards,omitempty"`
	NewTasks       []NewRec      `json:"new_tasks,omitempty"`
}

type ExistingRec struct {
	CardID  string `json:"card_id"`
	State   string `json:"state"`
	Summary string `json:"summary"`
	Blocker string `json:"blocker,omitempty"`
}

type NewRec struct {
	Title   string `json:"title"`
	State   string `json:"state"`
	Summary string `json:"summary"`
	Blocker string `json:"blocker,omitempty"`
}

// systemPrompt is the fourth place the state vocabulary is written down and the third
// place the title bound is. The other three of each are data — the two schema enums,
// the schema's maxLength, and applyNew's title cut — so a walk of the schema
// reaches them and this one it cannot: it is prose inside a const.
//
// That asymmetry is why the drift is worth a test. A state the SCHEMA rejects costs a
// refused tool call, which is loud. A state the PROMPT omits is one the model is never
// told about, so it is never reported, and nothing fails.
//
// The two title numbers are deliberately different and are not one number badly
// copied: ≤60 below is ADVICE, aimed at titles a human can read on a card, and the
// schema's maxLength of 80 is the HARD CUT that applyNew enforces. The only
// relationship that has to hold is advice ≤ cut.
//
// prompt_vocabulary_test.go holds all four authorings to each other.
const systemPrompt = `You are an internal task-tracking classifier for an AI agent infrastructure. You analyze a single chat session between a user and an autonomous agent, and decide what work-tracking cards should exist on a kanban board to represent the tasks happening in that session.

Trackable cards represent meaningful units of user-directed work — bug fixes, feature builds, investigations, migrations, refactors, infra work. NOT: trivial questions, casual chat, one-line edits, or pure exploration without changes.

A session may contain ZERO, ONE, or MULTIPLE distinct tasks. Emit one card per distinct task. If the session pivoted from finishing task A to starting task B, both should be reflected (A as done, B as in_progress).

You will be given:
- Session metadata (display name, harness, state, message count)
- All user prompts verbatim (intent signal)
- Recent assistant turns (current activity / completion signals)
- Recent tool-call activity
- Any cards already linked to this session, with their current state and body

You MUST output by calling the report_classification tool exactly once.

Rules:
- For every existing_card you are given, return a record (don't drop any). If you have no opinion, repeat its current state.
- Use new_tasks only for tasks that are NOT already represented by an existing card.
- title for new tasks: short imperative phrase, ≤60 chars (e.g. "Migrate auth middleware to auth-store").
- summary: one or two sentences on what's happening / what was done / what's blocking.
- state must be one of: in_progress, blocked, done, abandoned.
- If the session is purely chit-chat / Q&A with no real task, set trackable=false and omit existing_cards / new_tasks.

Bridge state ≠ task state. A session can be "completed" while tasks are still in_progress (session ended early), or "idle" with all tasks done. Trust the conversational evidence over bridge state.`

// reportSchema is the JSON Schema the model's reply must conform to. It was
// the input_schema of a forced tool call when this command spoke to
// api.anthropic.com directly; the oneshot bridge path takes the bare schema.
func reportSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"trackable":       map[string]any{"type": "boolean", "description": "False if the session has no real work to track."},
			"session_summary": map[string]any{"type": "string", "description": "1-3 sentence overall description."},
			"existing_cards": map[string]any{
				"type":        "array",
				"description": "One entry per pre-existing card on this session. Required: include every card you were given.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"card_id": map[string]any{"type": "string"},
						"state":   map[string]any{"type": "string", "enum": []string{"in_progress", "blocked", "done", "abandoned"}},
						"summary": map[string]any{"type": "string"},
						"blocker": map[string]any{"type": "string"},
					},
					"required": []string{"card_id", "state", "summary"},
				},
			},
			"new_tasks": map[string]any{
				"type":        "array",
				"description": "Tasks happening in this session that are NOT yet represented by an existing card.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						// The hard cut applyNew enforces. The prompt's "≤60 chars" is
						// advice and is deliberately lower — see the note on systemPrompt.
						"title":   map[string]any{"type": "string", "maxLength": 80},
						"state":   map[string]any{"type": "string", "enum": []string{"in_progress", "blocked", "done", "abandoned"}},
						"summary": map[string]any{"type": "string"},
						"blocker": map[string]any{"type": "string"},
					},
					"required": []string{"title", "state", "summary"},
				},
			},
		},
		"required": []string{"trackable"},
	}
}

// maxOutputTokens bounds the classifier's reply. A full sweep costs roughly 69
// output tokens per existing card. This was 1024 on the direct-API path and
// was measured truncating at 69 cards (session br_1784416900461396845, which
// needed 4,771 tokens); on the subscription there is no per-token bill, so it
// sits high enough that only a pathological session hits it — and one that
// does is still rejected, not silently shortened (see errTruncatedReport).
const maxOutputTokens = 8192

// errTruncatedReport marks a classification that the model did not finish
// writing. It is deliberately distinct from a transport or decode failure: the
// call succeeded, and it must not be retried on identical input. It is the
// package's name for oneshot.ErrTruncated so call sites read in this
// command's vocabulary.
var errTruncatedReport = oneshot.ErrTruncated

// classify runs one schema-forced call through llm-bridge-server's POST
// /oneshot, asking for the configured model-store role; the bridge picks the
// model and the instance.
func classify(ctx context.Context, cfg config, digest string) (*Report, error) {
	var rep Report
	answer, err := oneshot.Call(ctx, oneshot.Config{
		BridgeURL: cfg.BridgeURL,
		ModelRole: cfg.ModelRole,
		Caller:    "kanban-classifier",
		MaxTokens: maxOutputTokens,
		Timeout:   180 * time.Second,
	}, systemPrompt, digest, reportSchema(), &rep)
	if err != nil {
		return nil, err
	}
	log.Printf("[classifier] classified with role %s: %s", cfg.ModelRole, answer)
	return &rep, nil
}

// ============================ Scheduler self-registration ============================

// schedulerJob is the job -register creates.
//
// A function rather than a literal inside the POST because this payload is the
// whole point of -register: it is written once per host and -register never
// rewrites an existing job, so a mistake here needs a hand PATCH to fix.
func schedulerJob(cfg config) schedulerjobs.Job {
	return schedulerjobs.Job{
		Name:        schedulerJobName,
		Description: "Cheap-model classifier that creates and updates kanban cards from bridge sessions.",
		Schedule:    schedulerJobSchedule,
		Command:     cfg.BinaryPath,
		// Was absent, so a fresh host registered this job with the scheduler's
		// 5-minute default while the host it was written on runs 900.
		TimeoutSeconds: schedulerJobTimeoutSeconds,
	}
}

func registerSchedulerJob(cfg config) error {
	return schedulerjobs.RegisterIfAbsent(cfg.SchedulerURL, schedulerJob(cfg))
}

// ============================ misc ============================

// silence unused import on net/url — kept in case we need query encoding later.
var _ = url.PathEscape
