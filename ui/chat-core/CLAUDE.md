# About chat-core

## What it is

`@kayushkin/chat-core` is the data layer under bridge-ui's chat (`bridge-ui/src/components/chat/`), the only chat on this host. It is a TypeScript library, not a service: no port, no database of its own, no deploy. **It holds data only and ships no React components** — what stays under `src/react/` is `ChatProvider`, the context and the hooks. Anything that draws (`RefChip`, `SignalCard`, `SessionSignals`) lives in bridge-ui; put new components there.

The client renders from memory and the network only reconciles in the background. That is what gives fast session switching, a warm cache of recent sessions, optimistic mutations, and **non-destructive** OTel/rollout dedup: a duplicate is annotated, never dropped.

`README.md` still says "dash's chat". The chat page moved into bridge-ui on 2026-09-10; dash only hosts it.

# How it works

## Layers

- `src/store/` — `ChatStore` (Zustand Maps) and its selectors. Every action renders from here.
- `src/reduce/` — `TurnReducer` folds the live event tail into turns; `otelDedup` annotates duplicates; `refChips` finds record ids in prose.
- `src/cache/` — `SessionCache` persists the store to IndexedDB so a cold boot paints with no network. `evict` keeps two LRU bounds: about 50 sessions' turns, and one sidebar page of the list.
- `src/sync/` — `SyncEngine` holds one session-list SSE stream and one active-session stream, plus a validator sweep that repairs stale sessions quietly. `sse.ts` uses `fetch` and a `ReadableStream`, not `EventSource`.
- `src/net/` — `ApiClient` and `types.ts`, the wire types.
- `src/boot/` — `Prefetcher`: the boot sequence, and hover and idle prefetch.
- `src/react/` — `ChatProvider` and the hooks: the public API.

Composer drafts and the sidebar filter live in `localStorage`, read synchronously when the store is built, so both are in the first paint.

## The wire contract

`docs/WIRE.md` is the contract with llm-bridge-server (`:8160`) and log-store. `src/net/types.ts` is its TypeScript source of truth, and the Go structs must serialize to the same JSON. Change both sides together. `docs/PUBLIC-API.md` is what a host page may import; keep the exports and that file in step.

Rules that are easy to break:

- **Nothing on the wire is lossy.** Dedup is an annotation on an `Entry` (`duplicate`, `primary`, `groupId`). The raw timeline must be able to rebuild every stored event.
- **The server owns a session's status, and `as_of` orders it.** A status arrives four ways, in no fixed order: the session's stream, the list stream, a summary page, the cache. The larger `as_of` wins, whatever arrived last. Every path that stores a `SessionSummary` goes through `withNewestStatus` (`src/store/sessionStatus.ts`). Do not derive a status on the client.
- **`Entry.origin` (`live` or `page`) is stamped by the client** and kept in the cache. A server must not send it.
- **Id lists go in a POST body.** `POST /sessions/summary` and `POST /sessions/validators` exist because a long query string makes nginx kill the whole HTTP/2 connection at about 11.5 KB of URL. `ApiClient` picks the encoding; a caller never does.
- **Message reads are always bounded.** `GET /sessions/{id}/messages` returns the last `limit` turns and pages older ones with `before`.

## The IndexedDB cache

`SessionCache` opens the database at `DB_VERSION`. An upgrade blocked by another open tab never settles, so the open has a timeout and the client carries on with no cache rather than hang. Bump `DB_VERSION` when a stored shape changes, and keep that timeout path working. The cache must never hold up the session-list stream (`test/cacheNeverBlocksTheStream.test.ts`).

# Access and operations

## How consumers get it

bridge-ui and dash both depend on it as `file:../chat-core`, a symlink to this main clone. **They bundle `dist/`, never `src/`**, and `dist/` is gitignored, so a consumer sees a change only after a build here. dash's `scripts/build-linked-libraries.mjs` rebuilds it from committed source before a dash build, and refuses when a file the build reads is modified, staged or untracked. So keep this clone on a clean `main`, and check `git status` here before building, so a rebuild does not replace the `dist/` another agent is working against.

Because the link carries its own `node_modules`, every consumer lists `react` and `react-dom` under `resolve.dedupe` in its Vite and vitest configs. Without it `ChatProvider`'s context is made by one copy of React and read by another: a blank page with no error. Use `dedupe`, not `alias`.

It pushes to `github.com/kayushkin/chat-core`. There is no `deploy.sh`; a change goes live when dash is deployed.

# Working in this repo

## Build and test

`npm install && npm run build && npm test`. `build` is `tsc -b` over `src/` only. `test` is vitest over `test/*.test.ts` in the node environment, with `fake-indexeddb` standing in for the browser's; it took about 25 seconds for 628 tests on 2026-09-20. `npm run check` is the assertion the nightly repo-node-guard runs, and it is `test` alone on purpose: the reason is in `package.json` under `//check`. `tsc` never type-checks the test tree; vitest is the only thing that reads it.

## The sabotage scripts

`scripts/sabotage-failure-value.py` applies one source mutation, runs the suite, and reports whether the suite went red; `scripts/scan-failure-value-tests.py` finds the tests to aim it at. Both write scratch output that is gitignored (`mutation-results.json`, `failure-value-tests.json`). The scorer mutates the working tree and reads the whole suite's exit code, so `scripts/tree_hold.py` allows one run per tree at a time. Do not run it in the main clone while anything builds from it; use a worktree.
