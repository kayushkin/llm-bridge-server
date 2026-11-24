# About skill-store

## What it owns

`:8301`. Registry of Claude Code / Agent `SKILL.md` skills, ingested by cloning an upstream repo — or, since 2026-08-04, by reading a directory on this host — and walking it for `SKILL.md`. Data in `~/.config/skill-store/`. Unit `skill-store.service` is tracked here since 2026-11-05 (`%h` paths, `127.0.0.1:8301`, which is also the default), and `deploy.sh` installs and enables it, so a fresh host needs no hand-made unit.

The three variables it reads, `SKILL_STORE_ADDR`, `SKILL_STORE_DATA_DIR` and `SKILL_STORE_INSTALL_DIR`, are declared once in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`, which makes `deploy-gate` check that tree too) and served read-only at `GET /settings`. Both binaries read them through the registry: `cmd/skill-store` serves the route, `cmd/seed` only builds the registry. A new variable is declared there or `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails, and a set variable under an owned prefix that nobody declared stops the process at boot; `deploy.sh` checks the running service's environment for one before it stops it. The owned prefixes are the three full names, not `SKILL_STORE_`, because `SKILL_STORE_URL` is how the rest of the fleet finds this service and it reaches `cmd/seed` through the shell that runs it. Declare nothing `Editable` while the service has no operator gate.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.
