# Multi-stage, multi-target build for the llm-bridge stack.
#
# Targets:
#   server       — distroless, mock-only (smoke tests, no LLM creds)
#   server-full  — debian-slim + claude CLI + llm-bridge-claudecode wrapper
#                  so real claude_code sessions work. Used by compose default.
#   log-store    — distroless, durable event log + materialized history.
#   llmux        — token-auth UI that proxies /api/bridge/* to the server.
#
# The build context MUST be the parent directory holding every sibling
# repo side-by-side (the layout scripts/bootstrap.sh produces). Build any
# target from the parent dir, e.g.:
#   docker build -f llm-bridge-server/Dockerfile --target llmux \
#                -t llmux llm-bridge-server/..

# --- Go build stage -------------------------------------------------------

FROM golang:1.25 AS build
WORKDIR /src

# Sibling Go modules referenced by go.mod replace blocks across the stack.
COPY llm-bridge            /src/llm-bridge
COPY log-store             /src/log-store
COPY logstack              /src/logstack
COPY agent-store           /src/agent-store
COPY harness-store         /src/harness-store
COPY memory-store          /src/memory-store
COPY aiauth                /src/aiauth
COPY model-store           /src/model-store
COPY hook-store            /src/hook-store
# Optional siblings (server degrades gracefully without snapshot-store).
COPY bus                   /src/bus
COPY snapshot-store        /src/snapshot-store

# Harness wrappers we want bakeable into server-full.
COPY llm-bridge-claudecode /src/llm-bridge-claudecode

# Sibling store services packaged into the compose stack.
COPY auth-store            /src/auth-store
COPY kanban-store          /src/kanban-store
COPY usage-store           /src/usage-store

# llmux Go server (no sibling replace deps of its own — uses module proxy).
COPY llmux                 /src/llmux

COPY llm-bridge-server     /src/llm-bridge-server

WORKDIR /src/llm-bridge-server
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llm-bridge-server ./cmd/llm-bridge-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llm-bridge-mock   ./cmd/mock-harness

WORKDIR /src/llm-bridge-claudecode
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llm-bridge-claudecode .

WORKDIR /src/log-store
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/log-store ./cmd/log-store

WORKDIR /src/auth-store
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/auth-store ./cmd/auth-store

WORKDIR /src/kanban-store
# kanban-store uses mattn/go-sqlite3 which requires cgo; the other stores
# use pure-Go modernc. Building it with CGO_ENABLED=1 means the final
# kanban-store image needs a libc-bearing base instead of distroless.
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/kanban-store ./cmd/kanban-store

WORKDIR /src/usage-store
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/usage-store ./cmd/usage-store-server

WORKDIR /src/llmux
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llmux-server ./server

# distroless has no shell — bake a writeable /data fixture with the
# nonroot uid so the final stages can copy it in with correct ownership.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# --- frontend build stage ------------------------------------------------

FROM node:22-slim AS frontend
WORKDIR /src

# bridge-ui consumes llm-bridge/ts via file:../llm-bridge/ts, llmux
# consumes bridge-ui via file:../bridge-ui. Both file: deps resolve
# relative to the package directory, so the on-disk layout has to match.
COPY llm-bridge  /src/llm-bridge
COPY bridge-ui   /src/bridge-ui
COPY llmux       /src/llmux

# bridge-ui ships a committed dist/ (it's the npm package's `main`), so
# we skip rebuilding it inside the image and rely on what's checked in.
# This also dodges any uncommitted-source tsc churn in the working tree.

WORKDIR /src/llmux
RUN npm install --no-audit --no-fund && npm run build
RUN mkdir -p /out && cp -r dist /out/llmux-dist

# --- server target (distroless, mock-only) -------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS server
WORKDIR /app
COPY --from=build /out/llm-bridge-server /usr/local/bin/llm-bridge-server
COPY --from=build /out/llm-bridge-mock   /usr/local/bin/llm-bridge-mock
COPY --from=build /src/llm-bridge-server/images /app/images

ENV LLMBRIDGE_LISTEN_ADDR=:8160 \
    LLMBRIDGE_IMAGES_DIR=/app/images \
    LLMBRIDGE_DB_PATH=/data/bridge.db \
    LLMBRIDGE_AGENT_DB=/data/agents.db \
    LLMBRIDGE_MEMORY_DB=/data/memory.db \
    LLMBRIDGE_HARNESS_DB=/data/harness.db \
    LLMBRIDGE_HOOK_DB=/data/hooks.db \
    LLMBRIDGE_MODEL_STORE_DB=/data/models.db \
    LLMBRIDGE_SNAPSHOT_DB=/data/snapshots.db \
    LLMBRIDGE_SNAPSHOT_GIT=/data/snapshots.git \
    LLMBRIDGE_BRIDGE_PREFS=/data/bridge-prefs.json \
    LLMBRIDGE_CONFORMANCE_PATH=/data/conformance.json \
    PATH=/usr/local/bin

COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8160
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/llm-bridge-server"]

# --- server-full target (debian-slim + claude CLI + claudecode wrapper) --

FROM node:22-slim AS server-full
WORKDIR /app

# Install the upstream claude CLI globally. The harness wrapper exec's
# this binary, so it must resolve via PATH from inside the container.
RUN npm install -g @anthropic-ai/claude-code --no-audit --no-fund \
 && claude --version

COPY --from=build /out/llm-bridge-server      /usr/local/bin/llm-bridge-server
COPY --from=build /out/llm-bridge-mock        /usr/local/bin/llm-bridge-mock
COPY --from=build /out/llm-bridge-claudecode  /usr/local/bin/llm-bridge-claudecode
COPY --from=build /src/llm-bridge-server/images /app/images

ENV LLMBRIDGE_LISTEN_ADDR=:8160 \
    LLMBRIDGE_IMAGES_DIR=/app/images \
    LLMBRIDGE_DB_PATH=/data/bridge.db \
    LLMBRIDGE_AGENT_DB=/data/agents.db \
    LLMBRIDGE_MEMORY_DB=/data/memory.db \
    LLMBRIDGE_HARNESS_DB=/data/harness.db \
    LLMBRIDGE_HOOK_DB=/data/hooks.db \
    LLMBRIDGE_MODEL_STORE_DB=/data/models.db \
    LLMBRIDGE_SNAPSHOT_DB=/data/snapshots.db \
    LLMBRIDGE_SNAPSHOT_GIT=/data/snapshots.git \
    LLMBRIDGE_BRIDGE_PREFS=/data/bridge-prefs.json \
    LLMBRIDGE_CONFORMANCE_PATH=/data/conformance.json \
    HOME=/data/home \
    PATH=/usr/local/bin:/usr/bin:/bin

RUN mkdir -p /data/home /data && chown -R 1000:1000 /data
EXPOSE 8160
VOLUME ["/data"]
# Run as a fixed uid so a host-side bind mount of ~/.claude lines up.
# Override at runtime with `--user $(id -u):$(id -g)` if your host uid
# differs from 1000.
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/llm-bridge-server"]

# --- log-store target ----------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS log-store
COPY --from=build /out/log-store /usr/local/bin/log-store

ENV LOG_STORE_LISTEN_ADDR=:8175 \
    LOG_STORE_DB_PATH=/data/log-store.db \
    LOG_STORE_LOGSTACK_URL=http://logstack:8081

COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8175
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/log-store"]

# --- auth-store target --------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS auth-store
COPY --from=build /out/auth-store /usr/local/bin/auth-store
COPY --from=build --chown=65532:65532 /out/data /data
ENV HOME=/data
EXPOSE 8303
VOLUME ["/data"]
USER nonroot:nonroot
# --bind-all so other containers in the compose network can reach this.
# Default flags target the in-container HOME=/data so the SQLite db lands
# on the persistent volume.
ENTRYPOINT ["/usr/local/bin/auth-store", "--bind-all", "--port=8303", "--db=/data/.config/auth-store/auth.db"]

# --- kanban-store target ------------------------------------------------

# debian-slim (not distroless) because mattn/go-sqlite3 needs libc at
# runtime. We still chown a /data directory for the nonroot uid.
FROM debian:bookworm-slim AS kanban-store
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 65532 nonroot && useradd -u 65532 -g 65532 -m nonroot \
 && mkdir -p /data && chown 65532:65532 /data
COPY --from=build /out/kanban-store /usr/local/bin/kanban-store
ENV KANBAN_PORT=8305 \
    KANBAN_DB=/data/kanban.db \
    HOME=/data
EXPOSE 8305
VOLUME ["/data"]
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kanban-store"]

# --- usage-store target -------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS usage-store
COPY --from=build /out/usage-store /usr/local/bin/usage-store
COPY --from=build --chown=65532:65532 /out/data /data
ENV USAGE_STORE_LISTEN_ADDR=:8185 \
    USAGE_STORE_DB=/data/usage.db \
    USAGE_STORE_TOKENS_DB=/data/tokens.db \
    HOME=/data
EXPOSE 8185
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/usage-store"]

# --- llmux target (token-auth UI) ---------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS llmux
COPY --from=build    /out/llmux-server  /usr/local/bin/llmux-server
COPY --from=frontend /out/llmux-dist    /app/dist

ENV LLMUX_PORT=8170 \
    LLM_BRIDGE_URL=http://llm-bridge-server:8160

WORKDIR /app
EXPOSE 8170
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/llmux-server"]
