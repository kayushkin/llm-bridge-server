#!/usr/bin/env bash
# Keep an llm-bridge-runner in WSL connected.
#
# WSL stops its Linux VM a few seconds after the last terminal closes, and
# the runner's systemd user units stop with it, so a runner in WSL drops off
# the bridge whenever no terminal is open. This script, run inside the WSL
# distro that holds the runner:
#
#   1. checks that systemd runs in this distro, and turns it on in
#      /etc/wsl.conf if not (which needs `wsl --shutdown` once, then rerun);
#   2. lets this user's units run without a login (loginctl enable-linger);
#   3. puts a shortcut in the Windows Startup folder that starts this distro,
#      hidden, and holds it open with `sleep infinity`, then opens it now. A
#      Startup shortcut needs no administrator rights; a logon scheduled task
#      does ("Access is denied" on SSDAWN, 2026-10-14);
#   4. reruns install.sh, which updates the runner in place from the saved
#      config — no passphrase — or enrolls with --passphrase on a first install;
#   5. prints the state of both units.
#
#   curl -fsSL https://raw.githubusercontent.com/kayushkin/llm-bridge-runner/main/scripts/wsl-setup.sh | bash
#   ... | bash -s -- --endpoint user@host --passphrase XXXX     # first install
#
# Arguments are passed through to install.sh.

set -euo pipefail

install_script_url="https://raw.githubusercontent.com/kayushkin/llm-bridge-runner/main/scripts/install.sh"
keepalive_shortcut_name="WSL keepalive for llm-bridge-runner.lnk"

if ! grep -qi microsoft /proc/version; then
  echo "this is not WSL; use install.sh directly" >&2
  exit 1
fi
if [[ -z "${WSL_DISTRO_NAME:-}" ]]; then
  echo "WSL_DISTRO_NAME is unset; run this from a WSL shell, not through sudo" >&2
  exit 1
fi
for windows_tool in powershell.exe wsl.exe; do
  if ! command -v "$windows_tool" >/dev/null 2>&1; then
    echo "$windows_tool is not reachable from WSL; Windows interop is off ([interop] enabled=false in /etc/wsl.conf?)" >&2
    exit 1
  fi
done

echo "== 1. systemd"
if [[ "$(ps -p 1 -o comm=)" != "systemd" ]]; then
  if grep -qs '^\s*systemd\s*=\s*true' /etc/wsl.conf; then
    echo "/etc/wsl.conf turns systemd on, but PID 1 is $(ps -p 1 -o comm=)." >&2
    echo "Run 'wsl --shutdown' in Windows, open this distro again and rerun this script." >&2
    exit 1
  fi
  echo "systemd is off in $WSL_DISTRO_NAME; turning it on in /etc/wsl.conf (sudo)"
  printf '\n[boot]\nsystemd=true\n' | sudo tee -a /etc/wsl.conf >/dev/null
  echo "Now run 'wsl --shutdown' in Windows, open this distro again and rerun this script."
  exit 1
fi
echo "systemd is PID 1"

echo "== 2. linger"
if [[ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]]; then
  sudo loginctl enable-linger "$USER"
fi
echo "linger: $(loginctl show-user "$USER" -p Linger --value)"

echo "== 3. Startup shortcut holding $WSL_DISTRO_NAME open"
# conhost --headless runs wsl.exe with no window; sleep infinity keeps the VM up.
# The distro name goes to PowerShell through the environment, never into its code.
# stdin is /dev/null: under `curl | bash` the rest of this script is on stdin,
# and a Windows program would read it.
KEEPALIVE_DISTRO="$WSL_DISTRO_NAME" KEEPALIVE_SHORTCUT_NAME="$keepalive_shortcut_name" WSLENV=KEEPALIVE_DISTRO:KEEPALIVE_SHORTCUT_NAME \
  powershell.exe -NoProfile -NonInteractive -Command '
    $ErrorActionPreference = "Stop"
    $path = Join-Path ([Environment]::GetFolderPath("Startup")) $env:KEEPALIVE_SHORTCUT_NAME
    $shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut($path)
    $shortcut.TargetPath = Join-Path $env:SystemRoot "System32\conhost.exe"
    $shortcut.Arguments = "--headless wsl.exe -d $env:KEEPALIVE_DISTRO --exec /bin/sleep infinity"
    $shortcut.WindowStyle = 7
    $shortcut.Save()
    Write-Output "shortcut: $path"
    Start-Process -FilePath $path
    Write-Output "started"
  ' </dev/null | tr -d '\r'

echo "== 4. runner install or update"
curl -fsSL "$install_script_url" | bash -s -- "$@"

echo "== 5. state"
for unit in llm-bridge-tunnel.service llm-bridge-runner.service; do
  if systemctl --user cat "$unit" >/dev/null 2>&1; then
    printf '%-28s %s\n' "$unit" "$(systemctl --user is-active "$unit" || true)"
  fi
done
echo "Recent runner log:"
journalctl --user -u llm-bridge-runner -n 8 --no-pager || true
