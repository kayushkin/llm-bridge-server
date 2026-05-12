# Multi-stage build for llm-bridge-server + log-store + mock-harness.
#
# The repo's go.mod uses `replace ../X` for ten sibling repos, so this
# image MUST be built from the parent directory that holds all those
# checkouts side-by-side (the same layout scripts/bootstrap.sh produces).
#
# Build the server alone:
#   docker build -f llm-bridge-server/Dockerfile -t llm-bridge-server llm-bridge-server/..
#
# Build the log-store sidecar (uses the same context):
#   docker build -f llm-bridge-server/Dockerfile --target log-store -t log-store llm-bridge-server/..
#
# Or use docker-compose.yml which sets context + target automatically.

FROM golang:1.25 AS build
WORKDIR /src

# Copy every sibling that go.mod references, plus this repo last.
COPY llm-bridge       /src/llm-bridge
COPY log-store        /src/log-store
# logstack is transitively required by log-store's go.mod replace block.
COPY logstack         /src/logstack
COPY agent-store      /src/agent-store
COPY harness-store    /src/harness-store
COPY memory-store     /src/memory-store
COPY aiauth           /src/aiauth
COPY model-store      /src/model-store
COPY hook-store       /src/hook-store
# bus + snapshot-store are optional siblings — copy if present.
COPY bus              /src/bus
COPY snapshot-store   /src/snapshot-store

COPY llm-bridge-server /src/llm-bridge-server

WORKDIR /src/llm-bridge-server
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llm-bridge-server ./cmd/llm-bridge-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llm-bridge-mock   ./cmd/mock-harness

WORKDIR /src/log-store
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/log-store ./cmd/log-store

# distroless has no shell, so prepare a writeable /data fixture here for
# the final stages to copy in with the right ownership.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# --- server image ----------------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS server
WORKDIR /app
COPY --from=build /out/llm-bridge-server /usr/local/bin/llm-bridge-server
# Ship mock-harness alongside the server so smoke tests against the image
# work without an extra step.
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

# --- log-store image ------------------------------------------------------

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
