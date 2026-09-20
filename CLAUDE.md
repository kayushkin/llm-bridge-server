# About mailstack

## What it owns

`:8195`. Centralized read/write email behind dash's Mail page, across Gmail, Outlook (Microsoft Graph) and PrivateEmail (IMAP/SMTP), with multiple accounts per provider. ⚠️ `PLAN.md` still says *"planning, no code yet"* and was last touched 2026-04-17, while the service is running and listening — trust the code over the plan.

Its six environment variables are declared once, in `settings.go`, with llm-bridge `servicesettings`; `cmd/mailstack` reads them through that registry, and `GET /settings` describes them behind the same bearer as `/api/*`, serving a token as set or unset and never its text. A test fails on any `os.Getenv` the declarations do not name, so add a variable there first. The service owns the four names `MAILSTACK_PORT`, `MAILSTACK_DB`, `MAILSTACK_LOCAL_DB` and `MAILSTACK_TOKEN` and refuses to start on a set variable that begins with one of them and is not declared (`MAILSTACK_DB_PATH`); it does not own `MAILSTACK_`, because `MAILSTACK_URL` is what callers read to find it. The `--port`, `--db` and `--local-db` flags override their settings, and the unit passes `--port`, so the settings page shows the environment's value, not a flag's. `deploy.sh` builds the registry from the running service's environment before it stops it. `scripts/e2e-smoke.sh` asserts that the default database is absent, so run it with `HOME` pointed at an empty directory.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.
