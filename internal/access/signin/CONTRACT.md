# sign-in-store contract

How people sign in, and the sessions that result. A package the server mounts: llm-bridge-server serves every route below under `/sign-in/`, the same paths behind the prefix, and keeps the records in the Postgres module `sign_in` (role `sign_in_module`), built by `migrations/*.sql`. Who anybody is belongs to principal-store, which the server answers in process (`PrincipalDirectory`); people-store, auth-store and multichat are reached over HTTP. `ImportStandaloneDatabase` copies a standalone sign-in-store's SQLite file into an empty module, keeping every id, hash and time. Design note `f3d492d9-a6b0-4ea1-bd8a-46f28ee3542b`.

## Records

**Sign-in link** (`sign_in_link_000001`): a one-time secret that signs one principal in. It works once, within 7 days. Only the secret's SHA-256 is kept.

| Field | Meaning |
|---|---|
| `principal_id` | who it signs in |
| `sent_by` | `operator` (made by an administrator and handed over), `discord` or `phone` (asked for by handle and sent to the person's own chat), or `host_shell` (made from a shell on the server's host with `MakeSignInLinkFromHostShell`, which needs only the database and principal-store) |
| `made_by` | the administrator, for an `operator` link |
| `created_at`, `expires_at`, `redeemed_at` | epoch seconds; `redeemed_at` 0 until spent |
| `session_id` | the session the redemption opened |

**Session** (`session_000001`): one browser signed in as one principal, for 180 days from sign-in. Only the cookie secret's SHA-256 is kept. Never deleted; revoking stamps it.

| Field | Meaning |
|---|---|
| `principal_id`, `method` (`sign_in_link` or `discord`), `sign_in_link_id` | who, and how they signed in |
| `created_at`, `expires_at`, `last_seen_at` | epoch seconds; `last_seen_at` written at most once a minute |
| `revoked_at`, `revoked_by`, `revoke_reason` | who ended it and why; sign-out records the person themselves |
| `user_agent`, `remote_address` | the browser, as it signed in |

## Who may sign in

An enabled principal-store principal of kind `human`. A group, a contact or a peer cannot. principal-store's answer is cached 30 seconds, so disabling someone signs them out within 30 seconds.

## The cookie

`sign_in_session`, set for the configured cookie domain (`HandlersConfig.CookieDomain`) and every name under it, `HttpOnly`, `Secure`, `SameSite=Lax`, expiring with the session.

## Routes a browser reaches

| Method | Path | |
|---|---|---|
| POST | `/link-requests` | `{"handle"}`: a Discord username, or a phone number in international form. **202 with the same message whether or not the handle has an account.** When a person with an enabled principal owns it, a link goes to their one-to-one chat on that kind of account through multichat (Discord for a username; SMS, WhatsApp or Signal for a number). At most 5 an hour per address and per handle, then 429 |
| POST | `/redeem` | `{"secret"}`, the part of the link after `#`: 200 with the session (below) and the cookie set; 410 `link_not_usable` for used, expired or unknown; 403 `may_not_sign_in` when the principal is now disabled or not a person (the link is not spent) |
| GET | `/session` | 200 `{session_id, principal_id, display_name, is_administrator, group_ids, method, expires_at}` and header `X-Signed-In-Principal-Id`; 401 with code `not_signed_in`, `session_unknown`, `session_revoked`, `session_expired`, `principal_unknown` or `principal_may_not_sign_in`; 502 when principal-store cannot answer. Apps and Caddy's `forward_auth` call it with the browser's cookies |
| POST | `/sign-out` | revokes the browser's session and clears the cookie; 200 even with no session |

## Sign in with Discord

| Method | Path | |
|---|---|---|
| GET | `/discord/start` | `?purpose=sign_in` (default) or `link`, `&return_to=` an https address on the cookie domain (default the sign-in page; anything else 400). `link` needs a live session (401 otherwise). Records an attempt, sets the `sign_in_discord_attempt` cookie, and redirects to Discord with scope `identify`. 503 when no Discord application is set |
| GET | `/discord/callback` | Discord sends the browser back here. Spends the attempt (once, within 10 minutes, only in the browser holding its cookie), trades the code for the Discord user id, then: **sign_in** finds the principal bound to it in principal-store (`provider=discord`, empty tenant) and opens a session (`method` `discord`); **link** binds it to the principal that started the link, `linked_by` themselves. Redirects to `return_to`, with `?discord_linked=1` after a link, or `?sign_in_error=` one of `discord_denied`, `discord_attempt_not_usable` (then to the sign-in page), `discord_unavailable`, `discord_not_linked`, `may_not_sign_in`, `discord_already_linked`, `signed_out_while_linking` |

## Routes the operator reaches

Each acts for the administrator the server that mounts it puts in the request context (`ContextWithAdministrator`), from a credential it verified; no header names the administrator. The route checks again with principal-store that the principal exists, is enabled and is an administrator, and records its id. None in the context, or one that is not an enabled administrator, is 403 `administrator_required`.

| Method | Path | |
|---|---|---|
| POST | `/sign-in-links` | `{"principal_id"}`: 201 `{link, url, display_name}`. The `url` holds the secret, shown this once |
| GET | `/sign-in-links` | `?principal_id=&limit=` (1–1000, default 100), newest first; never a secret |
| GET | `/sessions` | the same query, newest first; never a secret |
| POST | `/sessions/{id}/revoke` | `{"reason"}`, required: 200 with the session; 409 when revoked before |
| POST | `/principals/{id}/discord-messages` | `{"body"}`: sends it to the one-to-one Discord chat of the person in people-store pointing at the principal, through multichat, so from the operator's own account; the body must say who it is from. 200 `{sent, person_id}`; 404 `unknown_principal` or `no_person`; 409 `no_chat` when there is no one-to-one Discord chat; 502 when people-store, principal-store or multichat fails |

## Open

`GET /health`.
