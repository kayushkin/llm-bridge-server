#!/usr/bin/env bash
# A self-contained llm-bridge-server for trying a build out, on the machine this
# script runs on. It installs as systemd user units, keeps every file under one
# folder, listens on loopback only, and touches nothing of a real install: its
# processes run with HOME pointed inside that folder and a PATH that does not
# reach the machine's own harness wrappers.
#
#   scripts/test-instance.sh install <folder of binaries>
#   scripts/test-instance.sh status
#   scripts/test-instance.sh service-token      print the token for API calls
#   scripts/test-instance.sh uninstall          stop it and delete the folder
#
# <folder of binaries> holds llm-bridge-server, llm-bridge-runner,
# llm-bridge-mock, log-store, tool-store, principal-store and grant-store, built
# for this machine. The machine itself needs no Go or Node, only systemd user
# units, curl, jq and sqlite3. Running install again replaces the binaries and
# restarts; settings, secrets and data are kept.
#
# What it runs:
#   the server, with its front end at /ui/ and demo login switched on
#   log-store, tool-store, principal-store and grant-store, each its own unit
#   a runner enrolled with this server, offering the mock harness
#
# kanban-store, reference-store and permission-store are not installed. The
# server requires their addresses, so they are set to a port nothing listens
# on, and the pages that need them answer an error.
#
# Demo login lets anyone who can reach the server sign in as any person, with
# no password. That is acceptable here only because the server listens on
# loopback. Never put this instance behind a public address.
set -euo pipefail

ROOT="${BRIDGE_TEST_INSTANCE_DIR:-$HOME/.local/opt/llm-bridge-test}"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_PREFIX="llm-bridge-test"
STORES=(log-store tool-store principal-store grant-store)
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

fail() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '==> %s\n' "$*"; }

load_settings() {
  [ -f "$ROOT/env/ports.env" ] || fail "no test instance at $ROOT; run: $0 install <folder of binaries>"
  # shellcheck disable=SC1091
  . "$ROOT/env/ports.env"
  BASE="http://127.0.0.1:$BRIDGE_PORT"
}

service_token() {
  sed -n 's/^LLMBRIDGE_SERVICE_TOKEN=//p' "$ROOT/env/secrets.env"
}

bridge_curl() {
  curl -sS -H "X-LLM-Bridge-Service-Token: $(service_token)" "$@"
}

wait_for() { # <what> <url>
  for _ in $(seq 1 100); do
    if curl -fsS -o /dev/null "$2" 2>/dev/null; then return 0; fi
    sleep 0.2
  done
  fail "$1 did not answer at $2; see: journalctl --user -u '$UNIT_PREFIX*' -n 50"
}

random_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

write_unit() { # <unit name> <description> <binary> <environment file>... ; extra unit lines on stdin
  local name="$1" description="$2" binary="$3"; shift 3
  {
    echo "[Unit]"
    echo "Description=$description"
    echo
    echo "[Service]"
    echo "ExecStart=$ROOT/bin/$binary"
    echo "WorkingDirectory=$ROOT"
    echo "Environment=HOME=$ROOT/home"
    echo "Environment=XDG_CONFIG_HOME=$ROOT/home/.config"
    echo "Environment=XDG_DATA_HOME=$ROOT/home/.local/share"
    echo "Environment=XDG_STATE_HOME=$ROOT/home/.local/state"
    echo "Environment=PATH=$ROOT/bin:/usr/bin:/bin"
    for file in "$@"; do echo "EnvironmentFile=$file"; done
    cat
    echo "Restart=on-failure"
    echo "RestartSec=2"
    echo
    echo "[Install]"
    echo "WantedBy=default.target"
  } > "$UNIT_DIR/$name.service"
}

install_instance() {
  local binaries="${1:-}"
  [ -n "$binaries" ] && [ -d "$binaries" ] || fail "usage: $0 install <folder of binaries>"
  for tool in systemctl curl jq sqlite3; do
    command -v "$tool" >/dev/null 2>&1 || fail "this machine has no $tool"
  done
  for binary in llm-bridge-server llm-bridge-runner llm-bridge-mock "${STORES[@]}"; do
    [ -x "$binaries/$binary" ] || fail "$binaries/$binary is missing or not executable"
  done

  step "laying out $ROOT"
  mkdir -p "$ROOT"/{bin,data,env,home,runner,runner-assets} "$UNIT_DIR"
  chmod 700 "$ROOT" "$ROOT/env"

  if [ ! -f "$ROOT/env/ports.env" ]; then
    local base="${BRIDGE_TEST_PORT_BASE:-18160}"
    cat > "$ROOT/env/ports.env" <<EOF
BRIDGE_PORT=$base
LOG_STORE_PORT=$((base + 15))
TOOL_STORE_PORT=$((base + 142))
PRINCIPAL_STORE_PORT=$((base + 154))
GRANT_STORE_PORT=$((base + 155))
EOF
  fi
  load_settings
  local nowhere="http://127.0.0.1:1"

  if [ ! -f "$ROOT/env/secrets.env" ]; then
    step "making this instance's own secrets"
    ( umask 077; cat > "$ROOT/env/secrets.env" <<EOF
LLMBRIDGE_SERVICE_TOKEN=$(random_hex 32)
LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY=$(random_hex 32)
GRANT_STORE_SERVICE_TOKEN=$(random_hex 32)
KANBAN_STORE_SERVICE_TOKEN=$(random_hex 32)
EOF
    )
  fi

  cat > "$ROOT/env/server.env" <<EOF
LLMBRIDGE_LISTEN_ADDR=127.0.0.1:$BRIDGE_PORT
LLMBRIDGE_DEMO_LOGIN_ENABLED=true
LLMBRIDGE_DB_PATH=$ROOT/data/bridge.db
LLMBRIDGE_AGENT_DB=$ROOT/data/agents.db
LLMBRIDGE_MEMORY_DB=$ROOT/data/memory.db
LLMBRIDGE_HARNESS_DB=$ROOT/data/harness.db
LLMBRIDGE_HOOK_DB=$ROOT/data/hooks.db
LLMBRIDGE_MODEL_STORE_DB=$ROOT/data/models.db
LLMBRIDGE_SNAPSHOT_DB=$ROOT/data/snapshots.db
LLMBRIDGE_SNAPSHOT_GIT=$ROOT/data/snapshots.git
LLMBRIDGE_OPERATIONS_DB=$ROOT/data/operations.db
LLMBRIDGE_BRIDGE_PREFS=$ROOT/data/bridge-prefs.json
LLMBRIDGE_CONFORMANCE_PATH=$ROOT/data/conformance.json
LLMBRIDGE_IMAGES_DIR=$ROOT/data/images
LLMBRIDGE_SESSION_FILES_DIR=$ROOT/data/session-files
LLMBRIDGE_RUNNER_ASSETS_DIR=$ROOT/runner-assets
LLMBRIDGE_LOG_STORE_URL=http://127.0.0.1:$LOG_STORE_PORT
LLMBRIDGE_TOOL_STORE_URL=http://127.0.0.1:$TOOL_STORE_PORT
LLMBRIDGE_PRINCIPAL_STORE_URL=http://127.0.0.1:$PRINCIPAL_STORE_PORT
LLMBRIDGE_GRANT_STORE_URL=http://127.0.0.1:$GRANT_STORE_PORT
LLMBRIDGE_KANBAN_STORE_URL=$nowhere
LLMBRIDGE_REFERENCE_STORE_URL=$nowhere
LLMBRIDGE_PERMISSION_STORE_URL=$nowhere
EOF

  # One file per store: a store refuses to start when a variable carrying its
  # own prefix is set that it does not declare, so grant-store's
  # PRINCIPAL_STORE_URL must not reach principal-store.
  cat > "$ROOT/env/log-store.env" <<EOF
LOG_STORE_LISTEN_ADDR=127.0.0.1:$LOG_STORE_PORT
LOG_STORE_DB_PATH=$ROOT/data/log-store.db
LOG_STORE_LOGSTACK_URL=$nowhere
EOF
  echo "TOOL_STORE_ADDR=127.0.0.1:$TOOL_STORE_PORT" > "$ROOT/env/tool-store.env"
  echo "PRINCIPAL_STORE_ADDR=127.0.0.1:$PRINCIPAL_STORE_PORT" > "$ROOT/env/principal-store.env"
  cat > "$ROOT/env/grant-store.env" <<EOF
GRANT_STORE_ADDR=127.0.0.1:$GRANT_STORE_PORT
PRINCIPAL_STORE_URL=http://127.0.0.1:$PRINCIPAL_STORE_PORT
TOOL_STORE_URL=http://127.0.0.1:$TOOL_STORE_PORT
LLM_BRIDGE_URL=http://127.0.0.1:$BRIDGE_PORT
SKILL_STORE_URL=$nowhere
KANBAN_STORE_URL=$nowhere
EOF
  rm -f "$ROOT/env/stores.env"

  step "stopping what is running, to replace the binaries"
  systemctl --user stop "$UNIT_PREFIX-runner.service" "$UNIT_PREFIX.service" 2>/dev/null || true
  for store in "${STORES[@]}"; do systemctl --user stop "$UNIT_PREFIX-$store.service" 2>/dev/null || true; done

  step "installing binaries"
  for binary in llm-bridge-server llm-bridge-runner llm-bridge-mock "${STORES[@]}"; do
    install -m 755 "$binaries/$binary" "$ROOT/bin/$binary"
  done
  # The server offers runners the build in this folder. It is the same file the
  # test runner runs, so the two never disagree about which is newer.
  install -m 755 "$binaries/llm-bridge-runner" "$ROOT/runner-assets/llm-bridge-runner-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"

  step "writing units"
  for store in "${STORES[@]}"; do
    # Only grant-store presents tokens to the others; the rest are given none.
    if [ "$store" = grant-store ]; then
      write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" "$ROOT/env/secrets.env" </dev/null
    else
      write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" </dev/null
    fi
  done
  write_unit "$UNIT_PREFIX" "llm-bridge test instance: the server" llm-bridge-server "$ROOT/env/server.env" "$ROOT/env/secrets.env" <<EOF
EOF
  write_unit "$UNIT_PREFIX-runner" "llm-bridge test instance: its runner" "llm-bridge-runner run" /dev/null <<EOF
Environment=LLMBRIDGE_RUNNER_CONFIG=$ROOT/runner/config.json
EOF
  # /dev/null stood in for "no environment file"; systemd would refuse it.
  sed -i '\|^EnvironmentFile=/dev/null$|d' "$UNIT_DIR/$UNIT_PREFIX-runner.service"
  systemctl --user daemon-reload

  step "starting the stores"
  for store in "${STORES[@]}"; do systemctl --user enable --now "$UNIT_PREFIX-$store.service" >/dev/null 2>&1; done
  wait_for principal-store "http://127.0.0.1:$PRINCIPAL_STORE_PORT/kinds"
  wait_for tool-store "http://127.0.0.1:$TOOL_STORE_PORT/tools"
  wait_for log-store "http://127.0.0.1:$LOG_STORE_PORT/health"

  step "starting the server"
  systemctl --user enable --now "$UNIT_PREFIX.service" >/dev/null 2>&1
  wait_for "the server" "$BASE/health"

  # A session needs a model that resolves, and a new registry is empty. The
  # server has no route that writes one, so the row goes into its database.
  if [ "$(sqlite3 "$ROOT/data/models.db" "SELECT count(*) FROM model_roles WHERE role = 'default'")" = "0" ]; then
    step "giving the model registry a default model for the mock harness"
    sqlite3 "$ROOT/data/models.db" \
      "INSERT INTO providers (id, name) VALUES ('test-instance', 'test instance');
       INSERT INTO models (id, provider, name) VALUES ('test-instance-model', 'test-instance', 'test instance model');
       INSERT INTO model_roles (role, model_id) VALUES ('default', 'test-instance-model');"
  fi

  local principal_store="http://127.0.0.1:$PRINCIPAL_STORE_PORT"
  if [ "$(curl -fsS "$principal_store/principals" | jq '[.[] | select(.kind == "human")] | length')" = "0" ]; then
    step "making an administrator to sign in as"
    local principal_id
    principal_id="$(curl -fsS -X POST "$principal_store/principals" -H 'Content-Type: application/json' \
      -d '{"kind":"human","display_name":"Test operator"}' | jq -r '.id')"
    curl -fsS -o /dev/null -X PATCH "$principal_store/principals/$principal_id" -H 'Content-Type: application/json' -d '{"is_administrator":true}'
  fi

  if [ ! -f "$ROOT/runner/config.json" ]; then
    step "enrolling the runner"
    local passphrase
    passphrase="$( set -a; . "$ROOT/env/server.env"; . "$ROOT/env/secrets.env"; set +a
                   HOME="$ROOT/home" "$ROOT/bin/llm-bridge-server" mint-enroll | sed -n 's/^passphrase: *//p' )"
    [ -n "$passphrase" ] || fail "the server printed no enrollment passphrase"
    HOME="$ROOT/home" PATH="$ROOT/bin:/usr/bin:/bin" LLMBRIDGE_RUNNER_CONFIG="$ROOT/runner/config.json" \
      "$ROOT/bin/llm-bridge-runner" enroll --server "$BASE" --passphrase "$passphrase" --name "test-instance-runner"
  fi
  step "starting the runner"
  systemctl --user enable --now "$UNIT_PREFIX-runner.service" >/dev/null 2>&1

  status_instance
}

status_instance() {
  load_settings
  echo "test instance in $ROOT"
  for unit in "$UNIT_PREFIX" "$UNIT_PREFIX-runner" $(printf "$UNIT_PREFIX-%s " "${STORES[@]}"); do
    printf '  %-36s %s\n' "$unit" "$(systemctl --user is-active "$unit.service" 2>/dev/null || true)"
  done
  echo "  server:     $BASE   (front end: $BASE/ui/)"
  if curl -fsS -o /dev/null "$BASE/health" 2>/dev/null; then
    echo "  machines:   $(bridge_curl "$BASE/machines" | jq -r '[.[] | "\(.name) (\(.transport))"] | join(", ")')"
    echo "  instances:  $(bridge_curl "$BASE/instances" | jq -r '[.[] | "\(.id) \(.harness_type // .harness)"] | join(", ")')"
    echo "  sign in as: $(curl -fsS "http://127.0.0.1:$PRINCIPAL_STORE_PORT/principals" | jq -r '[.[] | select(.kind == "human") | "\(.id) \(.display_name)"] | join(", ")')"
  else
    echo "  the server is not answering"
  fi
}

uninstall_instance() {
  step "stopping and removing the units"
  for unit in "$UNIT_PREFIX-runner" "$UNIT_PREFIX" $(printf "$UNIT_PREFIX-%s " "${STORES[@]}"); do
    systemctl --user disable --now "$unit.service" >/dev/null 2>&1 || true
    rm -f "$UNIT_DIR/$unit.service"
  done
  systemctl --user daemon-reload
  # ROOT is only ever this script's own folder: refuse anything that does not
  # carry its marker before deleting.
  [ -f "$ROOT/env/ports.env" ] || fail "$ROOT does not look like a test instance; not deleting it"
  step "deleting $ROOT"
  rm -rf "$ROOT"
}

case "${1:-}" in
  install)       install_instance "${2:-}" ;;
  status)        status_instance ;;
  service-token) load_settings; service_token ;;
  uninstall)     uninstall_instance ;;
  *)             echo "usage: $0 install <folder of binaries> | status | service-token | uninstall" >&2; exit 2 ;;
esac
