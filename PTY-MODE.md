# PTY Mode — Design Spec

Status: **draft (2026-04-27)** — pre-implementation. Open questions tagged `[OPEN]`.

## Why

Today every session is "events mode": the harness subprocess speaks structured JSON and llm-bridge-server emits a normalized `msg.Event` stream. That's the right abstraction for tools that build their own UI, but it's the wrong abstraction for tools that already give the user a terminal — `claude-squad`, vibe-kanban-style runners, ssh-attach setups, anything that wants the unmodified upstream CLI's TUI.

User intent (from todo `e2627af8`):

> "if there is a cli available it would be nice to be able to spawn a session as either the normal mode or as a cli window instead."

PTY mode lets the caller pick at session-spawn time:
- `events` (default, current behavior) — JSON event stream over SSE.
- `pty` — a real pseudoterminal running the upstream CLI. Caller attaches via WebSocket.

## Non-goals (this spec)

- **Dual mode** (same session emitting both pty bytes and structured events). Useful for `claude-squad`, but designing the demuxing is its own can of worms — defer to a follow-up.
- **PTY support for HTTP-based harnesses** (`hermes`, `dexto`, `inber`). They have no CLI; PTY would have to synthesize one. Out of scope.
- **Per-tab multiplexing inside one pty** (`tmux`-on-the-wire). The pty hosts whatever the upstream CLI runs; if that's already a tmux session, fine — but llm-bridge-server doesn't add a layer.

## Surface

### Capability declaration

`HarnessBridge` (`~/repos/llm-bridge/bridge/bridge.go:40`) gains an optional capability:

```go
// SupportsPTY returns true if this harness can run inside a pseudoterminal.
// CLI-based harnesses (claudecode, codex when shelling out) return true.
// HTTP-based harnesses (hermes, dexto, inber) return false.
type PTYCapableHarness interface {
    HarnessBridge
    SupportsPTY() bool
}
```

Using a separate optional interface (rather than adding `SupportsPTY()` to `HarnessBridge` directly) avoids forcing every existing harness to recompile for a feature most won't implement. `llm-bridge-server` checks via type assertion.

This bit is also surfaced over HTTP at `GET /harnesses/{name}/capabilities` — it's already the discovery endpoint (server.go:95), so adding a `pty: true|false` field there is the natural place.

### Session creation

`POST /sessions` accepts a new optional field:

```json
{
  "harness": "claudecode",
  "instance": "...",
  "prompt": "...",
  "mode": "events" | "pty"
}
```

- `mode` defaults to `events` for backwards compatibility.
- `mode: "pty"` on a harness that doesn't support pty returns `400 Bad Request` with `error.code = "pty_unsupported"`.
- Initial pty size is taken from optional fields `pty_rows` / `pty_cols`. If absent, default to `24x80`. Caller almost always wants to resize as soon as it attaches anyway.
- Session record stores the mode so `GET /sessions/{id}` reports it back and `Last-Event-ID` replay logic can branch on it.

### Attach endpoint

```
GET /sessions/{id}/attach?token=<attach_token>
Upgrade: websocket
```

Bidirectional binary WebSocket. Auth is a per-session attach token (32 hex chars, 128 bits of entropy) minted at hub construction and returned in the `POST /sessions` response as `attach_token` for pty sessions. Comparison is constant-time; a missing or wrong token returns `401 Unauthorized` before the upgrade. The token lives only on the in-memory `AttachHub` — when the pty exits and the hub is dropped, the token is unreachable and no further attaches succeed against that session.

Browser-facing path: dash/llmux fetch the token from the create-session response, hold it in memory, and pass it as the `?token=` query parameter on the WS URL (`Authorization` headers can't be set on WebSocket handshakes from `WebSocket` in browsers, hence the query string).

**Wire format** — every WebSocket frame is a length-prefixed control envelope so we can interleave terminal bytes with control messages (resize, signal, etc.). Concrete shape `[OPEN]`:

- Option A: framed JSON with `{"type":"data"|"resize"|"signal"|"close","payload":...}`. Easy to extend, modest overhead.
- Option B: first byte = type, rest = payload. SSH-style. Low overhead, harder to extend.
- Option C: text frames are control JSON, binary frames are raw pty bytes. Idiomatic for browsers using `WebSocket.binaryType`.

**Recommended: option C.** Browsers can pipe binary frames straight into xterm.js, and JSON control is human-debuggable in DevTools.

Control messages (sent as text frames):

```json
{"type":"resize","rows":40,"cols":120}
{"type":"signal","signal":"SIGINT"}
{"type":"close"}
```

Server → client only:

```json
{"type":"role","role":"writer"}    // or "reader" — sent once, immediately after attach, before the ring-buffer replay
{"type":"exit","code":0,"signal":""}
```

Clients should treat the `role` frame as the signal that the attach is live and decide whether to wire up keystroke forwarding (writers) or render a "read-only" affordance (readers).

### Multiplexing

Multiple clients MAY attach to the same pty session simultaneously:

- **First attacher is the writer.** It owns stdin and resize.
- **Subsequent attachers are readers** — they receive output but their input frames are dropped. (Could later be promoted via a `{"type":"steal"}` message; deferred.)
- **Resize policy**: writer's last resize wins. If writer detaches, server keeps the last size; next attacher becomes writer and sends its own resize.
- **Late attach**: server keeps a small ring buffer (e.g. 64KB) of recent pty output and replays it to a fresh attacher so they see the current screen state. xterm.js + a clear-and-redraw on resize handles the rest.

Single-writer-multi-reader matches the user intent ("read-only observers + one writer") and avoids the keystroke-race horror of multi-writer pty.

## Implementation

### Server-side

The current spawn path goes through `harness.StartProcess()` (`process.go:114`), which uses raw `cmd.StdinPipe()` / `cmd.StdoutPipe()` for the JSON-events protocol. PTY mode adds a parallel path:

```
StartProcess(...)              — events mode (existing)
StartProcessPTY(...)           — pty mode (new)
```

Both return a `HarnessProcess` (`process.go:24`); the existing `Events()` channel is empty for pty sessions, and a new `PTYStreams()` method exposes the pty fd pair.

Dependency: `github.com/creack/pty` (BSD-3, mature, used by `kubectl exec`). Pulls in nothing surprising.

The attach endpoint registers a per-session `attachHub` keyed by session ID:

- `attachHub.AddWriter(conn)` / `AddReader(conn)` — bidirectional copy goroutines.
- `attachHub.Resize(rows, cols)` — calls `pty.Setsize(fd, ...)`.
- `attachHub.Broadcast(buf)` — fan-out to all readers; also writes to the ring buffer.
- On pty EOF or process exit, hub sends `{"type":"exit"}` to all clients and closes connections.

### Harness-side (CLI harnesses)

The harness needs a way to run "interactively" rather than its current `--input-format stream-json --output-format stream-json` invocation.

For `llm-bridge-claudecode`, the upstream `claude` CLI already runs interactively when given no special flags. The harness's job for pty mode reduces to:

1. Pick the binary path (already does this).
2. Build an `exec.Cmd` with no `--input-format` / `--output-format` flags.
3. Hand the cmd back to the server, which wires it into a pty.

Meaning: most of the pty plumbing is server-side; harness work is small.

For `llm-bridge-codex`, current implementation talks to Codex's AppServer over WebSocket — there is no CLI subprocess. To support pty mode, the harness would shell out to the actual `codex` CLI instead. That's a meaningful change; mark codex pty support as a separate child todo.

### What changes in for-integrators.md

Add a section: "Choosing a session mode (events vs pty)". Frame it as:

- "Building your own UI? Use events mode and consume `msg.Event` over SSE. This is the default and what most integrations want."
- "Embedding the upstream CLI's TUI directly? Use pty mode and attach over WebSocket. Less normalization, but the user sees exactly what they would see running the CLI by hand."

## Phasing

Children are scoped so each can be finished by an unattended session. Order is rough — 1 and 2 are prereqs for everything else.

1. **Bridge interface + capability discovery.** Add `PTYCapableHarness` interface in `~/repos/llm-bridge/bridge/`. Surface in `/harnesses/{name}/capabilities`. No actual pty work yet — just the bit. Update generated types (TS + Python). Conformance tests should still pass.
2. **Server-side pty spawn + attach hub (claudecode only).** Add `StartProcessPTY` in `internal/harness/process.go`. Add `attachHub`. Add `GET /sessions/{id}/attach` WebSocket route. Wire `mode: "pty"` through `POST /sessions`. Test with `llm-bridge-claudecode` running interactively.
3. **Resize + multi-reader.** Make resize work end-to-end (control message → `pty.Setsize`). Add ring buffer + replay for late attachers. Single-writer enforcement.
4. **for-integrators.md update.** Document the new mode. Add a tiny end-to-end example (curl + `wscat` or similar).
5. **Codex pty mode.** Switch `llm-bridge-codex` to also support pty by shelling out to the `codex` CLI. Separate from the codex AppServer path — both modes coexist.
6. **Integration test.** Spawn claude in pty, attach via WebSocket, send a few keystrokes, assert bytes round-trip. Live test (real `claude` binary) lives behind a build tag because it requires creds.

## Open questions

- `[OPEN]` Wire format — option C (text=JSON, binary=pty bytes) is the recommendation but not yet ratified. Can be revisited in child #2.
- `[OPEN]` Should pty mode be allowed on remote/SSH harnesses? In principle yes (allocate pty on the remote side), but it adds a forwarding hop. Defer; treat as `false` from `SupportsPTY()` on remote-spawned sessions in v1.
- ~~`[OPEN]` Auth — current sessions have no per-session token.~~ Resolved 2026-05-10: per-session attach token (128-bit hex, in-memory on the AttachHub, returned in the `POST /sessions` response, passed as `?token=` on the WS upgrade). Chosen over a shared admin secret because the token dies with the pty — leaking one session's token can't be replayed against a different session, and there's no env-var rotation step. See `internal/harness/attachhub.go` `mintAttachToken` and `internal/server/attach.go` token check.
- ~~`[OPEN]` Buffer size for late-attach replay — 64KB is a guess. Make it configurable; tune later.~~ Resolved in child 3: `LLMBRIDGE_PTY_RING_BUFFER_BYTES` env var, defaults to 65536.

## Out of scope, but worth noting

- **`claude-squad` integration** explicitly wants the dual-mode (pty bytes + structured events on the same session) so it can show a TUI but also drive its own UI from events. That's the stretch goal in the original todo, deliberately deferred — it's a clean follow-up once basic pty works.
- **Recording / playback.** A pty stream is trivially recordable (asciinema format). Not needed for v1, but the attach hub is the obvious place to plumb it in later.
