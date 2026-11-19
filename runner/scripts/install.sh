#!/usr/bin/env bash
#
# llm-bridge-runner install script.
#
# Single-command bootstrap. Prompts for a bridge endpoint and a one-time
# enrollment passphrase, downloads the runner binary, exchanges the
# passphrase for a durable per-machine token, and installs a systemd
# user unit that keeps the runner connected.
#
# Endpoint formats:
#   https://bridge.example.com             public TLS endpoint, no tunnel
#   http://bridge.internal:8160            direct HTTP, no tunnel
#   user@host                              SSH tunnel to host's localhost:8160
#   user@host:9999                         SSH tunnel to host's localhost:9999
#
# When the endpoint is an SSH target the script also installs a second
# systemd unit (llm-bridge-tunnel.service) that keeps an `ssh -NL` tunnel
# up; the runner service depends on it.
#
# Usage:
#   curl -fsSL <bridge-url>/api/runner/install.sh | bash
#   curl -fsSL <bridge-url>/api/runner/install.sh | bash -s -- --endpoint URL --passphrase XXXX

set -euo pipefail

endpoint="${LLMBRIDGE_ENDPOINT:-${LLMBRIDGE_SERVER:-}}"
passphrase="${LLMBRIDGE_PASSPHRASE:-}"
machine_name="${LLMBRIDGE_MACHINE_NAME:-}"
binary_url="${LLMBRIDGE_RUNNER_BINARY_URL:-}"
local_port="${LLMBRIDGE_LOCAL_PORT:-8160}"
non_interactive=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --endpoint|--server)  endpoint="$2"; shift 2 ;;
    --passphrase)         passphrase="$2"; shift 2 ;;
    --name)               machine_name="$2"; shift 2 ;;
    --binary-url)         binary_url="$2"; shift 2 ;;
    --local-port)         local_port="$2"; shift 2 ;;
    --yes|-y)             non_interactive=1; shift ;;
    -h|--help)
      sed -n '2,21p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

# ──────────────────────────────────────────────────────────────────────
# Environment detection
# ──────────────────────────────────────────────────────────────────────

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

is_wsl=0
if grep -qEi '(microsoft|wsl)' /proc/version 2>/dev/null; then
  is_wsl=1
fi

run_user="$(id -un)"
home_dir="${HOME:-$(getent passwd "$run_user" | cut -d: -f6)}"

# ──────────────────────────────────────────────────────────────────────
# Interactive prompts (read from /dev/tty so it works under curl|bash)
# ──────────────────────────────────────────────────────────────────────

prompt_value() {
  local label="$1" current="$2" secret="${3:-}" value
  if [[ -n "$current" ]]; then
    echo "$current"
    return
  fi
  if [[ ! -e /dev/tty ]]; then
    echo "missing required value: $label (no tty available for prompt)" >&2
    exit 2
  fi
  if [[ "$secret" == "secret" ]]; then
    printf "%s: " "$label" >/dev/tty
    IFS= read -rs value </dev/tty
    printf "\n" >/dev/tty
  else
    printf "%s: " "$label" >/dev/tty
    IFS= read -r value </dev/tty
  fi
  echo "$value"
}

# ──────────────────────────────────────────────────────────────────────
# Update mode: re-run on a host that's already enrolled
#
# When the saved config exists, treat this invocation as an update —
# refresh the runner binary and restart the service without re-prompting
# for a fresh passphrase. Re-enrollment requires --force-enroll.
# ──────────────────────────────────────────────────────────────────────

config_file="$home_dir/.config/llm-bridge-runner/config.json"
tunnel_unit="$home_dir/.config/systemd/user/llm-bridge-tunnel.service"
update_mode=0
if [[ -f "$config_file" && "${force_enroll:-0}" -ne 1 ]]; then
  read_field() {
    if ! command -v python3 >/dev/null 2>&1 && command -v plutil >/dev/null 2>&1; then
      plutil -extract "$1" raw -o - "$config_file" 2>/dev/null || true
      return
    fi
    python3 - "$config_file" "$1" <<'PY' 2>/dev/null || true
import json, sys
path, key = sys.argv[1], sys.argv[2]
try:
  print(json.load(open(path)).get(key, ''))
except Exception:
  pass
PY
  }
  saved_endpoint="$(read_field endpoint)"
  saved_machine_name="$(read_field machine_name)"
  saved_server_url="$(read_field server_url)"

  # Older configs didn't persist endpoint; fall back to the SSH target
  # in the systemd tunnel unit. Last word on the ExecStart line is the
  # `user@host[:port]` argument to ssh.
  if [[ -z "$saved_endpoint" && -f "$tunnel_unit" ]]; then
    saved_endpoint="$(awk -F= '/^ExecStart=/{print $0; exit}' "$tunnel_unit" | awk '{print $NF}')"
  fi
  # And if even that didn't help, but the saved server_url is non-tunneled
  # (no localhost), use the URL directly — it's a public-bridge install.
  if [[ -z "$saved_endpoint" && "$saved_server_url" != *localhost* && "$saved_server_url" != *127.0.0.1* ]]; then
    saved_endpoint="$saved_server_url"
  fi

  if [[ -n "$saved_endpoint" ]]; then
    update_mode=1
    if [[ -z "$endpoint" ]]; then endpoint="$saved_endpoint"; fi
    if [[ -z "$machine_name" && -n "$saved_machine_name" ]]; then machine_name="$saved_machine_name"; fi
    echo "found existing install for ${saved_machine_name:-this host} — updating in place"
  fi
fi

if [[ "$update_mode" -ne 1 ]]; then
  if [[ "$non_interactive" -eq 1 ]]; then
    for var in endpoint passphrase; do
      if [[ -z "${!var}" ]]; then
        echo "missing required value in non-interactive mode: --$var" >&2
        exit 2
      fi
    done
  else
    endpoint="$(prompt_value "bridge endpoint (URL or user@host)" "$endpoint")"
    passphrase="$(prompt_value "enrollment passphrase" "$passphrase" secret)"
    default_name="$(hostname -s 2>/dev/null || hostname)"
    if [[ "$is_wsl" -eq 1 ]]; then
      default_name="wsl-${default_name}"
    fi
    if [[ -z "$machine_name" ]]; then
      typed="$(prompt_value "machine label [$default_name]" "")"
      machine_name="${typed:-$default_name}"
    fi
  fi

  if [[ -z "$endpoint" || -z "$passphrase" ]]; then
    echo "endpoint and passphrase are required" >&2
    exit 2
  fi
fi

# ──────────────────────────────────────────────────────────────────────
# Endpoint parsing — URL vs SSH target
# ──────────────────────────────────────────────────────────────────────

needs_tunnel=0
ssh_target=""
ssh_remote_port=8160

if [[ "$endpoint" =~ ^https?:// ]] || [[ "$endpoint" =~ ^wss?:// ]]; then
  # Direct URL — no tunnel.
  bridge_url="$endpoint"
elif [[ "$endpoint" =~ ^([^@]+@)?([A-Za-z0-9.-]+)(:([0-9]+))?$ ]]; then
  # SSH target. Optional :port specifies the *bridge* port on the remote
  # host (default 8160). The local port is configurable via --local-port.
  ssh_target="${BASH_REMATCH[1]}${BASH_REMATCH[2]}"
  if [[ -n "${BASH_REMATCH[4]}" ]]; then
    ssh_remote_port="${BASH_REMATCH[4]}"
  fi
  needs_tunnel=1
  bridge_url="http://localhost:${local_port}"
else
  echo "unrecognized endpoint format: $endpoint" >&2
  echo "  expected https://host or http://host:port or user@host[:port]" >&2
  exit 2
fi

# ──────────────────────────────────────────────────────────────────────
# Pre-flight checks
# ──────────────────────────────────────────────────────────────────────

if ! command -v claude >/dev/null 2>&1; then
  echo "warning: 'claude' not found on PATH — Claude Code sessions will fail to spawn" >&2
  echo "         install with: npm install -g @anthropic-ai/claude-code" >&2
  echo "         (continuing — you can install it later)" >&2
fi

if [[ "$needs_tunnel" -eq 1 ]]; then
  if ! command -v ssh >/dev/null 2>&1; then
    echo "error: ssh client is required for tunneling endpoint $endpoint" >&2
    exit 1
  fi
  echo "ssh tunnel: localhost:${local_port} → ${ssh_target}:${ssh_remote_port}"
fi

# ──────────────────────────────────────────────────────────────────────
# Open install-time SSH tunnel (if needed)
# ──────────────────────────────────────────────────────────────────────

tunnel_pid=""
cleanup_install_tunnel() {
  if [[ -n "$tunnel_pid" ]]; then
    kill "$tunnel_pid" 2>/dev/null || true
  fi
}
trap cleanup_install_tunnel EXIT

# ss is Linux's; macOS has lsof.
local_port_is_listening() {
  if [[ "$os" == "darwin" ]]; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  else
    ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"
  fi
}

if [[ "$needs_tunnel" -eq 1 ]]; then
  # Whatever's bound to $local_port — reuse it if it actually proxies to
  # a healthy bridge (typical case: the persistent llm-bridge-tunnel
  # service is up from a previous install). Otherwise refuse loudly so
  # we don't silently talk to the wrong service.
  if local_port_is_listening "$local_port"; then
    if curl -fsS -o /dev/null --max-time 3 "${bridge_url}/health"; then
      echo "reusing existing tunnel on localhost:${local_port}"
      needs_install_tunnel=0
    else
      echo "error: localhost:${local_port} is in use but doesn't proxy to a healthy bridge; pass --local-port N to pick a free one" >&2
      exit 1
    fi
  else
    needs_install_tunnel=1
  fi
fi

if [[ "${needs_install_tunnel:-0}" -eq 1 ]]; then
  ssh -fN \
    -L "${local_port}:localhost:${ssh_remote_port}" \
    -o BatchMode=yes \
    -o ExitOnForwardFailure=yes \
    -o StrictHostKeyChecking=accept-new \
    -o ServerAliveInterval=30 \
    -o ServerAliveCountMax=3 \
    "$ssh_target"
  # `ssh -fN` daemonizes; find the process so we can clean it up at exit.
  tunnel_pid="$(pgrep -nu "$run_user" -f "ssh -fN -L ${local_port}:localhost:${ssh_remote_port} .* ${ssh_target}" || true)"
  # Give it a beat to bind.
  for _ in 1 2 3 4 5; do
    if curl -fsS -o /dev/null --max-time 2 "${bridge_url}/health"; then
      break
    fi
    sleep 1
  done
fi

# Sanity check: the bridge URL must respond.
if ! curl -fsS -o /dev/null --max-time 5 "${bridge_url}/health"; then
  echo "error: cannot reach bridge at ${bridge_url}" >&2
  exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Install the runner binary (download is always idempotent)
# ──────────────────────────────────────────────────────────────────────

bin_dir="$home_dir/.local/bin"
mkdir -p "$bin_dir"
runner_bin="$bin_dir/llm-bridge-runner"

if [[ -z "$binary_url" ]]; then
  binary_url="${bridge_url%/}/api/runner/binary?os=${os}&arch=${arch}"
fi

# Stop the running service before clobbering its binary. Otherwise the
# rename below races and ELF reload can leave a half-running daemon.
service_was_active=0
launch_agent_dir="$home_dir/Library/LaunchAgents"
tunnel_launch_label="com.kayushkin.llm-bridge-tunnel"
runner_launch_label="com.kayushkin.llm-bridge-runner"
launchd_domain="gui/$(id -u)"
if [[ "$update_mode" -eq 1 && "$os" == "darwin" ]]; then
  launchctl bootout "$launchd_domain/$runner_launch_label" 2>/dev/null || true
fi
if [[ "$update_mode" -eq 1 ]] && command -v systemctl >/dev/null 2>&1; then
  if systemctl --user is-active --quiet llm-bridge-runner.service; then
    systemctl --user stop llm-bridge-runner.service || true
    service_was_active=1
  fi
fi

echo "downloading llm-bridge-runner from ${binary_url}"
tmp="$(mktemp)"
if ! curl -fsSL "$binary_url" -o "$tmp"; then
  echo "error: download failed from $binary_url" >&2
  exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$runner_bin"
echo "installed $runner_bin"

# ──────────────────────────────────────────────────────────────────────
# Enroll: trade the passphrase for a durable runner token
# (skipped on update — existing token is reused)
# ──────────────────────────────────────────────────────────────────────

if [[ "$update_mode" -ne 1 ]]; then
  echo "enrolling with ${bridge_url} ..."
  "$runner_bin" enroll --server "$bridge_url" --passphrase "$passphrase" --name "$machine_name" --endpoint "$endpoint"
  unset passphrase
fi

# ──────────────────────────────────────────────────────────────────────
# Configure systemd user units
# ──────────────────────────────────────────────────────────────────────

# Capture the user's interactive PATH so harnesses spawned by the runner
# can find tools installed under nvm/npm-global/cargo/homebrew/etc. Without
# this, a service gets a minimal default PATH and `claude`, `node`, `npm`, …
# come back ENOENT even though `which claude` works in the shell. Same
# pattern llm-bridge.service uses on the host side. (`paste -` because
# macOS's paste needs the operand.)
runner_path="$(echo "$PATH" | tr ':' '\n' | awk '!seen[$0]++' | paste -sd: -)"

if [[ "$os" == "darwin" ]]; then
  # launchd agents in the user's GUI session: they start at login, restart
  # when they exit, and — unlike a daemon — can reach the screen, which
  # computer use will need.
  xml_escaped() {
    local value="$1"
    # Quoted replacements: in bash 5.2 a bare & there means the matched text.
    value="${value//&/"&amp;"}"
    value="${value//</"&lt;"}"
    value="${value//>/"&gt;"}"
    printf '%s' "$value"
  }
  write_launch_agent() {
    local label="$1" plist="$launch_agent_dir/$1.plist" argument
    shift
    {
      printf '<?xml version="1.0" encoding="UTF-8"?>\n'
      printf '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
      printf '<plist version="1.0">\n<dict>\n'
      printf '  <key>Label</key><string>%s</string>\n' "$(xml_escaped "$label")"
      printf '  <key>ProgramArguments</key>\n  <array>\n'
      for argument in "$@"; do
        printf '    <string>%s</string>\n' "$(xml_escaped "$argument")"
      done
      printf '  </array>\n'
      printf '  <key>EnvironmentVariables</key>\n  <dict>\n'
      printf '    <key>PATH</key><string>%s</string>\n' "$(xml_escaped "$runner_path")"
      printf '    <key>HOME</key><string>%s</string>\n' "$(xml_escaped "$home_dir")"
      printf '  </dict>\n'
      printf '  <key>RunAtLoad</key><true/>\n'
      printf '  <key>KeepAlive</key><true/>\n'
      printf '  <key>ThrottleInterval</key><integer>5</integer>\n'
      printf '  <key>StandardOutPath</key><string>%s</string>\n' "$(xml_escaped "$home_dir/Library/Logs/$label.log")"
      printf '  <key>StandardErrorPath</key><string>%s</string>\n' "$(xml_escaped "$home_dir/Library/Logs/$label.log")"
      printf '</dict>\n</plist>\n'
    } >"$plist"
    plutil -lint "$plist" >/dev/null
    launchctl bootout "$launchd_domain/$label" 2>/dev/null || true
    launchctl bootstrap "$launchd_domain" "$plist"
  }
  mkdir -p "$launch_agent_dir" "$home_dir/Library/Logs"
  # launchd starts a launcher that runs the runner, never the runner itself:
  # macOS gives Screen Recording and Accessibility to the program launchd
  # started and identifies an unsigned one by its file's hash, so grants
  # given to the runner were lost at every runner update. The launcher is
  # installed only when missing, because replacing it costs those grants.
  # It downloads here, before the tunnel agent below is restarted: through a
  # tunnel that is still coming back the download fails (MacBook, 2026-10-14).
  launcher_bin="$bin_dir/llm-bridge-runner-launcher"
  if [[ ! -x "$launcher_bin" ]]; then
    launcher_tmp="$(mktemp)"
    if ! curl -fsSL "${bridge_url%/}/api/runner/binary?name=runner-launcher&os=${os}&arch=${arch}" -o "$launcher_tmp"; then
      echo "error: could not download the runner launcher" >&2
      exit 1
    fi
    chmod +x "$launcher_tmp"
    mv "$launcher_tmp" "$launcher_bin"
    echo "installed $launcher_bin"
  fi
  # The install-time tunnel and the launchd tunnel can't both bind the port.
  cleanup_install_tunnel
  trap - EXIT
  if [[ "$needs_tunnel" -eq 1 ]]; then
    write_launch_agent "$tunnel_launch_label" /usr/bin/ssh -NL "${local_port}:localhost:${ssh_remote_port}" \
      -o BatchMode=yes -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=accept-new \
      -o ServerAliveInterval=30 -o ServerAliveCountMax=3 "$ssh_target"
  fi
  write_launch_agent "$runner_launch_label" "$launcher_bin" run
  echo
  echo "launchd agents loaded. Logs:"
  echo "  tail -f ~/Library/Logs/$runner_launch_label.log"
  if [[ "$needs_tunnel" -eq 1 ]]; then
    echo "  tail -f ~/Library/Logs/$tunnel_launch_label.log"
  fi
  echo "State: launchctl print $launchd_domain/$runner_launch_label | grep -E 'state|pid'"
  echo
  echo "For computer use, grant these once in System Settings → Privacy & Security,"
  echo "to $launcher_bin (press ⌘⇧G in the file picker to type the path):"
  echo "  Accessibility, and Screen Recording."
  exit 0
fi

if [[ "$os" != "linux" ]]; then
  echo "unsupported OS $os: run $runner_bin run under your own service manager" >&2
  exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
  echo "systemctl not found; service install skipped. Run manually: $runner_bin run" >&2
  exit 0
fi

unit_dir="$home_dir/.config/systemd/user"
mkdir -p "$unit_dir"

# Persistent tunnel unit (only when needed). Restart=always plus the SSH
# keepalive options keep the link healthy across network blips.
runner_after="network-online.target"
runner_requires=""
if [[ "$needs_tunnel" -eq 1 ]]; then
  cat >"$unit_dir/llm-bridge-tunnel.service" <<EOF
[Unit]
Description=SSH tunnel to llm-bridge-server (${ssh_target}:${ssh_remote_port})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/env ssh -NL ${local_port}:localhost:${ssh_remote_port} -o BatchMode=yes -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 -o ServerAliveCountMax=3 ${ssh_target}
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
  runner_after="${runner_after} llm-bridge-tunnel.service"
  runner_requires="Requires=llm-bridge-tunnel.service"
fi


# A unit's Environment= splits on spaces unless the assignment is quoted, and
# WSL's PATH carries Windows folders like /mnt/c/Program Files/…; unquoted,
# systemd cut PATH at the first space and dropped the rest. Inside the quotes
# a backslash and a double quote are escaped, and % (a unit specifier) doubled.
systemd_quoted_assignment() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//%/%%}"
  printf '"%s"' "$value"
}

cat >"$unit_dir/llm-bridge-runner.service" <<EOF
[Unit]
Description=llm-bridge-runner (${machine_name})
After=${runner_after}
Wants=network-online.target
${runner_requires}

[Service]
Type=simple
Environment=$(systemd_quoted_assignment "PATH=${runner_path}")
Environment=$(systemd_quoted_assignment "HOME=${home_dir}")
ExecStart=${runner_bin} run
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
if [[ "$needs_tunnel" -eq 1 ]]; then
  systemctl --user enable --now llm-bridge-tunnel.service
fi
# The install-time tunnel and the systemd-managed tunnel can't both bind
# the same local port — drop ours before starting the persistent one.
cleanup_install_tunnel
trap - EXIT
systemctl --user enable --now llm-bridge-runner.service

echo
echo "service started. Tail logs with:"
echo "  journalctl --user -u llm-bridge-runner -f"
if [[ "$needs_tunnel" -eq 1 ]]; then
  echo "  journalctl --user -u llm-bridge-tunnel -f"
fi
echo
echo "To survive logout (keep the runner alive after terminal closes):"
echo "  sudo loginctl enable-linger $run_user"
