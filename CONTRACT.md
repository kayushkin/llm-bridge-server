# sign-in-store contract

`127.0.0.1:8328`. How people sign in to dash, kayushkin.com and watch.kayushkin.com, and the sessions that result. Who anybody is belongs to principal-store; this service asks it on every session check. Design note `f3d492d9-a6b0-4ea1-bd8a-46f28ee3542b`.

## Records

**Sign-in link** (`sign_in_link_000001`): a one-time secret that signs one principal in. It works once, within 7 days. Only the secret's SHA-256 is kept.

| Field | Meaning |
|---|---|
| `principal_id` | who it signs in |
| `sent_by` | `operator` (made by an administrator and handed over), `discord` or `phone` (asked for by handle and sent to the person's own chat) |
| `made_by` | the administrator, for an `operator` link |
| `created_at`, `expires_at`, `redeemed_at` | epoch seconds; `redeemed_at` 0 until spent |
| `session_id` | the session the redemption opened |

**Session** (`session_000001`): one browser signed in as one principal, for 180 days from sign-in. Only the cookie secret's SHA-256 is kept. Never deleted; revoking stamps it.

| Field | Meaning |
|---|---|
| `principal_id`, `method` (`sign_in_link`), `sign_in_link_id` | who, and how they signed in |
| `created_at`, `expires_at`, `last_seen_at` | epoch seconds; `last_seen_at` written at most once a minute |
| `revoked_at`, `revoked_by`, `revoke_reason` | who ended it and why; sign-out records the person themselves |
| `user_agent`, `remote_address` | the browser, as it signed in |

## Who may sign in

An enabled principal-store principal of kind `human`. A group, a contact or a peer cannot. principal-store's answer is cached 30 seconds, so disabling someone signs them out within 30 seconds.

## The cookie

`sign_in_session`, set for `SIGN_IN_STORE_COOKIE_DOMAIN` (`kayushkin.com`) and every name under it, `HttpOnly`, `Secure`, `SameSite=Lax`, expiring with the session.

## Routes a browser reaches (through nginx)

| Method | Path | |
|---|---|---|
| POST | `/link-requests` | `{"handle"}`: a Discord username, or a phone number in international form. **202 with the same message whether or not the handle has an account.** When a person with an enabled principal owns it, a link goes to their one-to-one chat on that kind of account through multichat (Discord for a username; SMS, WhatsApp or Signal for a number). At most 5 an hour per address and per handle, then 429 |
| POST | `/redeem` | `{"secret"}`, the part of the link after `#`: 200 with the session (below) and the cookie set; 410 `link_not_usable` for used, expired or unknown; 403 `may_not_sign_in` when the principal is now disabled or not a person (the link is not spent) |
| GET | `/session` | 200 `{session_id, principal_id, display_name, is_administrator, group_ids, method, expires_at}` and header `X-Signed-In-Principal-Id`; 401 with code `not_signed_in`, `session_unknown`, `session_revoked`, `session_expired`, `principal_unknown` or `principal_may_not_sign_in`; 502 when principal-store cannot answer. Apps and Caddy's `forward_auth` call it with the browser's cookies |
| POST | `/sign-out` | revokes the browser's session and clears the cookie; 200 even with no session |

## Routes the operator reaches (through dash)

Each needs `X-Sign-In-Store-Service-Token` and `X-Principal-Id` naming an enabled administrator, whose id is recorded. No token or a wrong one is 401; no administrator is 403.

| Method | Path | |
|---|---|---|
| POST | `/sign-in-links` | `{"principal_id"}`: 201 `{link, url, display_name}`. The `url` holds the secret, shown this once |
| GET | `/sign-in-links` | `?principal_id=&limit=` (1–1000, default 100), newest first; never a secret |
| GET | `/sessions` | the same query, newest first; never a secret |
| POST | `/sessions/{id}/revoke` | `{"reason"}`, required: 200 with the session; 409 when revoked before |

## Open

`GET /health`, `GET /settings` (secrets shown only as set or unset).
