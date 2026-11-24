# About repodetect

## What it owns

The repository-signature ruleset: a pure `Detect(path) → Signature` plus tag helpers, reporting which languages, frameworks, build tools and test tools a repo uses. Split out of the repo-store module on 2026-07-28 so a consumer can take the ruleset without pulling in the service.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.
