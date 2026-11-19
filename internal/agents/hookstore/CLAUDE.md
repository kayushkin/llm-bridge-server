# About hook-store

## What it owns

SQLite registry of bridge-managed harness hooks — each row a handler (harness, event, matcher, shell command) bound to a `global`, `instance` or `session` scope. The server queries it at session spawn to decide which hooks to wire into the harness's native mechanism. A pure data layer: it does not execute hooks, render settings JSON, or talk to harnesses.

⚠️ **A `Stop` row whose command prints `additionalContext` keeps every session it reaches from ending its turn.** Measured on Claude Code 2.1.282 (2026-09-26, `prediction_000011`): the text reaches the model as a `Stop hook additional context` system reminder, and the harness then treats it as a block on stopping, so it prompts the model again. With no guard it did so 9 times, until its own cap ended the turn; one one-word turn cost $0.17. The harness's own message says such a hook should succeed without output while its stdin has `stop_hook_active: true`; that guard was not tested. Try one on a `session`-scoped row first, never a `global` one.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.
