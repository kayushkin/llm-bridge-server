# rule-store

> **In llm-bridge-server.** This copy runs inside llm-bridge-server on a
> Postgres module (`rules`), not as a service of its own: its routes are the
> server's `/rule-store/<path>` (`CONTRACT.md`'s `<path>`), operator only. It
> reads sessions and their feed from the server and cards and entity tags from
> kanban-store, both in process as the internal service, so it has no address
> or token of its own and `GET /settings` is gone (the server's own settings
> describe it). The reactor runs only while `rule_store.reactor.enabled` is on;
> off, nothing is decided, an activation is not checked, and `/health` says
> why under `reactor.switched_off_because`. The sweep interval is
> `rule_store.sweep_interval`. `llm-bridge-server import-rule-store -sqlite
> <copy>` copies a standalone rule-store in, keeping every id. Its command,
> unit and `deploy.sh` are gone from this copy; what follows about SQLite and
> the unit describes the standalone service.

Rules that react to changes in other services, held as data and run against
llm-bridge-server's session feed. Every rule runs in shadow mode until it has
been compared with the binary it replaces. See `CONTRACT.md`.
