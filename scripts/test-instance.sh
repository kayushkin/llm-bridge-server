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
#   scripts/test-instance.sh use-machine-logins run real Claude Code and codex
#                                               on this machine's own logins
#
# <folder of binaries> holds llm-bridge-server, llm-bridge-runner,
# llm-bridge-mock and the seven stores below, built for this machine. The machine itself needs no Go or Node, only systemd user
# units, curl, jq and sqlite3. Running install again replaces the binaries and
# restarts; settings, secrets and data are kept.
#
# What it runs:
#   the server, with its front end at /ui/ and demo login switched on
#   log-store, tool-store, principal-store, grant-store, kanban-store,
#   reference-store and permission-store, each its own unit
#   a runner enrolled with this server, offering the mock harness
#
# Every address a store would otherwise default to is set here, because the
# defaults are a real install's ports, and on a machine with a real runner
# 127.0.0.1:8160 is a tunnel to the real server. A service the test instance
# does not run (noteboard, bundle-store, auth-store, skill-store) is set to a
# port nothing listens on, and what needs it answers an error.
#
# Demo login lets anyone who can reach the server sign in as any person, with
# no password. That is acceptable here only because the server listens on
# loopback. Never put this instance behind a public address.
set -euo pipefail

ROOT="${BRIDGE_TEST_INSTANCE_DIR:-$HOME/.local/opt/llm-bridge-test}"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_PREFIX="llm-bridge-test"
STORES=(log-store tool-store principal-store grant-store kanban-store reference-store permission-store)
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

  # Ports keep a real install's offsets from the server's port. A key already
  # in ports.env keeps its value, so an instance installed before a store was
  # added gets that store's port without the others moving.
  local base
  base="$(sed -n 's/^BRIDGE_PORT=//p' "$ROOT/env/ports.env" 2>/dev/null)"
  base="${base:-${BRIDGE_TEST_PORT_BASE:-18160}}"
  touch "$ROOT/env/ports.env"
  add_port() { # <key> <port>
    grep -q "^$1=" "$ROOT/env/ports.env" || echo "$1=$2" >> "$ROOT/env/ports.env"
  }
  add_port BRIDGE_PORT "$base"
  add_port LOG_STORE_PORT $((base + 15))
  add_port TOOL_STORE_PORT $((base + 142))
  add_port PRINCIPAL_STORE_PORT $((base + 154))
  add_port GRANT_STORE_PORT $((base + 155))
  add_port PERMISSION_STORE_PORT $((base + 144))
  add_port KANBAN_STORE_PORT $((base + 145))
  add_port REFERENCE_STORE_PORT $((base + 183))
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

  # reference-store's token is for its own callers. Nothing here presents it,
  # but the store will not start without one. It has a file of its own, so it
  # reaches no other process.
  if [ ! -f "$ROOT/env/reference-store-secrets.env" ]; then
    ( umask 077; echo "REFERENCE_STORE_SERVICE_TOKEN=$(random_hex 32)" > "$ROOT/env/reference-store-secrets.env" )
  fi
  # kanban-store gets only the two tokens it declares, copied from the server's
  # file on every install so the two never differ.
  ( umask 077; grep -E '^(KANBAN_STORE_SERVICE_TOKEN|GRANT_STORE_SERVICE_TOKEN)=' "$ROOT/env/secrets.env" > "$ROOT/env/kanban-store-secrets.env" )

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
LLMBRIDGE_KANBAN_STORE_URL=http://127.0.0.1:$KANBAN_STORE_PORT
LLMBRIDGE_REFERENCE_STORE_URL=http://127.0.0.1:$REFERENCE_STORE_PORT
LLMBRIDGE_PERMISSION_STORE_URL=http://127.0.0.1:$PERMISSION_STORE_PORT
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
KANBAN_STORE_URL=http://127.0.0.1:$KANBAN_STORE_PORT
EOF
  cat > "$ROOT/env/kanban-store.env" <<EOF
KANBAN_LISTEN_HOST=127.0.0.1
KANBAN_PORT=$KANBAN_STORE_PORT
KANBAN_DB=$ROOT/data/kanban-store.db
KANBAN_NOTEBOARD_URL=$nowhere
PRINCIPAL_STORE_URL=http://127.0.0.1:$PRINCIPAL_STORE_PORT
REFERENCE_STORE_URL=http://127.0.0.1:$REFERENCE_STORE_PORT
LLM_BRIDGE_URL=http://127.0.0.1:$BRIDGE_PORT
BUNDLE_STORE_URL=$nowhere
AUTH_STORE_URL=$nowhere
GRANT_STORE_URL=http://127.0.0.1:$GRANT_STORE_PORT
EOF
  cat > "$ROOT/env/reference-store.env" <<EOF
REFERENCE_STORE_ADDR=127.0.0.1:$REFERENCE_STORE_PORT
REFERENCE_STORE_SERVICE_URLS=llm-bridge-server:http://127.0.0.1:$BRIDGE_PORT,tool-store:http://127.0.0.1:$TOOL_STORE_PORT,principal-store:http://127.0.0.1:$PRINCIPAL_STORE_PORT
REFERENCE_STORE_PRINCIPAL_STORE_URL=http://127.0.0.1:$PRINCIPAL_STORE_PORT
REFERENCE_STORE_GRANT_STORE_URL=http://127.0.0.1:$GRANT_STORE_PORT
EOF
  # permission-store reads no environment; its port and database are flags in
  # its unit.
  : > "$ROOT/env/permission-store.env"
  rm -f "$ROOT/env/stores.env"

  step "stopping what is running, to replace the binaries"
  systemctl --user stop "$UNIT_PREFIX-runner.service" "$UNIT_PREFIX.service" 2>/dev/null || true
  for store in "${STORES[@]}"; do systemctl --user stop "$UNIT_PREFIX-$store.service" 2>/dev/null || true; done

  step "installing binaries"
  for binary in llm-bridge-server llm-bridge-runner llm-bridge-mock "${STORES[@]}"; do
    install -m 755 "$binaries/$binary" "$ROOT/bin/$binary"
  done
  # The real harness wrappers. They run only once use-machine-logins has put
  # the agent programs and their logins in place.
  for wrapper in llm-bridge-claudecode llm-bridge-codex; do
    if [ -x "$binaries/$wrapper" ]; then install -m 755 "$binaries/$wrapper" "$ROOT/bin/$wrapper"; fi
  done
  # The server offers runners the build in this folder. It is the same file the
  # test runner runs, so the two never disagree about which is newer.
  install -m 755 "$binaries/llm-bridge-runner" "$ROOT/runner-assets/llm-bridge-runner-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"

  step "writing units"
  for store in "${STORES[@]}"; do
    # Each store gets only its own files: grant-store presents the server's
    # store tokens, kanban-store holds its two, reference-store its own.
    case "$store" in
      grant-store)
        write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" "$ROOT/env/secrets.env" </dev/null ;;
      kanban-store)
        write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" "$ROOT/env/kanban-store-secrets.env" </dev/null ;;
      reference-store)
        write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" "$ROOT/env/reference-store-secrets.env" </dev/null ;;
      permission-store)
        write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store --port $PERMISSION_STORE_PORT --db $ROOT/data/permission-store.db" "$ROOT/env/$store.env" </dev/null ;;
      *)
        write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" </dev/null ;;
    esac
  done
  write_unit "$UNIT_PREFIX" "llm-bridge test instance: the server" llm-bridge-server "$ROOT/env/server.env" "$ROOT/env/secrets.env" <<EOF
EOF
  # The runner's own environment reaches every harness it starts, so this is
  # where a test sets a harness's variables (the mock harness reads
  # MOCK_HARNESS_DELAY_MS, say). Written once and then left to whoever edits it.
  if [ ! -f "$ROOT/env/runner.env" ]; then
    echo "# Variables for the test runner and the harnesses it starts. Restart $UNIT_PREFIX-runner after a change." > "$ROOT/env/runner.env"
  fi
  write_unit "$UNIT_PREFIX-runner" "llm-bridge test instance: its runner" "llm-bridge-runner run" "$ROOT/env/runner.env" <<EOF
Environment=LLMBRIDGE_RUNNER_CONFIG=$ROOT/runner/config.json
EOF
  systemctl --user daemon-reload

  step "starting the stores"
  for store in "${STORES[@]}"; do systemctl --user enable --now "$UNIT_PREFIX-$store.service" >/dev/null 2>&1; done
  wait_for principal-store "http://127.0.0.1:$PRINCIPAL_STORE_PORT/kinds"
  wait_for tool-store "http://127.0.0.1:$TOOL_STORE_PORT/tools"
  wait_for log-store "http://127.0.0.1:$LOG_STORE_PORT/health"
  wait_for kanban-store "http://127.0.0.1:$KANBAN_STORE_PORT/health"
  wait_for reference-store "http://127.0.0.1:$REFERENCE_STORE_PORT/types"
  wait_for permission-store "http://127.0.0.1:$PERMISSION_STORE_PORT/healthz"

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
  local real_home file
  real_home="$(getent passwd "$(id -un)" | cut -d: -f6)"
  for file in "${LOGIN_FILES[@]}"; do
    [ -e "$ROOT/home/$file" ] || continue
    if [ "$(stat -c %i "$ROOT/home/$file")" = "$(stat -c %i "$real_home/$file" 2>/dev/null)" ]; then
      echo "  login:      $file shared with $real_home"
    else
      echo "  login:      WARNING: $file is no longer the same file as $real_home/$file; run use-machine-logins again"
    fi
  done
  if curl -fsS -o /dev/null "$BASE/health" 2>/dev/null; then
    echo "  machines:   $(bridge_curl "$BASE/machines" | jq -r '[.[] | "\(.name) (\(.transport))"] | join(", ")')"
    echo "  instances:  $(bridge_curl "$BASE/instances" | jq -r '[.[] | "\(.id) \(.harness_type // .harness)"] | join(", ")')"
    echo "  sign in as: $(curl -fsS "http://127.0.0.1:$PRINCIPAL_STORE_PORT/principals" | jq -r '[.[] | select(.kind == "human") | "\(.id) \(.display_name)"] | join(", ")')"
  else
    echo "  the server is not answering"
  fi
}

# The login files the agent programs keep, relative to a home folder.
LOGIN_FILES=(.claude/.credentials.json .codex/auth.json)

# use-machine-logins lets the test runner start real Claude Code and codex
# sessions on the logins this machine's own user already has.
#
# The programs are linked into the test folder's bin. Each login file is a
# HARD link, not a symbolic one: Claude Code opens its credentials with
# O_NOFOLLOW and treats a symbolic link as no login at all (read in the 2.1.330
# binary). It rewrites the file in place (O_TRUNC), so with a hard link both
# installs read one file, and a token refreshed by either is the token both
# use. Everything else -- history, projects, settings -- stays in the test
# home, so the machine's own runner never discovers a test session. status
# reports a login file whose link has come apart.
use_machine_logins() {
  load_settings
  local real_home program path wrapper file
  real_home="$(getent passwd "$(id -un)" | cut -d: -f6)"
  [ "$(stat -c %d "$real_home")" = "$(stat -c %d "$ROOT")" ] || fail "$ROOT and $real_home are on different file systems, so a login file cannot be hard linked"
  for program in claude codex node; do
    path="$(PATH="$real_home/.local/bin:$PATH" command -v "$program")" || fail "this machine has no $program"
    ln -sfn "$path" "$ROOT/bin/$program"
  done
  for wrapper in llm-bridge-claudecode llm-bridge-codex; do
    [ -x "$ROOT/bin/$wrapper" ] || fail "$ROOT/bin/$wrapper is missing; install a folder built by build-test-instance.sh"
  done
  for file in "${LOGIN_FILES[@]}"; do
    [ -f "$real_home/$file" ] || fail "this machine has no login at $real_home/$file"
    mkdir -p "$(dirname "$ROOT/home/$file")"
    chmod 700 "$(dirname "$ROOT/home/$file")"
    ln -f "$real_home/$file" "$ROOT/home/$file"
  done
  # The agent programs must not update themselves from inside the test home.
  grep -q '^DISABLE_AUTOUPDATER=' "$ROOT/env/runner.env" || echo 'DISABLE_AUTOUPDATER=1' >> "$ROOT/env/runner.env"

  # The sessions need models the agent programs know. The registry is the
  # server's own database, which no route writes.
  step "adding a Claude model and a codex model to the registry"
  sqlite3 "$ROOT/data/models.db" \
    "INSERT OR IGNORE INTO providers (id, name) VALUES ('anthropic', 'Anthropic'), ('openai', 'OpenAI');
     INSERT OR IGNORE INTO models (id, provider, name) VALUES ('claude-haiku-5-5', 'anthropic', 'Claude Haiku 5.5'), ('gpt-5.6-luna', 'openai', 'GPT-5.6 Luna');"

  # A chat started from the front end takes the harness's default model; the
  # registry's own default is the mock harness's stand-in, which Claude Code
  # answers with a 404. Set one per harness, keeping any already chosen.
  step "making them the default models of their harnesses"
  local prefs
  prefs="$(bridge_curl -f "$BASE/bridge-prefs")" || fail "could not read $BASE/bridge-prefs"
  prefs="$(jq '.defaults.claude_code.model //= "claude-haiku-5-5" | .defaults.codex.model //= "gpt-5.6-luna"' <<<"$prefs")"
  bridge_curl -f -o /dev/null -X PUT "$BASE/bridge-prefs" -H 'Content-Type: application/json' -d "$prefs" || fail "could not write $BASE/bridge-prefs"

  step "restarting the runner, which offers every harness it finds on its PATH"
  systemctl --user restart "$UNIT_PREFIX-runner.service"
  sleep 3
  status_instance
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
  use-machine-logins) use_machine_logins ;;
  *)             echo "usage: $0 install <folder of binaries> | status | service-token | uninstall | use-machine-logins" >&2; exit 2 ;;
esac
