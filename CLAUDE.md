# About sign-in-store

## What it owns

`127.0.0.1:8328`. **How people sign in** to dash, kayushkin.com and watch.kayushkin.com, and the **sessions** that result: one-time sign-in links (`sign_in_link_000001`) and sessions (`session_000001`), each kept only as a SHA-256 of its secret. It does not own who anybody is: principal-store does, and every session check asks it whether the principal still exists, is enabled, is a person and is an administrator. It replaces three separate logins — dash's shared `DASH_TOKEN`, kayushkin.com's film-viewer links, and llm-bridge-server's demo login — in the steps of design note `f3d492d9-a6b0-4ea1-bd8a-46f28ee3542b`. `CONTRACT.md` is the route table.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Links and sessions

A link signs one person in once, within 7 days. The operator makes one through `POST /sign-in-links` and hands it over, or the person types their Discord username or phone number into the sign-in page and `POST /link-requests` sends one to their own one-to-one chat through multichat, found through people-store's identities; that route answers the same whether or not the handle has an account, and allows 5 an hour per address and per handle. The link is `SIGN_IN_STORE_SIGN_IN_PAGE_URL#<secret>`: the secret sits after `#`, which a browser never sends, so a chat app's link preview cannot spend it, and the page posts it to `POST /redeem`. Redeeming spends the link and opens a 180-day session in one transaction, and sets cookie `sign_in_session` for `kayushkin.com` and every name under it. `GET /session` is what every app and Caddy's `forward_auth` ask with the browser's cookies. **No self sign-up**: every way in finds a principal that already exists, and only an enabled `human` signs in. principal-store's answer is cached 30 s (`principalCacheLifetime`), the delay on disabling someone.

## Sign in with Discord

`discord.go`. A person who has **linked** their Discord account signs in by proving they hold it: `GET /discord/start` sends the browser to Discord (the operator's Event-Manager application `1540841733535834242`, shared with discord-signup-store, scope `identify`), Discord sends it back to `GET /discord/callback`, the code is traded for the Discord user id with the client secret from auth-store `cred_b6101072bcdf427cd2758413`, and principal-store's identity binding (`provider=discord`, empty tenant, the user id as subject) names the principal. **No binding, no sign-in**: Discord proves who someone is on Discord, and only a link they made themselves says who they are here. Linking is the same round trip with `purpose=link`, started by someone already signed in; it binds the account they prove to their own principal, `linked_by` themselves, and principal-store refuses an account already bound to anyone. Each round trip is a `discord_attempts` row: the state and a browser-binding cookie kept as hashes, 10 minutes, spent once, finished only in the browser that started it. `return_to` must be https on the cookie domain, so the callback is never an open redirect. ⚠️ **Discord sends a browser back only to an address on the application's redirect list**: `SIGN_IN_STORE_DISCORD_REDIRECT_URL` (`https://kayushkin.com/api/sign-in/discord/callback`) must be added there on Discord's developer portal, by the operator, or every attempt ends on Discord's own error page.

## What comes next

Steps 1 to 3 are live: this service; kayushkin.com's `/signin` page, its stream check (which Caddy on watch.kayushkin.com asks) and downloadstack's film-viewer check reading `sign_in_session`, with kayushkin.com's older `viewer_session` still believed until it expires; and dash, which takes `sign_in_session` beside `DASH_TOKEN`, acts as each person's own principal, and lets only administrators in. nginx on kayushkin.com passes exactly the four browser routes under `/api/sign-in/`. Sign in with Discord is built (above). Still to do, in order: Entra moved over from llm-bridge-server; passwords; passkeys; demo login off.

# Access and operations

## Callers, bind and secrets

**The way back in from a shell:** `~/bin/sign-in-store make-link principal_000001` (as root, `sudo -iu kayushkincom ~/bin/sign-in-store make-link principal_000001`) prints a one-time sign-in link for that principal, `sent_by` `host_shell`. It needs only the database and principal-store, not the running service or its token, because having a shell on this host is the credential. It refuses to create a database, so run as the wrong user it fails instead of making a link nobody can use. Use it when nobody can sign in any other way, as on 2026-10-23, when changing `DASH_TOKEN` logged the operator out everywhere.


Binds loopback only. nginx must pass a browser only `POST /link-requests`, `POST /redeem`, `GET /session`, `POST /sign-out`, `GET /discord/start` and `GET /discord/callback`, setting `X-Real-IP`, which is believed only from a loopback caller. The administrator routes take `X-Sign-In-Store-Service-Token` plus `X-Principal-Id` naming an enabled administrator, whose id is recorded on what they do. The service token and `AUTH_STORE_TOKEN` live in `~/.config/sign-in-store/sign-in-store.env` (mode 600), which the first `deploy.sh` writes; multichat's bearer is read from auth-store credential `cred_e1b7ffd551a0741c9a6d5f93` each time a link is sent. Unit `sign-in-store.service` (user), binary `~/bin/sign-in-store`, data `~/.config/sign-in-store/sign-in-store.db`.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/sign-in-store`, root package `signinstore`, server in `cmd/sign-in-store`, `replace ../llm-bridge` for `servicesettings`. Every variable is declared in `settings.go`; a set `SIGN_IN_STORE_` variable nobody declared stops the service at boot, and `deploy.sh` checks the running service's environment before it stops it. Tests stand in for principal-store, people-store, auth-store and multichat with one `httptest` server (`server_test.go`). Deploy with `./deploy.sh` from the main clone on `main`; it smoke-checks that `/session` and `/sessions` refuse a caller with nothing.
