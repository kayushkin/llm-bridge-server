# mailstack — implementation plan

**Status:** planning, no code yet.
**Owner:** vvkayushkin
**Last updated:** 2026-04-17

## Goal

Centralized read/write email service that powers a Mail page on dash. Supports
multiple accounts across three providers:

- **Gmail** (multiple Google accounts) — Gmail API v1, OAuth2 via apiauth.
- **Outlook** (multiple Microsoft accounts) — Microsoft Graph API, OAuth2 via apiauth.
- **PrivateEmail** (multiple Namecheap mailboxes) — IMAP/SMTP, password creds via apiauth.

`mailstack` is a standalone Go service. dash-server proxies a thin
`/api/mail/*` namespace through to it. The dash React frontend is the only UI.

## Non-goals (v1)

- Threading / conversation grouping (deferred).
- Search (deferred — providers' native search syntaxes differ too much).
- Local **message** caching / offline mode (deferred — message bodies/lists
  hit providers live). Token caching is in scope (see Stateful pieces).
- Push notifications (Pub/Sub, webhooks, IDLE) — manual refresh + 60s client poll only.
- Sending from aliases.
- Calendar / contacts integration (out of scope; calendar already lives in dash-server).

---

## Architecture

```
┌────────────────────────────────────────────┐
│ dash frontend (React, /mail)               │
│  3-pane: sidebar | list | reader           │
│  60s poll while tab visible + manual btn   │
└────────────────────┬───────────────────────┘
                     │ /api/mail/*
┌────────────────────▼───────────────────────┐
│ dash-server (:8101)  api_mail.go           │
│  thin reverse-proxy → mailstack            │
│  (session-cookie auth same as calendar)    │
└────────────────────┬───────────────────────┘
                     │ HTTP (loopback)
┌────────────────────▼───────────────────────┐
│ mailstack (:8195, new)                     │
│  ┌────────────────────────────────────┐    │
│  │ MailBackend interface              │    │
│  │   gmail │ graph │ imap impls       │    │
│  └────────────────────────────────────┘    │
│  ┌────────────────────────────────────┐    │
│  │ accounts.db (sqlite)               │    │
│  │   accounts table only              │    │
│  └────────────────────────────────────┘    │
└─────────────┬──────────────────┬───────────┘
              │ tokens           │ imap creds
┌─────────────▼──────────────────▼───────────┐
│ apiauth (:8190)                            │
│  oauth tokens (multi-account already)      │
│  + new imap_creds table                    │
└────────────────────────────────────────────┘
```

### Why separate service

- Reusable from CLI / agents / inber later — over HTTP only. **mailstack is a
  service, not a library.** No Go import path for consumers; every client
  (dash, agents, CLI) goes through the HTTP API.
- Keeps dash-server lean — it stays a UI shell + thin proxy.
- Mail logic (IMAP connection pools, OAuth token refresh, MIME parsing, HTML
  sanitization) is heavy enough to warrant isolation.
- Independent deploy cadence; restart mail without bouncing the dash UI.

### Stateful pieces

mailstack is not stateless. It holds:

1. **Token cache** — in-memory `map[(provider,app,cred,account)]CachedToken`,
   entries evict 60s before expiry, refresh on miss via apiauth. Avoids an
   apiauth hop on every provider request. Cache lives only in process memory;
   restart = cold start.
2. **IMAP connection pool** — **single persistent connection per enabled IMAP
   account** (expected: 5-10 accounts total). Lazy-dial on first use. On
   disconnect: log, mark unhealthy, reconnect on next request. No multiplexing
   — v1 serializes FETCH/STORE/IDLE through the one conn.
3. **Account registry** — `accounts.db` (SQLite), canonical source for
   `ID → (provider, apiauth_app, apiauth_cred, apiauth_account, imap_cred)`.

### Port allocation

`mailstack` listens on **`:8195`** (loopback only). Adjacent to apiauth (8190).
Confirmed unused via `ss -tlnp` on 2026-04-17. Verify again at deploy time —
ports are not hardcoded in CLAUDE.md per directive.

---

## Multi-account model

apiauth's `tokens` table already has `(provider, account)` as the primary key
with `account TEXT NOT NULL DEFAULT 'default'` — so multi-account storage is
already supported. The HTTP API needs to expose `?account=` everywhere (verify;
extend if missing).

mailstack owns its own `accounts` table — the canonical registry of mail
accounts the user has configured. Each row references credentials by name in
apiauth.

```sql
-- mailstack/accounts.db
CREATE TABLE accounts (
    id              TEXT PRIMARY KEY,        -- e.g. "gmail-personal", uuid or slug
    label           TEXT NOT NULL,           -- "Vlad Personal", "Work Outlook"
    provider        TEXT NOT NULL,           -- "google" | "microsoft" | "imap"
    address         TEXT NOT NULL,           -- email address (display + reply matching)
    -- OAuth accounts (google/microsoft): reference a specific apiauth credential + user-token
    apiauth_app     TEXT NOT NULL DEFAULT '',   -- e.g. "mail"
    apiauth_cred    TEXT NOT NULL DEFAULT '',   -- name of the OAuth client, e.g. "primary"
    apiauth_account TEXT NOT NULL DEFAULT '',   -- apiauth token's account field
    -- IMAP accounts: reference a single apiauth imap_cred
    imap_cred_name  TEXT NOT NULL DEFAULT '',
    color           TEXT NOT NULL DEFAULT '#888',
    signature       TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
```

The `id` is the public account identifier used in every API call
(`?account=gmail-personal`). Frontend never sees apiauth internals.

---

## apiauth changes

### 1. Schema migration: named credentials

Today: `credentials` keyed by `(provider)` only. One OAuth client per provider.
Tokens keyed by `(provider, account)`.

Required: support **multiple OAuth clients per provider** (one per service: mail,
calendar, drive, etc.) AND **multiple registered apps within the same service**
(e.g. two distinct "mail" OAuth clients for separation between personal and
business audits).

New schema:

```sql
CREATE TABLE credentials (
    provider      TEXT NOT NULL,           -- "google" | "microsoft"
    app           TEXT NOT NULL,           -- "mail" | "calendar" | ...
    name          TEXT NOT NULL,           -- "primary" | "secondary" | ...
    client_id     TEXT NOT NULL,
    client_secret TEXT NOT NULL,
    tenant_id     TEXT NOT NULL DEFAULT '',
    redirect_url  TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (provider, app, name)
);

CREATE TABLE tokens (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    provider    TEXT NOT NULL,
    app         TEXT NOT NULL,
    cred_name   TEXT NOT NULL,             -- which credential this token came from
    account     TEXT NOT NULL DEFAULT 'default',
    scopes      TEXT NOT NULL,
    access      TEXT NOT NULL,
    refresh     TEXT NOT NULL,
    expires_at  INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE(provider, app, cred_name, account)
);
```

Migration plan: existing rows backfill as `app='calendar'`, `cred_name='primary'`
(since today's only OAuth use is the calendar). One-shot migration in apiauth's
init function, gated on a `schema_version` row.

### 2. New scopes for existing providers

`apiauth/providers/google.go` — add scope set keyed to `app="mail"`:
```
https://mail.google.com/
```
Full Gmail access (needed for permanent delete; modify-only can't hard-delete).

`apiauth/providers/microsoft.go` — add scope set keyed to `app="mail"`:
```
Mail.ReadWrite
Mail.Send
MailboxSettings.ReadWrite
offline_access
User.Read
```

Provider scope tables become `map[app][]string` instead of a flat default list.

### 3. Updated HTTP API

```
# Credentials (CRUD)
POST   /api/credentials                              # body: {provider, app, name, client_id, ...}
GET    /api/credentials?provider=&app=               # list (filterable)
GET    /api/credentials/:provider/:app/:name
PUT    /api/credentials/:provider/:app/:name
DELETE /api/credentials/:provider/:app/:name

# OAuth login (per credential, per account)
GET    /api/login/:provider?app=mail&cred=primary&account=personal
       # initiates OAuth flow; redirects user; on callback persists token
       # under (provider, app, cred_name, account)

# Tokens
GET    /api/token/:provider?app=mail&cred=primary&account=personal
       # returns valid access token; auto-refreshes using cred's client_secret
GET    /api/tokens?provider=&app=&cred=              # list (no secrets)
DELETE /api/token/:provider?app=mail&cred=primary&account=personal
```

If `cred=` is omitted on a `GET /api/token`, apiauth errors loudly (no
implicit selection — per the no-fallback-chains directive). The mailstack
account row always carries the explicit `(app, cred_name, account)` triple.

### 4. New IMAP credential type

New table in apiauth alongside `tokens` and `credentials`:

```sql
CREATE TABLE imap_creds (
    name        TEXT PRIMARY KEY,         -- e.g. "privateemail-vlad"
    address     TEXT NOT NULL,            -- email
    imap_host   TEXT NOT NULL,            -- mail.privateemail.com
    imap_port   INTEGER NOT NULL,         -- 993
    smtp_host   TEXT NOT NULL,
    smtp_port   INTEGER NOT NULL,         -- 465
    username    TEXT NOT NULL,
    password    TEXT NOT NULL,            -- plaintext; protected by fs perms
    tls_mode    TEXT NOT NULL,            -- "tls" | "starttls"
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
```

**Storage**: plaintext, same as OAuth tokens today (verified: apiauth has no
at-rest crypto — only `crypto/rand` for OAuth state). Protection is filesystem
perms. Tighten on init: `chmod 0600 ~/.config/apiauth/tokens.db` after open.
Documented as a deliberate single-user-homelab tradeoff. If we ever go
multi-user, revisit with a real KMS.

New endpoints:

```
POST   /api/imap/creds              # create  {name, address, imap_host, ...}
GET    /api/imap/creds              # list (no passwords in response)
GET    /api/imap/creds/:name        # get (no password)
GET    /api/imap/creds/:name/full   # internal: returns password (mailstack only)
PUT    /api/imap/creds/:name
DELETE /api/imap/creds/:name
POST   /api/imap/creds/:name/test   # try connect, return ok/err
```

The `/full` endpoint is gated by a localhost check + a service token shared
with mailstack (env var). dash-server never calls it.

---

## mailstack: data model

Internal Go types (canonical, provider-agnostic):

```go
type Account struct {
    ID, Label, Provider, Address string
    Color, Signature string
    Enabled bool
}

type Folder struct {
    ID         string         // provider-native ID
    AccountID  string
    Name       string         // display name
    Path       string         // hierarchical, "/" separated
    Canonical  CanonicalKind  // inbox | sent | drafts | trash | spam | archive | "" (custom)
    ParentID   string         // empty if root
    UnreadCount int
}

type CanonicalKind string  // "inbox", "sent", "drafts", "trash", "spam", "archive"

type MessageMeta struct {
    ID         string         // provider-native ID; opaque to client
    AccountID  string
    FolderID   string
    Subject    string
    From       Address
    To, Cc     []Address
    Date       time.Time
    Snippet    string         // first ~200 chars
    Unread     bool
    Flagged    bool
    HasAttach  bool
}

type Message struct {
    Meta        MessageMeta
    BodyHTML    string         // sanitized at the edge, not here
    BodyText    string
    Headers     map[string]string
    Attachments []Attachment
    InReplyTo   string         // for stash, even though we don't thread
}

type Attachment struct {
    ID, Filename, MimeType string
    Size                   int64
    InlineCID              string  // for inline images (cid: refs)
}

type Address struct{ Name, Email string }

type Draft struct {
    AccountID    string
    To, Cc, Bcc  []Address
    Subject      string
    BodyHTML     string  // canonical body format; text generated server-side
    InReplyTo    string  // message ID being replied to (provider-native)
    References   []string
    Attachments  []DraftAttachment
}
```

## mailstack: backend interface

```go
type MailBackend interface {
    Folders(ctx context.Context) ([]Folder, error)
    List(ctx context.Context, folderID, cursor string, limit int) (msgs []MessageMeta, next string, err error)
    Get(ctx context.Context, id string) (Message, error)

    MarkRead(ctx context.Context, id string, read bool) error
    Flag(ctx context.Context, id string, flagged bool) error
    Move(ctx context.Context, id, folderID string) error
    Delete(ctx context.Context, id string) error          // → trash
    PermDelete(ctx context.Context, id string) error      // delete from trash

    Send(ctx context.Context, d Draft) (sentID string, err error)
    SaveDraft(ctx context.Context, d Draft, existingID string) (id string, err error)
    Attachment(ctx context.Context, msgID, attID string) (io.ReadCloser, contentType string, err error)
}
```

Three implementations:

- `backend/gmail.go` — uses `google.golang.org/api/gmail/v1`. Gmail labels
  mapped to canonical folders (`INBOX` → inbox, `SENT` → sent, etc.). Move =
  add target label + remove source label.
- `backend/graph.go` — direct HTTP calls to `https://graph.microsoft.com/v1.0`
  (no SDK; the official Go SDK is huge). Folder IDs from `/me/mailFolders`.
- `backend/imap.go` — `github.com/emersion/go-imap/v2` for read/modify;
  `github.com/emersion/go-smtp` for send. Connection pool per account
  (one persistent IMAP conn, reconnect on idle close).

### Folder canonical mapping

Per provider, fixed map at startup (no heuristics):

| Canonical | Gmail label | Graph well-known folder | IMAP guess |
|-----------|-------------|--------------------------|---|
| inbox     | INBOX       | inbox                    | INBOX |
| sent      | SENT        | sentitems                | Sent / Sent Items |
| drafts    | DRAFT       | drafts                   | Drafts |
| trash     | TRASH       | deleteditems             | Trash / Deleted Items |
| spam      | SPAM        | junkemail                | Junk / Spam |
| archive   | (no system label) | archive            | Archive |

For IMAP we probe at account-add time using the SPECIAL-USE extension
(RFC 6154). Persist the resolved IDs in mailstack's accounts.db so we don't
re-probe per request. If SPECIAL-USE is unavailable, fall back to a small
candidate-name list per canonical kind. **Per the no-fallback-chains directive:
if the candidate-name lookup fails too, surface it as "user must map manually"
in the account settings UI — don't guess silently.**

---

## mailstack: HTTP API

All endpoints require the shared service token (Bearer header, env var
`MAILSTACK_TOKEN`). dash-server forwards it. No direct browser access.

```
# Accounts
GET    /api/accounts                              # list
POST   /api/accounts                              # add  {provider, apiauth_account, label, ...}
GET    /api/accounts/:id
PATCH  /api/accounts/:id                          # label, color, signature, enabled, sort_order
DELETE /api/accounts/:id

# Folders
GET    /api/folders?account=ID                    # one account
GET    /api/folders?account=all                   # all enabled accounts, grouped

# Messages
GET    /api/messages?account=ID&folder=FID&cursor=&limit=50
GET    /api/messages/:id?account=ID
POST   /api/messages/:id/read?account=ID          # body: {read: bool}
POST   /api/messages/:id/flag?account=ID          # body: {flagged: bool}
POST   /api/messages/:id/move?account=ID          # body: {folder: "FID"}
DELETE /api/messages/:id?account=ID               # → trash
POST   /api/messages/:id/perm-delete?account=ID

# Compose
POST   /api/send?account=ID                       # body: Draft
POST   /api/messages/:id/reply?account=ID         # body: Draft (to/cc/etc auto-filled if omitted)
POST   /api/messages/:id/forward?account=ID
POST   /api/drafts?account=ID                     # save / update draft
DELETE /api/drafts/:id?account=ID

# Attachments
GET    /api/attachments/:msgId/:attId?account=ID  # streamed

# Unified
GET    /api/unified?folder=inbox&limit=50&window=7d
       # folder must be canonical kind: inbox|sent|drafts|trash|spam|archive
```

### Unified inbox mechanics

- Pure read aggregation. Writes always target the message's owning account.
- Server fetches `limit` newest from each enabled account's matching canonical
  folder, with a `since = now - window` filter where the provider supports
  date filters. Merges, sorts by date desc, returns top `limit`.
- Pagination = expand window + bump limit. Client requests `window=7d` then
  `window=30d` then `window=365d`. Crude but adequate for v1.
- Per-account fetch is concurrent (`errgroup`). Per-account failure is reported
  in a sidecar `errors[]` array — does NOT fail the whole response. UI shows a
  banner. **Fail loud on the failing account, not on the unified call.**

---

## dash-server: mail proxy

New file `dash/server/api_mail.go`. Single reverse-proxy handler for
`/api/mail/*` that:

1. Verifies session cookie (existing dash auth middleware).
2. Strips `/api/mail` prefix and forwards to `http://localhost:8195/api/...`
   with the `MAILSTACK_TOKEN` Bearer header.
3. Streams response body through (so attachment downloads don't buffer in dash).

Reference: pattern matches `dash/server/api_apiauth.go` (apiauth proxy).

No mail logic in dash-server. It is a transparent transport (per repo directive
on layer transparency).

---

## dash frontend

### Routes

```
/mail                              → redirect to last-viewed
/mail/unified/:canonical           → unified view of a canonical folder
/mail/account/:accountId           → redirect to inbox
/mail/account/:accountId/:folderId
/mail/account/:accountId/:folderId/:msgId
```

Compose opens as a modal layered on top of the current route (so you can keep
referencing the current message while writing).

### Layout — 3 panes

```
┌──────────┬──────────────────┬──────────────────┐
│ sidebar  │ message list     │ reader           │
│          │                  │                  │
│ [Mode]   │ ☐ subject  date  │ subject          │
│ Unified  │ ☐ subject  date  │ from / to / date │
│ —        │ ...              │ ─────            │
│ Personal │                  │ html body        │
│  inbox 5 │                  │                  │
│  sent    │                  │                  │
│  ...     │                  │                  │
│ Work     │                  │                  │
│  ...                                            │
└──────────┴──────────────────┴──────────────────┘
```

**Sidebar mode toggle**: top of sidebar has segmented control:
`[ Unified | Per account ]`. Persisted in localStorage.

- Unified mode → list shows the six canonical folders, each row aggregates
  unread across all accounts.
- Per-account mode → grouped tree: account label → full folder tree (canonical
  + custom). Account header has the account color dot + total unread.

### Components

```
src/pages/Mail.tsx                       # route shell, mode state
src/mail/MailLayout.tsx                  # 3-pane container
src/mail/Sidebar.tsx                     # mode toggle + tree
src/mail/AccountFolders.tsx              # one account's tree
src/mail/UnifiedFolders.tsx              # canonical-only list
src/mail/MessageList.tsx                 # virtual scroll, bulk-select
src/mail/MessageRow.tsx
src/mail/Reader.tsx                      # html iframe + actions
src/mail/Composer.tsx                    # modal
src/mail/AttachmentList.tsx
src/mail/AccountSettings.tsx             # add/edit/remove accounts
src/mail/api.ts                          # typed fetchers for /api/mail/*
src/mail/types.ts                        # mirrors mailstack canonical types
src/mail/hooks/useMessages.ts            # list + pagination
src/mail/hooks/useMessage.ts             # single message
src/mail/hooks/usePolling.ts             # 60s while tab visible
```

Add nav entry "Mail" in `src/components/DashboardLayout.tsx` (between Calendar
and API Auth).

### HTML rendering & safety

- Reader renders message HTML in a sandboxed iframe (`sandbox="allow-popups
  allow-popups-to-escape-sandbox"`, no `allow-scripts`).
- HTML sanitized server-side in mailstack with `bluemonday`'s `UGCPolicy`
  before send, then again client-side with DOMPurify in the iframe srcdoc.
- Remote images blocked by default (rewrite `<img src>` → placeholder); per-message
  "Show images" button replaces the iframe srcdoc with the unmodified version.
- Inline `cid:` images rewritten to `/api/mail/attachments/...` URLs.

### Composer

- TipTap editor (rich text, good React story).
- "From" picker — defaults to:
  - new mail: last-used account.
  - reply: account that received the original (matched by `to/cc` against
    account addresses).
- Drafts auto-save on idle (1s debounce) → `POST /api/drafts`. Each provider
  stores in its own Drafts folder.
- Attachments: drag-drop + file picker, multipart upload to mailstack.

### Polling

`usePolling.ts` ticks every 60s while `document.visibilityState === "visible"`.
Each tick: refetch the current folder's message list and the sidebar's unread
counts. Manual refresh button forces an immediate tick. No SSE/websocket.

---

## Confirmed deferrals

- Threading (flat list only).
- Search (per-provider or unified — both deferred).
- Local message caching (no SQLite cache of bodies; folder/account list lives
  in mailstack's accounts.db only).
- Push notifications.
- Sending from aliases.
- Shared mailbox / delegated access (Graph permission variants).

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| 1 | Gmail scope | `https://mail.google.com/` (full) |
| 2 | Threading | deferred |
| 3 | Caching | none — pure pass-through |
| 4 | Refresh | manual + 60s client poll while visible |
| 5 | Rich-text editor | TipTap |
| 6 | HTML safety | bluemonday (server) + DOMPurify (client) + sandboxed iframe + remote-image blocking by default |
| 7 | Search | deferred |
| 8 | Aliases | not in v1 |
| 9 | Repo split | mailstack standalone service, dash-server thin proxy |
| 10 | apiauth credentials model | keyed by `(provider, app, name)` — multiple OAuth clients per provider/app supported |
| 11 | IMAP password encryption | plaintext in SQLite, protected by `chmod 0600` (matches existing apiauth token storage) |
| 12 | Account-add UX | deep-link Mail page's "Add account" button → existing `/api-auth` page with `?provider=&app=mail&cred=&account=<label>` |
| 13 | Token caching | in-memory cache in mailstack, evict 60s before expiry |
| 14 | IMAP concurrency | single persistent conn per account; serialize operations |
| 15 | Consumer interface | HTTP-only. mailstack is a service, not a library — no exported Go package for agents |

## Open questions — resolved 2026-04-17

1. ~~PrivateEmail server settings~~ — confirm per-mailbox via
   https://privateemail.com → Settings → Email Clients (or Namecheap KB
   article). Working assumption: `mail.privateemail.com:993` IMAP TLS,
   `:465` SMTP TLS. User will confirm during phase 1.
2. ~~apiauth secret encryption~~ — verified: **no encryption at rest**, only
   `crypto/rand` for OAuth state. IMAP passwords stored the same way; fs perms
   tightened to 0600 on init.
3. ~~OAuth client reuse~~ — **do not reuse**. Credentials become named per
   `(provider, app, name)`. Mail gets its own Google + Microsoft OAuth apps
   separate from calendar. Schema migration described above.
4. ~~Account-add UX~~ — deep-link to existing `/api-auth` page, no popup.

## Credential setup checklist (user action, before phase 1)

### Google (Gmail mail app)
1. https://console.cloud.google.com → new project (suggest `vlad-mail`).
2. APIs & Services → Library → enable **Gmail API**.
3. OAuth consent screen → External → add scope `https://mail.google.com/`.
   Add yourself as test user.
4. Credentials → Create → OAuth client ID → **Web application**.
5. Authorized redirect URI: same as the existing calendar app's
   (verified in phase 1 against apiauth's callback route).
6. Save **Client ID** + **Client secret**.

### Microsoft (Outlook mail app)
1. https://portal.azure.com → Azure Active Directory → App registrations →
   New registration. Name: `vlad-mail`.
2. Supported account types: "Personal Microsoft accounts only" (or
   multitenant + personal if any work accounts).
3. Redirect URI (Web): same as Google.
4. Copy **Application (client) ID** and **Directory (tenant) ID**.
5. Certificates & secrets → New client secret → copy value (shown once).
6. API permissions → Microsoft Graph → Delegated:
   `Mail.ReadWrite`, `Mail.Send`, `MailboxSettings.ReadWrite`,
   `offline_access`, `User.Read`. Grant admin consent if prompted.

### What to hand to mailstack setup
- Google: `client_id`, `client_secret`
- Microsoft: `client_id`, `tenant_id`, `client_secret`
- PrivateEmail (per mailbox): address, password, IMAP host/port, SMTP host/port

These get loaded via `POST /api/credentials` and `POST /api/imap/creds` in
apiauth. One-time.

## Phasing

| Phase | Scope | Deliverable |
|---|---|---|
| 1 | apiauth: mail scopes, multi-account login verified, `imap_creds` table + endpoints, encryption decision settled | re-auth all accounts; can fetch tokens; IMAP creds CRUD works; `test` endpoint connects to PrivateEmail |
| 2 | mailstack skeleton: server + sqlite + accounts CRUD + auth (token), `MailBackend` interface, **Gmail backend read-only** (folders, list, get) | `curl`-able read endpoints for one Gmail account |
| 3 | mailstack: Graph + IMAP backends read-only; folder canonical mapping with SPECIAL-USE probe | all 3 providers readable via mailstack |
| 4 | dash-server `/api/mail/*` proxy; dash frontend: Mail page shell, sidebar (per-account mode only), folder tree, message list, reader (HTML safe rendering, no images) | navigate and read mail in browser, all 3 providers |
| 5 | Unified inbox: backend merge + frontend toggle + canonical folder list | toggle sidebar between modes, see merged inbox |
| 6 | Write ops: read/unread, flag, move, delete (→trash), perm-delete; bulk select in list | inbox triage works |
| 7 | Compose + send: Composer modal, send/reply/reply-all/forward, drafts auto-save, attachments upload, image-show toggle | full-cycle email works for all 3 providers |
| 8 | Polish: per-account error banners, loading states, signatures, account settings UI, deploy.sh | shippable |

Each phase ends with a manual smoke test against real accounts. No phase
proceeds with the previous phase broken.

---

## Repository layout (mailstack)

```
mailstack/
├── PLAN.md                  (this doc)
├── go.mod
├── cmd/mailstack/main.go
├── server.go                # HTTP routes
├── accounts/                # accounts.db CRUD
│   ├── store.go
│   └── schema.go
├── backend/
│   ├── backend.go           # MailBackend interface, canonical types
│   ├── gmail.go
│   ├── graph.go
│   └── imap.go
├── apiauth/                 # client for the apiauth service
│   ├── client.go
│   └── tokens.go
├── unified/
│   └── merge.go             # cross-account merge for /api/unified
├── safe/
│   └── html.go              # bluemonday sanitization
└── deploy.sh                # build + systemd unit install/restart
```

## Tech stack

- Go 1.22+
- Sqlite via `modernc.org/sqlite` (matches apiauth)
- `google.golang.org/api/gmail/v1` + `golang.org/x/oauth2`
- `github.com/emersion/go-imap/v2` + `github.com/emersion/go-message`
- `github.com/emersion/go-smtp`
- `github.com/microcosm-cc/bluemonday` (HTML sanitize)
- Microsoft Graph: `net/http` direct, no SDK
- Frontend: same React + TS + Vite stack as the rest of dash; TipTap for
  composer; DOMPurify (already common in mail UIs); React Query (if dash uses
  it) for fetching

## Operational notes

- `mailstack` runs on `:8195` (loopback). systemd unit lives in
  `mailstack/deploy.sh` per the deploy-via-deploy.sh directive.
- Logs: same logstack pattern as other services (JSON to stdout → captured by
  systemd → forwarded to logstack).
- Health: `GET /health` returns `{ok: true, accounts: [{id, last_ok, last_err}]}`.
  Hooked into healthcheck service after phase 7.
- Backups: `~/.config/mailstack/accounts.db` is small; whatever scheme is used
  for apiauth's tokens.db should pick this up.

---

## What this plan does not cover

- Dashboard UX details beyond components — colors, exact spacing, animations.
- Account-add wizard wireframes.
- Failure-mode UX copy.
- Telemetry / metrics on mailstack itself.

These get refined inside their own phases when we hit them.
