# What the conformance matrix can and cannot say

Audit of `conformance/runner.go` at `2d60e11`, done 2026-08-07. It answers one question for
every feature in `AllFeatures`:

> **Can a harness ever be marked failed for the contract this feature names?**

The question matters because the matrix is read as capability coverage. For most of its rows it
is not. At `2d60e11` only three of twenty-two features could fail a harness for the thing they
name; `discover` has since joined them (finding 3). On the other eighteen, a harness that simply
does not implement the feature is filed as "not applicable" and the row goes grey forever — the
same verdict it would get if the feature did not apply to it.

This document is measurement, not a proposal. The choice of what to do about it is open —
see "What this settles" at the end.

## How a result is graded

`AddResult` (`conformance/matrix.go:122-133`) sorts every `TestResult` into one of three
buckets, and it checks `Skipped` **first**:

```go
case r.Skipped:  hr.Summary.Skipped++
case r.Passed:   hr.Summary.Passed++
default:         hr.Summary.Failed++
```

So `Skipped: true` is absolute. There is no fourth state, and no way to say "answered, and
correctly refused".

Every test also returns a plain `Error` when `launchProcess` or `rp.send` fails. Those are
excluded from the table below, because they say the binary would not start or its stdin was
already closed — they are the same for all twenty-two features and discriminate nothing. The
live matrix bears this out: all thirty `write |1: broken pipe` failures in it sit on `compact`,
`config`, `reasoning`, `user_message` and `message`, six each, which is one dead process
counted five times.

## The audit

| feature | test | can it fail on its own contract? | the failing path, if any |
|---|---|---|---|
| `start` | `testStart` | **yes** | never reaches `SessionRunning`; empty `bridge_session_id` |
| `message` | `testMessage` | **yes** | no `EventResult`; nil result; empty text |
| `import` | `testImport` | **yes** | any exit code other than 0 (pass) or 2 (skip) |
| `errors` | `testErrors` | only on a malformed payload | emits `EventError` with an empty message. Absence skips. |
| `session_info` | `testSessionInfo` | only on a malformed payload | emits `EventSessionInfo` with nil `Info`. Absence skips. |
| `streaming` | `testStreaming` | no — fails for `message` | "no result event received". No `EventStream` skips. |
| `block` | `testBlock` | no — fails for `message` | "no result event received". No `EventBlock` skips. |
| `user_message` | `testUserMessage` | no — fails for `message` | "no result event received". No `EventUserMessage` skips. |
| `context_used` | `testContextUsed` | no — fails for `message` | no result / nil result. All-zero usage skips. |
| `compact` | `testCompact` | **no** | — |
| `config` | `testConfig` | **no** | — |
| `fork` | `testFork` | **no** | — |
| `resume` | `testResume` | **no** | — |
| `reasoning` | `testReasoning` | **no** | — |
| `system_prompt` | `testSystemPrompt` | **no** | — |
| `discover` | `testDiscover` | **yes**, since 2026-08-07 (finding 3, fixed) | accepts `-discover`, exits 0, writes output that does not decode as `[]msg.StoredSession` |
| `plan` | `testPlan` | **no** | — |
| `hook` | `testHook` | **no** | — |
| `tool_calls` | none | **no** | unconditional skip, "requires real LLM interaction" |
| `thinking` | none | **no** | unconditional skip, same reason |
| `usage_total` | none | **no** | unconditional skip, "server-derived" — honest, see below |
| `turn_complete` | none | **no** | unconditional skip, same reason |

Four of twenty-two are gradeable — three when this audit was taken at `2d60e11`, plus `discover`
once finding 3 was fixed. Two more catch a malformed payload but never an absent one. Four have
a failing path that belongs to `message`. Eight cannot fail on any input. Four have no test body
at all.

`usage_total` and `turn_complete` are the honest members of that last group: the runner spawns
harnesses as direct subprocesses and llm-bridge-server derives those two events itself, so no
harness could emit them. They are matrix rows so the UI can name every event type in the
protocol, and `matrix.go:47-52` says so. That is documentation, not a hole.

## What the live matrix says

`~/.config/llm-bridge/conformance.json`, generated 2026-06-11, 17 harnesses × 22 features =
**374 results: 50 passed, 98 failed, 226 skipped.** Sixty per cent of the matrix is Skip.

⚠️ The run predates `c26608d` and `4e3b065`, so its counts are stale against `main`. They are
quoted here as evidence of the *shape*, not as current grades. The structural findings below
are properties of the code and hold whenever it is re-run.

**Every one of the 98 failures is a transport failure. There are exactly four distinct
reasons and not one of them is a feature contract:**

| count | reason |
|---|---|
| 33 | `no result event received` |
| 30 | `write \|1: broken pipe` |
| 22 | `event channel closed` |
| 13 | `timeout waiting for event` |

Seven features are **17 skip / 0 pass / 0 fail** — no harness has ever received a verdict for
`tool_calls`, `thinking`, `plan`, `hook`, `session_info`, `usage_total` or `turn_complete`.
Four more are 16 skip / 1 pass / 0 fail, the one pass being `aider`.

`discover` is the exception worth naming: 17/17 pass. It runs as a separate process invocation
and never touches the JSON-RPC loop, so it is the only row measuring something every harness
actually does.

## Four places the grading contradicts itself

These are the findings. Each is one file disagreeing with itself, so none of them needs a
cross-repo decision to see.

**1. The same failed start is graded FAIL under `start` and SKIP under three other features.**
`testFork` (`:778`), `testResume` (`:799`) and `testSystemPrompt` (`:574`) each start a session
and skip if it never reaches `SessionRunning`. `testStart` (`:624`) does the same thing and
returns an Error. The live matrix partitions on exactly this: `aider` passes all four, and the
other sixteen harnesses read `start` FAIL, `fork` SKIP, `resume` SKIP, `system_prompt` SKIP.
Forty-eight skips that are one start failure wearing "not applicable".

**2. "No terminating event" is an Error in three tests and a Skip in two.** `testStreaming`,
`testBlock` and `testUserMessage` return `Error: "no result event received"`. `testPlan`
(`:469`) and `testHook` (`:520`) return `Skipped: true, Error: "no terminating result/error
event"` for the identical condition. A harness that emits nothing and never terminates is
failed by three of those tests and excused by two.

**3. `discover` filed a broken implementation as an absent one — FIXED 2026-08-07.**
`testDiscover` skipped when the binary supported `-discover` and returned invalid JSON.
Supporting the flag and emitting garbage is a violation; supporting nothing is not. They got the
same verdict.

The malformed-output path now returns a plain `Error`. The non-zero-exit path still skips, and
rightly: that one really does mean the flag is not implemented. This needed none of the A/B/C
below — the existing `Error` verdict already means "this is wrong", and malformed output from a
supported flag is exactly that. `conformance/discover_test.go` drives `testDiscover` against
stub binaries covering all three answers and asserts the verdict through `AddResult`'s summary,
because `AddResult` tests `Skipped` before `Passed` and a Skipped result can never be counted
red however its `Error` field reads. The duplicate copy of the subtest in `conformance_test.go`
was changed with it.

This moves no current grade: `discover` is 17/17 pass in the stored matrix, because it runs as a
separate process invocation and never touches the JSON-RPC loop. It closes a hole before
something falls in it.

**4. `system_prompt` never checks the system prompt.** `testSystemPrompt` passes when the
session reaches `SessionRunning` — which is precisely what `testStart` asserts. Its Pass
carries no information `start` does not already carry, and its Skip means start broke. The
prompt is sent and nothing reads it back.

Contrast `testImport` (`:862-881`, doc at `:847`), whose doc comment spells out a tri-state contract — exit 2
skips, exit 0 passes, anything else fails — and which is the only feature in the suite that
grades all three answers. It is the shape the rest do not have.

## A second copy of the suite, three features behind

`conformance/conformance_test.go` is a near-duplicate of `runner.go`: the same nineteen
subtests over the same protocol, with its own `harnessProcess` instead of `runProcess`. It is
missing **`reasoning`, `system_prompt` and `context_used`**.

Those three arrived in `998f6e5` (2026-04-17), which touched `matrix.go` and `runner.go` and
not the test file. So the duplicate has been three features behind for nearly four months, and
nothing notices — `go test ./conformance` passes either way, because a feature the copy does
not know about cannot fail in it.

## What this settles

The todo this audit was filed against (`f44a9343`) offered three directions. The measurement
speaks to the choice without making it:

- It **kills option C** as written. C is "leave it, and say in the feature description that
  Config and Reasoning measure liveness, not capability". That is true of nineteen features,
  not two, and a description on each of nineteen rows saying "this row cannot fail" is a
  matrix nobody should read.
- It **enlarges option A** and cheapens it at the same time. A fourth verdict is a change to
  `TestResult`, `AddResult` and the UI once, and it then has nineteen rows to give a real
  answer to rather than two.
- Finding 1 is **fixable without picking any of the three**: `fork`, `resume` and
  `system_prompt` skip on a condition `start` already grades as a failure. Making them read
  `start`'s verdict rather than re-deriving it is not a grading policy, it is the removal of a
  second answer to a question already answered.
- Findings 2, 3 and 4 and the drifted duplicate are likewise independent of the A/B/C. Finding 3
  has since been fixed on that basis, and needed no new verdict to do it.

Nothing above changes any harness's grade on its own. Re-run the suite before quoting the
numbers.
