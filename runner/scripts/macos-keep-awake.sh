#!/usr/bin/env bash
# Keep a Mac that serves computer use awake, unlocked and on screen.
#
# Computer use needs a lit, unlocked display: a Mac asleep cannot be reached,
# and a locked one shows only the login window. macOS locks the screen some
# minutes after the display turns off or the screen saver starts, so this keeps
# the display on and the screen saver off, and the lock never starts.
#
#   1. a launchd agent, com.kayushkin.keep-awake, runs `caffeinate -d -i -s`:
#      no display sleep, no idle sleep, and no system sleep while on mains
#      power; launchd restarts it if it exits and starts it at every login;
#   2. the screen saver's idle time is set to never, for this user.
#
# No administrator password is needed. What it cannot do: keep a closed lid
# awake without an external display, or log in after a reboot (turn on
# automatic login in System Settings → Users & Groups for that).
#
# Undo: launchctl bootout gui/$(id -u)/com.kayushkin.keep-awake;
#       rm ~/Library/LaunchAgents/com.kayushkin.keep-awake.plist;
#       defaults -currentHost delete com.apple.screensaver idleTime
set -euo pipefail

[[ "$(uname -s)" == "Darwin" ]] || { echo "this is for macOS" >&2; exit 1; }
label="com.kayushkin.keep-awake"
plist="$HOME/Library/LaunchAgents/$label.plist"
domain="gui/$(id -u)"

mkdir -p "$HOME/Library/LaunchAgents"
cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/caffeinate</string>
    <string>-d</string>
    <string>-i</string>
    <string>-s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
PLIST
plutil -lint "$plist" >/dev/null
launchctl bootout "$domain/$label" 2>/dev/null || true
launchctl bootstrap "$domain" "$plist"

defaults -currentHost write com.apple.screensaver idleTime -int 0

sleep 1
echo "== agent"
launchctl print "$domain/$label" | grep -E "state =|pid ="
echo "== assertions"
pmset -g assertions | grep -E "PreventUserIdleDisplaySleep|PreventUserIdleSystemSleep|PreventSystemSleep" | head -3
pmset -g assertions | grep caffeinate || true
echo "== screen saver idle time: $(defaults -currentHost read com.apple.screensaver idleTime)"
