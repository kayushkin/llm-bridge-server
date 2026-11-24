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
# llm-bridge-mock and log-store, built for this machine. The
# machine itself needs no Go or Node, only systemd user
# units, curl, jq, sqlite3 and the Postgres 18 server programs (initdb,
# postgres, pg_isready, psql) in $BRIDGE_TEST_POSTGRES_BIN, by default where the
# Postgres project's packages put them. Install those with no cluster of the
# system's own (create_main_cluster = false). Running install again replaces the
# binaries and restarts; settings, secrets and data are kept.
#
# What it runs:
#   the server, with its front end at /ui/ and demo login switched on. It holds
#   principal-store, grant-store, sign-in-store, permission-store, tool-store,
#   skill-store, bundle-store, noteboard, kanban-store, project-store,
#   prediction-store, repo-store, work-graph-store and file-store in its own
#   process, in Postgres modules, each under /<store>/ (kanban-store under
#   /kanban/ and /kanban-store/), with file-store's bytes under
#   data/file-store; and reference-store, which keeps no records, at
#   /reference-store/
#   log-store, a unit of its own
#   a Postgres of its own (llm-bridge-test-postgres), data under data/postgres,
#   on 127.0.0.1 and a socket inside the folder, holding database bridge for
#   the server's login bridge_server, which may create roles
#   a runner enrolled with this server, offering the mock harness
#
# Every address a store would otherwise default to is set here, because the
# defaults are a real install's ports, and on a machine with a real runner
# 127.0.0.1:8160 is a tunnel to the real server. A service the test instance
# does not run (auth-store, PinchTab) is set to a port nothing listens on, and
# what needs it answers an error.
#
# An instance installed before principal-store, grant-store, tool-store,
# permission-store, kanban-store and reference-store moved into the server ran
# each as a unit of its own. The first install of a server that holds them
# copies each one's SQLite file into the server's module, once (import-<store>,
# keeping every id), then removes the unit and leaves the file where it was,
# unused. reference-store kept no records, so only its unit goes.
#
# Demo login lets anyone who can reach the server sign in as any person, with
# no password. That is acceptable here only because the server listens on
# loopback. Never put this instance behind a public address.
set -euo pipefail

ROOT="${BRIDGE_TEST_INSTANCE_DIR:-$HOME/.local/opt/llm-bridge-test}"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_PREFIX="llm-bridge-test"
STORES=(log-store)
# The stores the server now holds, which an older instance ran as units of its
# own. Only install's import and the unit clean-up name them.
RETIRED_STORES=(principal-store grant-store tool-store permission-store kanban-store reference-store)
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
POSTGRES_BIN="${BRIDGE_TEST_POSTGRES_BIN:-/usr/lib/postgresql/18/bin}"
# The login the server provisions its Postgres modules as, and its database.
POSTGRES_SERVER_LOGIN="bridge_server"
POSTGRES_DATABASE="bridge"

fail() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '==> %s\n' "$*"; }

load_settings() {
  [ -f "$ROOT/env/ports.env" ] || fail "no test instance at $ROOT; run: $0 install <folder of binaries>"
  # shellcheck disable=SC1091
  . "$ROOT/env/ports.env"
  BASE="http://127.0.0.1:$BRIDGE_PORT"
  POSTGRES_SOCKET_DIR="$ROOT/run/postgres"
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

# server_command runs the server binary for a one-off command (mint-enroll,
# import-<store>) with the unit's environment. The files are read line by
# line, not sourced: the Postgres URL holds a "&", which a shell would take for
# the end of a command.
server_command() { # <argument>...
  (
    local file line
    for file in "$ROOT/env/server.env" "$ROOT/env/secrets.env"; do
      while IFS= read -r line; do
        case "$line" in ''|'#'*) continue ;; esac
        export "${line?}"
      done < "$file"
    done
    HOME="$ROOT/home" exec "$ROOT/bin/llm-bridge-server" "$@"
  )
}

random_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# write_unit writes a user unit. <binary> is a program in the instance's bin,
# or an absolute path. Lines for [Unit] come from UNIT_SECTION_LINES, lines
# for [Service] on stdin.
write_unit() { # <unit name> <description> <binary> <environment file>... ; extra unit lines on stdin
  local name="$1" description="$2" binary="$3"; shift 3
  case "$binary" in /*) ;; *) binary="$ROOT/bin/$binary" ;; esac
  {
    echo "[Unit]"
    echo "Description=$description"
    if [ -n "${UNIT_SECTION_LINES:-}" ]; then printf '%s\n' "$UNIT_SECTION_LINES"; fi
    echo
    echo "[Service]"
    echo "ExecStart=$binary"
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

# prepare_postgres makes the instance's Postgres cluster the first time, and
# writes its settings and who may sign in every time. Only this machine's user
# signs in, over the socket inside the folder, as itself (the cluster's
# superuser) or as the server's login. Nobody has a password, so the TCP
# listener on 127.0.0.1 lets nobody in.
prepare_postgres() {
  local data="$ROOT/data/postgres" user
  user="$(id -un)"
  if [ ! -f "$data/PG_VERSION" ]; then
    step "making the instance's Postgres cluster in $data"
    "$POSTGRES_BIN/initdb" -D "$data" --encoding=UTF8 --locale=C.UTF-8 --auth-local=peer --auth-host=scram-sha-256 >/dev/null
    echo "include_if_exists = 'test-instance.conf'" >> "$data/postgresql.conf"
  fi
  cat > "$data/test-instance.conf" <<EOF
# Written by test-instance.sh on every install.
listen_addresses = '127.0.0.1'
port = $POSTGRES_PORT
unix_socket_directories = '$POSTGRES_SOCKET_DIR'
EOF
  cat > "$data/pg_hba.conf" <<EOF
# Written by test-instance.sh on every install.
local all all                peer map=test_instance
host  all all 127.0.0.1/32   scram-sha-256
EOF
  cat > "$data/pg_ident.conf" <<EOF
# Written by test-instance.sh on every install.
test_instance $user $user
test_instance $user $POSTGRES_SERVER_LOGIN
EOF
}

# make_postgres_login_and_database makes the server's login and its database
# when they are missing. The login may create roles, which the server needs to
# provision each module's role and schema, and is not a superuser.
make_postgres_login_and_database() {
  "$POSTGRES_BIN/psql" -q -X -v ON_ERROR_STOP=1 -h "$POSTGRES_SOCKET_DIR" -p "$POSTGRES_PORT" -d postgres <<EOF
SELECT 'CREATE ROLE $POSTGRES_SERVER_LOGIN LOGIN CREATEROLE'
  WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$POSTGRES_SERVER_LOGIN') \\gexec
SELECT 'CREATE DATABASE $POSTGRES_DATABASE OWNER $POSTGRES_SERVER_LOGIN'
  WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = '$POSTGRES_DATABASE') \\gexec
EOF
}

# retired_store_database is where an older instance's standalone store kept its
# SQLite file: permission-store and kanban-store were given one in their units,
# the others used their own default under the test home. reference-store kept
# none, since it only looked records up in the other stores, so it prints
# nothing and has nothing to import.
retired_store_database() { # <store>
  case "$1" in
    permission-store) echo "$ROOT/data/permission-store.db" ;;
    kanban-store)     echo "$ROOT/data/kanban-store.db" ;;
    reference-store)  ;;
    *)                echo "$ROOT/home/.config/$1/$1.db" ;;
  esac
}

# import_retired_stores copies each retired store's SQLite file into the
# server's module of that store, once: data/imported/<store> records that it
# was done and what was copied. The import refuses a module that already holds
# records, so it runs before the server's first start on a build that holds the
# store, and a failed import stops the install with the store's file and unit
# left as they were. The store is stopped by then; the file read is a .backup
# of it, so its write-ahead log is included.
import_retired_stores() {
  local store database copy
  mkdir -p "$ROOT/data/imported"
  for store in "${RETIRED_STORES[@]}"; do
    database="$(retired_store_database "$store")"
    [ -f "$ROOT/data/imported/$store" ] && continue
    [ -n "$database" ] && [ -f "$database" ] || continue
    step "importing $store's data into the server ($database)"
    copy="$ROOT/data/imported/$store.copy.db"
    rm -f "$copy" "$copy-shm" "$copy-wal"
    sqlite3 "$database" ".backup '$copy'"
    server_command "import-$store" -sqlite "$copy" > "$ROOT/data/imported/$store.out" 2>&1 ||
      fail "import-$store failed; nothing of it was imported, and $UNIT_PREFIX-$store is left in place: see $ROOT/data/imported/$store.out"
    rm -f "$copy" "$copy-shm" "$copy-wal"
    mv "$ROOT/data/imported/$store.out" "$ROOT/data/imported/$store"
    echo "    $(cat "$ROOT/data/imported/$store")"
  done
}

# remove_retired_store_units takes away the units and programs of the stores
# the server now holds, once their data is in it. Their data files stay where
# they were.
remove_retired_store_units() {
  local store
  for store in "${RETIRED_STORES[@]}"; do
    rm -f "$ROOT/bin/$store"
    [ -f "$UNIT_DIR/$UNIT_PREFIX-$store.service" ] || continue
    step "removing $UNIT_PREFIX-$store: the server holds $store now"
    systemctl --user disable --now "$UNIT_PREFIX-$store.service" >/dev/null 2>&1 || true
    rm -f "$UNIT_DIR/$UNIT_PREFIX-$store.service"
  done
  systemctl --user daemon-reload
}

install_instance() {
  local binaries="${1:-}"
  [ -n "$binaries" ] && [ -d "$binaries" ] || fail "usage: $0 install <folder of binaries>"
  for tool in systemctl curl jq sqlite3; do
    command -v "$tool" >/dev/null 2>&1 || fail "this machine has no $tool"
  done
  for program in initdb postgres pg_isready psql; do
    [ -x "$POSTGRES_BIN/$program" ] || fail "$POSTGRES_BIN/$program is missing: install the Postgres 18 server packages, with no system cluster, or set BRIDGE_TEST_POSTGRES_BIN"
  done
  for binary in llm-bridge-server llm-bridge-runner llm-bridge-mock "${STORES[@]}"; do
    [ -x "$binaries/$binary" ] || fail "$binaries/$binary is missing or not executable"
  done

  step "laying out $ROOT"
  mkdir -p "$ROOT"/{bin,data,env,home,runner,runner-assets,run/postgres} "$ROOT"/data/{file-store,work-graph-store} "$UNIT_DIR"
  chmod 700 "$ROOT" "$ROOT/env" "$ROOT/run/postgres"

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
  # A real Postgres is on 5432, below the server's port, so there is no offset
  # to keep; 18432 says which it is.
  add_port POSTGRES_PORT $((base + 272))
  # The retired stores' ports go: nothing listens on them any more.
  sed -i -E '/^(TOOL_STORE|PRINCIPAL_STORE|GRANT_STORE|PERMISSION_STORE|KANBAN_STORE|REFERENCE_STORE)_PORT=/d' "$ROOT/env/ports.env"
  load_settings
  local nowhere="http://127.0.0.1:1"

  if [ ! -f "$ROOT/env/secrets.env" ]; then
    step "making this instance's own secrets"
    ( umask 077; cat > "$ROOT/env/secrets.env" <<EOF
LLMBRIDGE_SERVICE_TOKEN=$(random_hex 32)
LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY=$(random_hex 32)
EOF
    )
  fi
  # grant-store's and kanban-store's own tokens were for the standalone stores,
  # which the server called over HTTP. Nothing presents either any more, nor
  # reference-store's, which the server sent to the standalone reference-store
  # from server-store-tokens.env.
  sed -i -E '/^(GRANT|KANBAN)_STORE_SERVICE_TOKEN=/d' "$ROOT/env/secrets.env"

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
LLMBRIDGE_SKILL_STORE_DATA_DIRECTORY=$ROOT/data/skill-store
LLMBRIDGE_LOCAL_TOOLS_PINCHTAB_URL=$nowhere
AUTH_STORE_URL=$nowhere
LLMBRIDGE_FILE_STORE_DATA_DIRECTORY=$ROOT/data/file-store
LLMBRIDGE_WORK_GRAPH_STORE_SPOOL_PATH=$ROOT/data/work-graph-store/ref-updates.jsonl
LLMBRIDGE_POSTGRES_URL=postgres://$POSTGRES_SERVER_LOGIN@/$POSTGRES_DATABASE?host=$POSTGRES_SOCKET_DIR&port=$POSTGRES_PORT
EOF

  # One file per store: a store refuses to start when a variable carrying its
  # own prefix is set that it does not declare.
  cat > "$ROOT/env/log-store.env" <<EOF
LOG_STORE_LISTEN_ADDR=127.0.0.1:$LOG_STORE_PORT
LOG_STORE_DB_PATH=$ROOT/data/log-store.db
LOG_STORE_LOGSTACK_URL=$nowhere
EOF
  # The files of the stores the server now holds.
  rm -f "$ROOT/env/stores.env" "$ROOT/env/tool-store.env" "$ROOT/env/principal-store.env" "$ROOT/env/grant-store.env" "$ROOT/env/permission-store.env" \
    "$ROOT/env/kanban-store.env" "$ROOT/env/kanban-store-secrets.env" \
    "$ROOT/env/reference-store.env" "$ROOT/env/reference-store-secrets.env" "$ROOT/env/server-store-tokens.env"

  step "stopping what is running, to replace the binaries"
  systemctl --user stop "$UNIT_PREFIX-runner.service" "$UNIT_PREFIX.service" 2>/dev/null || true
  for store in "${STORES[@]}" "${RETIRED_STORES[@]}"; do systemctl --user stop "$UNIT_PREFIX-$store.service" 2>/dev/null || true; done
  systemctl --user stop "$UNIT_PREFIX-postgres.service" 2>/dev/null || true

  prepare_postgres

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
    # Each store gets only its own file.
    write_unit "$UNIT_PREFIX-$store" "llm-bridge test instance: $store" "$store" "$ROOT/env/$store.env" </dev/null
  done
  write_unit "$UNIT_PREFIX-postgres" "llm-bridge test instance: its Postgres" "$POSTGRES_BIN/postgres -D $ROOT/data/postgres" <<EOF
Type=notify
KillMode=mixed
KillSignal=SIGINT
TimeoutSec=120
EOF
  UNIT_SECTION_LINES="Wants=$UNIT_PREFIX-postgres.service
After=$UNIT_PREFIX-postgres.service" \
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

  step "starting Postgres"
  systemctl --user enable --now "$UNIT_PREFIX-postgres.service" >/dev/null 2>&1
  for _ in $(seq 1 100); do
    "$POSTGRES_BIN/pg_isready" -q -h "$POSTGRES_SOCKET_DIR" -p "$POSTGRES_PORT" && break
    sleep 0.2
  done
  "$POSTGRES_BIN/pg_isready" -q -h "$POSTGRES_SOCKET_DIR" -p "$POSTGRES_PORT" ||
    fail "Postgres did not answer on $POSTGRES_SOCKET_DIR port $POSTGRES_PORT; see: journalctl --user -u $UNIT_PREFIX-postgres -n 50"
  make_postgres_login_and_database
  import_retired_stores
  remove_retired_store_units

  step "starting log-store"
  systemctl --user enable --now "$UNIT_PREFIX-log-store.service" >/dev/null 2>&1
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

  local principal_store="$BASE/principal-store"
  if [ "$(bridge_curl -f "$principal_store/principals" | jq '[.[] | select(.kind == "human")] | length')" = "0" ]; then
    step "making an administrator to sign in as"
    local principal_id
    principal_id="$(bridge_curl -f -X POST "$principal_store/principals" -H 'Content-Type: application/json' \
      -d '{"kind":"human","display_name":"Test operator"}' | jq -r '.id')"
    bridge_curl -f -o /dev/null -X PATCH "$principal_store/principals/$principal_id" -H 'Content-Type: application/json' -d '{"is_administrator":true}'
  fi

  if [ ! -f "$ROOT/runner/config.json" ]; then
    step "enrolling the runner"
    local passphrase
    passphrase="$(server_command mint-enroll | sed -n 's/^passphrase: *//p')"
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
  for unit in "$UNIT_PREFIX" "$UNIT_PREFIX-runner" "$UNIT_PREFIX-postgres" $(printf "$UNIT_PREFIX-%s " "${STORES[@]}"); do
    printf '  %-36s %s\n' "$unit" "$(systemctl --user is-active "$unit.service" 2>/dev/null || true)"
  done
  echo "  server:     $BASE   (front end: $BASE/ui/)"
  echo "  postgres:   127.0.0.1:$POSTGRES_PORT and $POSTGRES_SOCKET_DIR, database $POSTGRES_DATABASE as $POSTGRES_SERVER_LOGIN"
  local real_home file
  real_home="$(getent passwd "$(id -un)" | cut -d: -f6)"
  for file in "${LOGIN_FILES[@]}"; do
    [ -e "$ROOT/home/$file" ] || continue
    if [ "$(stat -L -c %d:%i "$ROOT/home/$file")" = "$(stat -L -c %d:%i "$real_home/$file" 2>/dev/null)" ]; then
      echo "  login:      $file shared with $real_home"
    else
      echo "  login:      WARNING: $file is no longer the same file as $real_home/$file; run use-machine-logins again"
    fi
  done
  if curl -fsS -o /dev/null "$BASE/health" 2>/dev/null; then
    echo "  machines:   $(bridge_curl "$BASE/machines" | jq -r '[.[] | "\(.name) (\(.transport))"] | join(", ")')"
    echo "  instances:  $(bridge_curl "$BASE/instances" | jq -r '[.[] | "\(.id) \(.harness_type // .harness)"] | join(", ")')"
    echo "  sign in as: $(bridge_curl -f "$BASE/principal-store/principals" | jq -r '[.[] | select(.kind == "human") | "\(.id) \(.display_name)"] | join(", ")')"
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
# HARD link (or, when the test folder is on a disk of its own, a bind mount
# made as root), never a symbolic one: Claude Code opens its credentials with
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
  for program in claude codex node; do
    path="$(PATH="$real_home/.local/bin:$PATH" command -v "$program")" || fail "this machine has no $program"
    ln -sfn "$path" "$ROOT/bin/$program"
  done
  for wrapper in llm-bridge-claudecode llm-bridge-codex; do
    [ -x "$ROOT/bin/$wrapper" ] || fail "$ROOT/bin/$wrapper is missing; install a folder built by build-test-instance.sh"
  done
  for file in "${LOGIN_FILES[@]}"; do
    [ -f "$real_home/$file" ] || fail "this machine has no login at $real_home/$file"
    # Already the same file: a hard link from an earlier run, or a bind mount
    # when the test folder lives on a disk of its own (a hard link cannot
    # cross disks, and a copy would split the login in two).
    if [ "$(stat -L -c %d:%i "$ROOT/home/$file" 2>/dev/null)" = "$(stat -L -c %d:%i "$real_home/$file")" ]; then
      continue
    fi
    [ "$(stat -L -c %d "$real_home")" = "$(stat -L -c %d "$ROOT/home")" ] ||
      fail "$ROOT/home is on another disk than $real_home, so $file cannot be hard linked; bind-mount $real_home/$file onto $ROOT/home/$file as root first"
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
  for unit in "$UNIT_PREFIX-runner" $(printf "$UNIT_PREFIX-%s " "${STORES[@]}" "${RETIRED_STORES[@]}") "$UNIT_PREFIX" "$UNIT_PREFIX-postgres"; do
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
