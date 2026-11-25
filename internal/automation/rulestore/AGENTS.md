# About rule-store

## What it owns

`127.0.0.1:8324`. Rules that react to a change in one service by doing something in another, starting with llm-bridge-server sessions: "when a session changes, if these conditions hold, do these effects". It owns the rules, their immutable revisions, every activate/pause/retire with who and why, and every decision a rule made (`rule_run_000001`). It owns no subject and keeps no clock. `CONTRACT.md` is the route table and the rule language.

## Why it exists

On 2026-10-19 the rules that react to sessions lived as hardcoded Go in polling binaries under `scheduler/cmd/`: kanban-curator (three jobs) and autoworker-reviewer each read the same session states every 5 minutes with their own copy of the session read, and coordinated through a session tag. rule-store holds those rules as data, reacts to llm-bridge-server's `/session-events` feed within seconds, and records every decision so it can be checked.

# How it works

## Shadow first

**A rule runs in shadow mode (records the calls its effects would make, makes none) or live mode (makes them, with every owner's answer kept on the run).** A live match is written as `applying` before its first call, so it cannot be applied twice; it ends `applied` or `failed` at the first refusal, with no retry, and a run caught mid-way by a restart is marked `interrupted`, never re-run. Switch a rule to live only after `GET /rules/{id}/comparison` agrees with the job it replaces, and turn that job off in the same sitting, so two writers never act on one card.

## What stays out

Rules that touch only their own service stay there (renaming and turn classification in llm-bridge-server, card-only ticket rules in kanban-store). No cron: the scheduler keeps time, and the sweep only re-checks `updated_before` conditions. No controllers or pipelines (autoworker pacing, reminders, email filing). A condition reads only the subject that changed, its age, its entity tags and its linked cards — **do not add a condition that reads another record**, and do not add "or" or nesting: a rule that needs "or" is two rules. Those limits are what keep a rules store from becoming a programming language nobody reviews.

## Once per state entry

A run is unique on (revision, session, `<state>#<episode>`), where rule-store counts each session's state changes itself because the bridge records no "entered at". The feed and the sweep both decide, and the uniqueness makes them safe together. A `hello` without `resume: replayed` makes rule-store read every session again (about 70,000, 6 seconds) and re-check everything in the background.

# Access and operations

## Running it

Unit `rule-store.service`, binary `~/bin/rule-store`, loopback only because the routes take no credential. The two tokens come from `~/.config/systemd/user/rule-store.service.d/10-tokens.conf`, which loads the shared `~/.config/principal-gating-tokens.env`; never put a token in the unit. `GET /health` shows whether the feed is connected, the last seed and sweep, and the last error.

# Working in this repo

## Build, test and deploy

`go test ./...`, `go vet ./...`; `make check`. Deploy only through `./deploy.sh` from the main clone on `main`, pushed. Settings are declared once in `settings.go` with llm-bridge `servicesettings` (`replace ../llm-bridge`); a set `RULE_STORE_` variable nobody declared stops the service at boot. `reactor_test.go` runs the reactor against a fake bridge feed; extend it when you change how the feed or sweep decides.
