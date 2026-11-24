package runnerprotocol

import (
	"encoding/json"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ──────────────────────────────────────────────────────────────────────────────
// Runner protocol
//
// The runner protocol is the wire format between llm-bridge-server and an
// llm-bridge-runner daemon deployed on a remote machine. Runners dial the
// server over WebSocket (outbound, NAT-traversal-friendly), authenticate,
// then accept Spawn requests for harness subprocesses. The runner forks the
// requested harness binary locally and pipes its stdio over the same WS
// connection, multiplexed by SessionID.
//
// Layering: the runner is a transparent transport — it does not parse
// harness stdout, it just ships lines verbatim. The server's existing
// NDJSON event pipeline runs unchanged on the received bytes.
// ──────────────────────────────────────────────────────────────────────────────

// RunnerMessageType discriminates the polymorphic RunnerMessage envelope.
type RunnerMessageType string

const (
	// Connection-level (no SessionID).
	RunnerMsgHello   RunnerMessageType = "hello"   // runner → server, sent immediately after connect
	RunnerMsgWelcome RunnerMessageType = "welcome" // server → runner, ack
	RunnerMsgError   RunnerMessageType = "error"   // either direction, fatal error before close
	RunnerMsgPing    RunnerMessageType = "ping"    // app-level keepalive (in addition to WS pings)
	RunnerMsgPong    RunnerMessageType = "pong"

	// Per-session (SessionID required).
	RunnerMsgSpawn  RunnerMessageType = "spawn"  // server → runner
	RunnerMsgStdin  RunnerMessageType = "stdin"  // server → runner
	RunnerMsgSignal RunnerMessageType = "signal" // server → runner
	RunnerMsgStdout RunnerMessageType = "stdout" // runner → server
	RunnerMsgStderr RunnerMessageType = "stderr" // runner → server
	RunnerMsgExit   RunnerMessageType = "exit"   // runner → server
	// RunnerMsgAck tells the runner the server has stored a session's output
	// up to a sequence number, so the runner may drop it. Server → runner,
	// only for a spawn that carries a SpawnID.
	RunnerMsgAck RunnerMessageType = "ack"

	// Seeding (no SessionID; carry context/skill files from the bridge to the runner).
	RunnerMsgSeedDelta    RunnerMessageType = "seed_delta"    // server → runner, single-file delta (push)
	RunnerMsgSeedSnapshot RunnerMessageType = "seed_snapshot" // server → runner, full manifest snapshot (after scan)

	// RunnerMsgClaudeOAuthRecovery starts or supplies the one-time code for a
	// Claude subscription login on the runner's own machine. It deliberately
	// lives outside a harness session: an expired Claude login cannot start a
	// Claude Code process to repair itself.
	RunnerMsgClaudeOAuthRecovery RunnerMessageType = "claude_oauth_recovery"

	// RunnerMsgMachineStats carries a reading of the runner's machine: CPU,
	// memory, drives, batteries, uptime. Runner → server, once a minute.
	RunnerMsgMachineStats RunnerMessageType = "machine_stats"
)

// RunnerMessage is the envelope for every frame exchanged on the runner WS.
// Exactly one payload field is populated, matching Type.
type RunnerMessage struct {
	Type      RunnerMessageType `json:"type"`
	SessionID string            `json:"session_id,omitempty"`

	Hello   *RunnerHello   `json:"hello,omitempty"`
	Welcome *RunnerWelcome `json:"welcome,omitempty"`
	Err     *RunnerError   `json:"error,omitempty"`

	Spawn  *RunnerSpawn  `json:"spawn,omitempty"`
	Stdin  *RunnerStdin  `json:"stdin,omitempty"`
	Stdout *RunnerStdout `json:"stdout,omitempty"`
	Stderr *RunnerStderr `json:"stderr,omitempty"`
	Signal *RunnerSignal `json:"signal,omitempty"`
	Exit   *RunnerExit   `json:"exit,omitempty"`
	Ack    *RunnerAck    `json:"ack,omitempty"`

	SeedDelta           *RunnerSeedDelta           `json:"seed_delta,omitempty"`
	SeedSnapshot        *RunnerSeedSnapshot        `json:"seed_snapshot,omitempty"`
	ClaudeOAuthRecovery *RunnerClaudeOAuthRecovery `json:"claude_oauth_recovery,omitempty"`
	MachineStats        *msg.RunnerMachineStats    `json:"machine_stats,omitempty"`
	DiskUsage           *RunnerDiskUsage           `json:"disk_usage,omitempty"`
}

// RunnerClaudeOAuthRecovery carries one step of a Claude Code subscription
// login. Start asks the runner to launch `claude auth login --claudeai`;
// Ready returns the authorization URL; Code gives the runner the one-time
// code; Succeeded and Failed settle the request. RecoveryID correlates all
// messages and is minted by the bridge, never by a runner.
type RunnerClaudeOAuthRecovery struct {
	RecoveryID       string `json:"recovery_id"`
	Action           string `json:"action"` // start, ready, code, succeeded, failed
	AuthorizationURL string `json:"authorization_url,omitempty"`
	Code             string `json:"code,omitempty"`
	Message          string `json:"message,omitempty"`
}

// ProtocolVersionSequencedOutput is the first version in which a runner
// numbers a session's output, keeps it until the server acknowledges it, and
// can hold a session's process while the server is away. A side that leaves
// ProtocolVersion out is version 1, where output is neither numbered nor kept
// and a runner ends every process when the connection drops.
//
// A server takes the current version and the one before it; a runner on
// anything older is refused and told to update.
const (
	ProtocolVersionSequencedOutput = 2
	CurrentProtocolVersion         = ProtocolVersionSequencedOutput
)

// RunnerHello is sent by the runner immediately after the WS upgrade.
// Authentication is the Authorization: Bearer header on the upgrade
// request — the server resolves the token to a Machine row by hash.
// The Hello payload is environmental detail the server records for
// display + binary-serving (hostname, OS, etc.); it carries no identity
// claims of its own. The machine label is server-side state, returned
// to the runner via Welcome.MachineName.
type RunnerHello struct {
	Hostname           string                 `json:"hostname"`            // os.Hostname()
	OS                 string                 `json:"os"`                  // GOOS
	Arch               string                 `json:"arch"`                // GOARCH
	User               string                 `json:"user"`                // os/user.Current().Username
	WorkingDir         string                 `json:"working_dir"`         // default cwd for spawned subprocesses
	AvailableHarnesses []msg.HarnessAvailable `json:"available_harnesses"` // detected harness binaries on this machine
	RunnerVersion      string                 `json:"runner_version"`      // build-stamped version
	// ComputerUse is set when this runner installed a computer-use tool it
	// can hand to a session; nil when it has none (no screen it can reach,
	// or the install failed, which the runner logs).
	ComputerUse *ComputerUseTool `json:"computer_use,omitempty"`
	// DiskUsage is set when this runner installed a disk-usage helper, so
	// the Machines page can show what fills its drives and delete from them
	// (disk_usage.go). Only a runner in WSL has one so far.
	DiskUsage *msg.DiskUsageTool `json:"disk_usage,omitempty"`
	// ProtocolVersion is the version this runner speaks; absent means 1.
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// HeldSessions are the spawns this runner kept while it had no
	// connection: processes still running, and ones that ended with output the
	// server has not acknowledged. The server answers each in its Welcome,
	// under Adopt or Release.
	HeldSessions []RunnerHeldSession `json:"held_sessions,omitempty"`
}

// RunnerHeldSession is one spawn a runner kept across a lost connection.
type RunnerHeldSession struct {
	SessionID string `json:"session_id"`
	SpawnID   string `json:"spawn_id"`
	// LastSequence is the number of the newest output entry the runner has
	// for this spawn.
	LastSequence uint64 `json:"last_sequence"`
	// Exited says the process has ended; its exit is the last entry.
	Exited bool `json:"exited,omitempty"`
}

// RunnerAdoptSession tells a runner the server takes a held spawn back, and
// from where to send its output again.
type RunnerAdoptSession struct {
	SessionID string `json:"session_id"`
	SpawnID   string `json:"spawn_id"`
	// ResumeAfterSequence is the last entry the server has stored. The runner
	// sends everything after it, and may drop everything up to it.
	ResumeAfterSequence uint64 `json:"resume_after_sequence"`
}

// ComputerUseTool is a runner's computer-use MCP server: the command a
// harness on that machine runs to get the "computer" tool. On WSL it is the
// Windows build, under the Windows user's local app data, run through WSL's
// interop.
type ComputerUseTool struct {
	Command string `json:"command"`           // absolute path on the runner's machine
	Version string `json:"version,omitempty"` // the tool's build stamp
}

// ComputerUseMCPServerName is the name a session's MCP config gives the
// computer-use server, so its tool is mcp__llm-bridge-computer-use__computer.
// Not "computer-use": Claude Code reserves that name and drops a server
// called it with only a debug-log warning (2.1.300, measured 2026-10-14).
const ComputerUseMCPServerName = "llm-bridge-computer-use"

// RunnerWelcome is the server's ack of a successful Hello.
//
// MachineName is the canonical name on the server side. Runners with a
// stale local label (e.g. machine renamed in the UI after enrollment)
// should overwrite their saved config from this value so subsequent
// connects don't keep reporting the old name.
type RunnerWelcome struct {
	MachineID        string    `json:"machine_id"`   // server-assigned, stable across reconnects
	MachineName      string    `json:"machine_name"` // authoritative current name
	ServerVersion    string    `json:"server_version"`
	PingIntervalSecs int       `json:"ping_interval_secs"` // app-level ping cadence the runner should follow
	AcceptedAt       time.Time `json:"accepted_at"`
	// ProtocolVersion is the version this server speaks; absent means 1.
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// HoldSessionsWithoutServer says the runner should keep its processes
	// running when this connection drops, instead of ending them, and keep
	// their output for the server's return. Off unless the server's operator
	// switched it on.
	HoldSessionsWithoutServer bool `json:"hold_sessions_without_server,omitempty"`
	// HoldWithoutServerSeconds is how long a held process may go on working
	// with no server before the runner interrupts it. The process is kept.
	HoldWithoutServerSeconds int `json:"hold_without_server_seconds,omitempty"`
	// Adopt lists the held spawns the server takes back. Release lists the
	// sessions whose held spawns it does not want: the runner ends them. A
	// held spawn named in neither is ended too.
	Adopt   []RunnerAdoptSession `json:"adopt,omitempty"`
	Release []string             `json:"release,omitempty"`
}

// RunnerError reports a fatal error before the connection is closed.
// Code is a stable machine-readable token; Message is human prose.
type RunnerError struct {
	Code    string `json:"code"` // "auth_failed", "duplicate_machine", "internal", …
	Message string `json:"message"`
}

// RunnerSpawn asks the runner to fork a harness subprocess for a session.
// StartParams is the same payload the local subprocess transport sends to
// the harness as the "start" JSON-RPC method — the runner writes it
// verbatim to the subprocess's stdin as the first line.
//
// Provision lists long-lived backend services the wrapper expects to be
// running on the same host (e.g. inber-server for the inber wrapper).
// The runner ensures each is installed and running before the fork —
// idempotently, so subsequent spawns don't restart a healthy backend.
// This is how "create an instance on a remote machine" actually
// deploys the harness's full stack to that machine.
type RunnerSpawn struct {
	Harness     msg.Harness      `json:"harness"`
	BinaryPath  string           `json:"binary_path,omitempty"` // optional override; runner falls back to PATH lookup
	WorkingDir  string           `json:"working_dir,omitempty"` // overrides Hello.WorkingDir for this session
	StartParams json.RawMessage  `json:"start_params"`          // verbatim JSON-RPC params for "start"
	Env         []string         `json:"env,omitempty"`         // extra env vars for the wrapper, "KEY=VALUE" form
	Provision   []HarnessService `json:"provision,omitempty"`   // backend services the runner should ensure are running
	// ComputerUse says the session was given the runner's computer-use tool,
	// so the runner makes the tool current before starting the harness.
	ComputerUse bool `json:"computer_use,omitempty"`
	// SpawnID names this one start of the session's process. When set, the
	// runner numbers the process's output from 1, keeps each entry until the
	// server acknowledges it, and stamps every Stdout and the Exit with the id
	// and a sequence number. The server makes the id; a runner never does.
	// Empty means the old way: output is sent once and not kept.
	SpawnID string `json:"spawn_id,omitempty"`
}

// HarnessService describes one long-lived backend daemon a harness needs
// running on the runner host. The bridge populates this from its own
// per-harness deployment manifest, with credential values already
// resolved into Env so the runner doesn't need bridge-side state.
type HarnessService struct {
	Name       string   `json:"name"`                  // unique key, e.g. "inber-server"; runners dedupe by this across spawns
	BinaryURL  string   `json:"binary_url"`            // canonical /api/runner/binary?name=…&os=…&arch=… URL
	BinaryPath string   `json:"binary_path,omitempty"` // absolute path on the runner; defaults to ~/.local/bin/<name>
	Args       []string `json:"args,omitempty"`        // command-line args passed to the binary
	Env        []string `json:"env,omitempty"`         // env vars (KEY=VALUE), including resolved credential values
	HealthURL  string   `json:"health_url"`            // GET probe to wait for after start (e.g. http://localhost:8200/health)
	Stdout     string   `json:"stdout,omitempty"`      // optional log file path; defaults to ~/.cache/llm-bridge-runner/<name>.log
}

// RunnerStdin carries one line (no trailing newline) of NDJSON to be
// written to the subprocess stdin, with a newline appended by the runner.
type RunnerStdin struct {
	Data string `json:"data"`
}

// RunnerStdout carries one line (no trailing newline) read from the
// subprocess stdout. The server's existing pipeline parses it as msg.Event.
type RunnerStdout struct {
	Data string `json:"data"`
	// SpawnID and Sequence are set for a spawn that carries a SpawnID.
	SpawnID  string `json:"spawn_id,omitempty"`
	Sequence uint64 `json:"sequence,omitempty"`
	// LineTooLong says a line longer than the runner reads stood here. Data
	// is empty; the entry exists so the sequence has no gap.
	LineTooLong bool `json:"line_too_long,omitempty"`
}

// RunnerStderr carries one line of subprocess stderr, forwarded for
// server-side logging. Not parsed.
type RunnerStderr struct {
	Data string `json:"data"`
}

// RunnerSignalType is the kind of signal to deliver to a subprocess.
type RunnerSignalType string

const (
	RunnerSignalInterrupt RunnerSignalType = "interrupt" // SIGINT, pause/cancel current turn
	RunnerSignalKill      RunnerSignalType = "kill"      // SIGKILL, terminate immediately
)

// RunnerSignal asks the runner to deliver a signal to a session's subprocess.
type RunnerSignal struct {
	Signal RunnerSignalType `json:"signal"`
}

// RunnerExit reports that a session's subprocess has exited.
type RunnerExit struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"` // non-empty if the process couldn't be reaped cleanly
	// SpawnID and Sequence are set for a spawn that carries a SpawnID. The
	// exit is the spawn's last entry, numbered after its last line.
	SpawnID  string `json:"spawn_id,omitempty"`
	Sequence uint64 `json:"sequence,omitempty"`
}

// RunnerAck tells the runner that the server has stored every entry of a
// spawn up to and including Sequence.
type RunnerAck struct {
	SpawnID  string `json:"spawn_id"`
	Sequence uint64 `json:"sequence"`
}

// SeedSource discriminates which store provided a seeded file. Lets the
// runner reconcile each provider against its own endpoint set without the
// bridge needing per-source message types.
type SeedSource string

const (
	SeedSourceAgentStore SeedSource = "agent-store" // CLAUDE.md, AGENTS.md, memory, subagents, …
	SeedSourceSkillStore SeedSource = "skill-store" // ~/.claude/skills/**
)

// SeedOp is the kind of change the runner should apply to a seeded path.
type SeedOp string

const (
	SeedOpUpsert SeedOp = "upsert" // create or overwrite (drift-checked)
	SeedOpDelete SeedOp = "delete" // remove (only if locally still equals last-pushed sha)
)

// RunnerSeedDelta is a single-file change pushed to a runner immediately
// after the bridge records a new authoritative version. The runner must:
//
//  1. Compute the sha256 of the local file at InstallPath, if any.
//  2. If the local sha differs from ExpectedPriorSHA (drift), POST the local
//     bytes to the source's /seed/drift endpoint *first*. Only if that call
//     succeeds may it proceed to step 3.
//  3. GET the new content from the source's version endpoint and write it
//     atomically (tmp + rename).
//
// ExpectedPriorSHA empty = the runner has never seen this file (first push).
// In that case any pre-existing content at InstallPath is treated as drift.
type RunnerSeedDelta struct {
	Source           SeedSource `json:"source"`
	Op               SeedOp     `json:"op"`
	TrackedID        int64      `json:"tracked_id"`                   // ID in the source store
	VersionID        int64      `json:"version_id,omitempty"`         // version to fetch on upsert
	InstallPath      string     `json:"install_path"`                 // $HOME-relative
	NewSHA           string     `json:"new_sha,omitempty"`            // sha of the new content (upsert only)
	ExpectedPriorSHA string     `json:"expected_prior_sha,omitempty"` // last-pushed sha bridge thinks runner has
}

// RunnerSeedSnapshot tells the runner to do a full manifest reconcile against
// the named source. Sent on connect and after a bridge-side scan completes.
// The actual manifest is fetched via HTTP — this message is just the trigger
// + opportunity to bundle metadata (e.g. ticket count) for telemetry.
type RunnerSeedSnapshot struct {
	Source     SeedSource `json:"source"`
	EntryCount int        `json:"entry_count,omitempty"` // bridge's count, for sanity logging
	Reason     string     `json:"reason,omitempty"`      // "connect", "scan", "manual", …
}
