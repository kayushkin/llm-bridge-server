# Session-State Inference Reliability

**Status**: Design / proposal
**Owner surface**: `internal/harness/derivation.go` (the state machine), `PTY-MODE.md`, `SESSION-SIGNALS.md`
**Prior art**: [Herdr](https://github.com/ogulcancelik/herdr) — see `~/repos/inber/docs/comparisons/herdr.md`

## Why this doc exists

Deriving a trustworthy `SessionState` for an agent is a swamp, and we are standing in it with
one boot. `derivation.go` already runs a 13-value state machine off the structured event stream,
but it transitions eagerly, trusts event ordering, can't tell a subagent's terminator from the
main turn's, and goes **completely blind in PTY mode** — where a session "never leaves `running`"
and the activity timestamp is the only liveness signal (`internal/config/config.go`,
`PTYIdleTimeout`). Every consumer that asks "is this agent blocked, done, or waiting for me?"
inherits those gaps: the `awaiting_user` 🔔 "Needs you" inbox (`SESSION-SIGNALS.md`), the herald
`ask` channel, the kanban curators reading session state, and any future team-orchestration
coordinator.

Herdr — a terminal multiplexer that wraps foreign agent CLIs it did not write — has independently
built production-grade answers to exactly this problem, because it *only* has the rendered screen to
go on and still has to say `idle | working | blocked | done | unknown` reliably. Its hard-won rules
are the substance worth importing; the state enum and the two-consumer split are ours to keep.

This is the state-inference **reliability** layer. It is one design applied in two places:

- **llm-bridge-server `derivation.go`** — the canonical state machine every harness routes through,
  including inber sessions (they flow in over the `llm-bridge-inber` bridge as `msg.Event`).
- **inber** — its native emitter (`server/events.go`, `server/api_bridge.go`) publishes only coarse
  `SessionIdle`/`SessionAborted` at endpoints; it should reach parity by consuming the same hardened
  machine rather than re-deriving a weaker one.

## What Herdr got right (the transferable rules)

Not the terminal-scraping specifics — the *discipline* around turning noisy signals into a stable
state. From Herdr's manifest engine, injected hooks, and (most tellingly) its CHANGELOG bug-fix
history:

1. **Monotonic sequence guard.** Every state report carries a `seq` (`report_seq = time.time_ns()`
   in the injected hook). A report older than the last applied one is dropped, so a late-arriving
   event can never override a newer truth.
2. **Serialize lifecycle reports.** Out-of-order plugin events "cannot leave an idle pane marked
   working" (their OpenCode fix). Reports are applied in a single ordered path, not raced.
3. **Settle before transitioning.** Transient message boundaries must not publish a mid-turn `idle`
   (their Pi fix uses "settled events"). Long-running background work is *pinned* so a redraw doesn't
   drop it to `idle` (their Grok fix).
4. **Subagent terminators must not revive the parent.** A `SubagentStop` is explicitly dropped so a
   subagent finishing never marks the main pane `idle` — "never let it revive an idle pane."
5. **Stall timeout, not infinite wait.** After a prompt is submitted, if no state change is observed
   within a bound, report `agent_prompt_stalled` rather than hang forever.
6. **Overlays suppress, they don't drive.** A transcript viewer or model-picker overlay matches a
   rule flagged `skip_state_update` — it prevents a wrong transition instead of forcing one.
7. **Explicit signal precedence.** Signals are ranked (rule `priority`, OSC-title over footer); a
   blocker beats a working signal beats an idle signal, deterministically.
8. **Two channels, because one is never enough.** Passive screen scrape (works for any agent) *plus*
   an active injected hook (authoritative, carries the native session id). Neither alone is reliable.

## Where `derivation.go` stands against each rule

| Rule | Today in `derivation.go` | Gap |
|---|---|---|
| 1. Monotonic seq guard | none — applies events in arrival order | a late/replayed event can wrongly transition |
| 2. Serialized reports | single-mutex `derive()` is ordered *per source*, but hook, harness-stdout, and OTel are distinct sources with no cross-source ordering | cross-source races (e.g. hook `completed` vs a stdout terminator) |
| 3. Settle / pin | transitions fire immediately on each event | a mid-turn `EventResult` (subagent, recap) can flip to `idle` |
| 4. Subagent suppression | `manager.go` notes the hazard; `derive()` does not distinguish subagent-origin terminators | a subagent's `EventResult` ends the parent turn |
| 5. Stall timeout | idle-reaping exists; no "submitted but no progress" signal | a wedged turn looks the same as a quiet idle |
| 6. Overlay suppression | no notion of a suppress-only signal | no clean way to ignore a UI overlay |
| 7. Precedence | one hard-coded rule (approval beats tool_running via `preApprovalState`) | no general ranking across signal sources |
| 8. Two channels | structured events only; `awaiting_user` is a `looksLikeQuestion` string match | **PTY mode has no structured channel at all → no state** |

The permission-override handling (rules 4/7 in spirit) is already good and is the model to
generalize: capture `preApprovalState`, let the higher-priority signal win, restore on drain.

## Proposed work

Three tracks, independently shippable, in priority order.

### A. Harden the structured state machine (`derivation.go`) — HIGH, shared by all harnesses

1. **Sequence guard.** Thread the event's monotonic ordering (row id / OTel timestamp) into
   `derivationState`; refuse a transition whose source event predates the last applied one. Cheap,
   removes a whole class of flicker, benefits every harness at once.
2. **Subagent-terminator suppression.** Tag `EventResult`/`EventError` that originate from a subagent
   (the wire already distinguishes indirect spawns — see `msg/server.go`'s note on subagent spawn
   kinds) and never let one close the parent turn's accumulator or drive `→ idle`. This is Herdr's
   rule 4, one-to-one.
3. **Settle window for turn-end.** Debounce the `→ idle` transition by a short, configurable window;
   a new assistant/tool event inside the window cancels it. Kills the mid-turn-`idle` flip (rule 3)
   without waiting on the derived-pass classifier.
4. **Suppress-only signals (rule 6).** Add a signal class that updates bookkeeping but is barred from
   changing `sessionState` — the generalization the model-picker/transcript-overlay cases need.
5. **Promote `awaiting_user` off the string match.** `looksLikeQuestion` is explicitly a placeholder;
   `SESSION-SIGNALS.md` already specifies the cheap-model derived pass (`source:"derived"`, Haiku on
   turn-ends). Land that and this reliability layer just consumes its verdict. (Cross-reference, not
   new scope here.)

### B. A passive PTY-mode classifier — MEDIUM-HIGH, closes the blind spot

PTY mode emits no structured events, so state must come from what's on screen. The build:

- **Reconstruct the screen, don't scrape bytes.** The existing ~64KB ring buffer (`attachhub.go`,
  `PTY-MODE.md`) holds *raw* PTY bytes — cursor moves, redraws, alt-screen switches, scroll regions.
  Region rules like `after_last_horizontal_rule` or `prompt_box_body` are meaningless against that
  stream; they need the *rendered* cell grid. So the classifier runs against a **headless VT screen
  model** fed the same byte stream — a server-side terminal emulator kept purely to answer "what is
  on screen now," never rendered to a human. This is the one place Herdr is right that you need an
  emulator server-side (see "Terminal emulation" below for *which* one).
- **Per-harness rule table** (TOML, versioned, hot-updatable), matching named regions of that
  reconstructed screen against `contains`/`regex`/`priority`, producing `working | idle | blocked` —
  the design is proven in Herdr's `detect/manifests/*.toml`; we adapt the *shape*, not the file.
- **Emit through the same `derive()` reliability layer** (rules 1–4, 6–7 apply identically) so PTY
  and structured sessions produce one consistent `SessionState`. The classifier is a *signal source*;
  the state machine stays the single arbiter.
- Result: the "Needs you" inbox, herald `ask`, and kanban curators finally see a blocked/idle PTY
  agent instead of a permanent `running`.

Scope guard: a rule table is a maintenance tax (Herdr's CHANGELOG is largely per-agent quirk fixes).
Ship it only for harnesses that actually run in PTY mode, and treat `unknown` as a first-class,
non-acting state — never guess `idle`.

#### Terminal emulation: two surfaces, two answers

Herdr vendors `libghostty-vt` (Ghostty's Zig emulator core) and links it into its binary because it
is a **native** multiplexer that *renders terminal cells directly* — kitty graphics, grapheme-cluster
cells, DEC private modes. That informs a natural question: should we adopt it? Split it:

- **Display (what the human sees): keep xterm.js. Don't adopt libghostty.** Our PTY path already
  passes raw bytes through to xterm.js in the browser and never renders server-side — which is exactly
  the "layers are transparent" rule. libghostty-vt is a native renderer with no browser target;
  xterm.js is a mature, correct browser emulator and the right tool for a web gateway. Different
  delivery surface, not a better option.
- **Classifier (what track B reads): yes to a headless emulator.** The classifier genuinely needs a
  server-side screen model (above). The objection to libghostty was never "Zig/Rust is bad" — it was
  **build coupling**: linking it via cgo drags a foreign toolchain into the Go module's build, against
  the "Go is primary," clean-clone build-guard, and OSS-prep constraints. That objection dissolves if
  the emulator runs **out-of-process** rather than linked (see the sidecar option below), which also
  reopens the far more reliable non-Go emulators. So the real axis is not "Go vs not-Go" but
  **linked-into-the-Go-build vs isolated subprocess**. Adopt Herdr's *insight* (rules match a
  reconstructed screen); the emulator choice is a separate, swappable decision.

#### Prototype findings (spike, `~/dev-spikes/vt-state-classifier/`)

A runnable spike validated the approach and compared the two candidate pure-Go emulators against a
crafted Claude-style transcript (OSC title + cursor moves + a permission prompt that is later cleared
and redrawn to an idle prompt box). Results:

- **The correctness premise holds.** On the transcript that ends idle, **raw-byte scraping classifies
  `blocked`** — the cleared "Do you want to proceed?" bytes are still in the buffer — while the
  **reconstructed screen classifies `idle`**, correctly. This is the concrete reason track B must run
  rules against a screen model, not the ring buffer.
- **Priority rules work.** In the genuinely-blocked frame the permission-prompt rule (priority 900)
  correctly wins over the working-spinner OSC title (priority 300) — blocker beats working, as intended.
- **Library choice — test beat reputation.** Both reconstruct the screen grid correctly. But
  `charmbracelet/x/vt` (actively maintained, richer callbacks, heavier deps) **mangled the multibyte
  OSC title**, delivering `"\xe2"` — the first byte of `✳` — instead of `"✳ Claude"` through its
  `Callbacks.Title`. `hinshun/vt10x` (zero extra deps, built-in `.Title()`, but unmaintained since
  2022) returned the title **correctly**. Since the working/idle title rules are literally braille /
  `✳` multibyte characters, that title bug is disqualifying *for the title-rule path* at x/vt's current
  commit.
#### Emulator options (maintenance + fit, verified against registries 2026-07-27)

`hinshun/vt10x` is correct today but **unmaintained since 2022** — a liability for a service. The full
field:

| Emulator | Lang | Maintained | Downloads | Screen grid | OSC title | Fit / cost |
|---|---|---|---|---|---|---|
| `hinshun/vt10x` | Go | **No (2022)** | — | ✓ | ✓ (correct in spike) | zero-dep, in-process; abandonment risk |
| `charmbracelet/x/vt` | Go | Yes (pub. today) | — | ✓ | ✗ **mangles multibyte** (spike) | in-process, pure-Go; untagged pre-release, self-flags typecheck issues |
| `asciinema/avt` | Rust | **Yes (v0.18, 2026-05)** | 224k | ✓ (primary/alt) | ✗ (discards OSC) | **purpose-built** for headless screen reconstruction from a byte stream; lean |
| `alacritty_terminal` | Rust | **Yes (v0.26, 2026-04)** | 899k | ✓ | ✓ (tracks `self.title`) | battle-tested (Alacritty/Zed); heavier, real-terminal-shaped API |
| `wezterm termwiz` | Rust | Yes (2025-03) | 15M | ✓ (`Surface`) | ✓ | full-featured, heavy |
| libghostty-vt | Zig | Yes (Ghostty) | — | ✓ | ✓ | rendering-grade; what Herdr uses; cgo/Zig build cost |

Two facts from the spike, not reputation: x/vt (maintained) **broke** the multibyte OSC title
(`"\xe2"` for `✳`); hinshun (abandoned) got it right. So "maintained" alone doesn't decide it — the
title path specifically must be tested on whichever emulator wins.

#### Recommendation: run the emulator as a subprocess sidecar, cored on `asciinema/avt`

The architecture already fits. **Every harness bridge in this ecosystem is a subprocess speaking
NDJSON on stdin/stdout** (`llm-bridge-claudecode` et al.). A **PTY-state sidecar** is the identical
shape: it reads the PTY byte stream (tee'd off `attachhub.Broadcast`), maintains the emulator, and
emits screen snapshots — `{screen, title, cursor, alt_screen, seq}` — as NDJSON. Consequences:

- **Reliability goes up, build stays pure-Go.** The sidecar is a separately-built binary (like the
  bridges), so the maintained-but-Rust `avt`/`alacritty_terminal` never touch the Go module's
  clean-clone build. This is the whole reason "go outside Go" is nearly free here — no cgo.
- **Core it on `avt`** — purpose-built for exactly this (reconstruct screen from a stream, headless),
  maintained, lean. Its one gap, discarding the OSC title, is trivially closed *in the sidecar*: the
  sidecar owns the byte stream, so it sniffs OSC 0/2 itself (a few lines) and attaches the title to
  the snapshot — turning avt's limitation into a non-issue. If we'd rather not hand-roll that,
  `alacritty_terminal` tracks the title natively at the cost of a heavier API.
- **Emulator stays swappable** behind the NDJSON snapshot contract — avt ↔ alacritty_terminal ↔ even
  libghostty-vt later — with zero change to the Go rule engine.
- Go keeps everything that isn't emulation: the manifest rule tables, the `derive()` reliability layer,
  and the `SessionState` arbitration.

**Fallback if a sidecar isn't wanted:** stay pure-Go/in-process with `charmbracelet/x/vt` (maintained),
and either file an upstream fix for the multibyte-title bug or read the title from a screen region
instead of the OSC callback. Accept its pre-1.0 churn. `hinshun/vt10x` drops to a throwaway
MVP-only choice given the abandonment risk — not the thing a service depends on.

**Not yet spiked:** the Rust path is validated on maintenance/API/purpose-fit but **not** run here (no
Rust toolchain on this host). Before committing, stand up a ~30-line `avt` sidecar and replay a real
captured Claude/Codex PTY transcript through it — the same bar the Go libraries cleared above.

### C. inber native-emitter parity — MEDIUM

inber's `server/events.go` publishes coarse endpoint states. Rather than grow a second, weaker state
machine, route inber's native path through the same hardened `derive()` (it already speaks
`msg.Event`), so an inber session gets identical state whether observed directly or through the
bridge. This is the `CONTEXT-MIGRATION.md` shared-library pattern applied to state derivation.

## Non-goals

- Not adopting Herdr's `libghostty-vt` or rendering terminals server-side for display — xterm.js stays
  the display emulator. Track B's headless VT screen model is a pure-Go parser used only to feed the
  classifier, never rendered to a human.
- Not a new state enum. The 13-value `SessionState` stays canonical; this hardens how we *reach* each
  value.
- Not the signal-delivery UX — that's `SESSION-SIGNALS.md`. This doc produces the trustworthy state
  that doc's inbox renders.

## Cross-references

- `~/repos/inber/docs/comparisons/herdr.md` — the source analysis (two-channel inference, the
  reliability rules, the CHANGELOG evidence).
- `SESSION-SIGNALS.md` — the `awaiting_user` derived-pass classifier and the "Needs you" inbox that
  consume this state.
- `PTY-MODE.md` — the ring buffer track B reuses, and the current "never leaves `running`" gap.
- `internal/harness/derivation.go` — the state machine to harden; `msg/provider.go` — the
  `SessionState` enum.
