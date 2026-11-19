# llm-bridge-runner

Long-lived daemon that registers a remote machine with an [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) and accepts harness-spawn requests over a single outbound WebSocket. Lets the server drive harness subprocesses on machines behind NAT without inbound SSH or a VPN.

## How it works

```
  Your llm-bridge-server                Remote machine
  ┌────────────────────────┐             ┌────────────────────────────┐
  │                        │  outbound   │                            │
  │  spawn harness X here  │◀────WS─────▶│  llm-bridge-runner daemon  │
  │                        │             │            │               │
  └────────────────────────┘             │            ▼               │
                                         │   harness subprocess(es)   │
                                         │   (claudecode, codex, …)   │
                                         └────────────────────────────┘
```

The runner connects out to the server and stays connected. The server sends spawn requests over that connection; the runner forks the harness binary locally and pipes its stdin/stdout/stderr back. Closes the loop without exposing any inbound port on the remote.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/kayushkin/llm-bridge-runner/main/scripts/install.sh | bash
```

The installer prompts for two things:

1. **bridge endpoint** — either a public URL (`https://bridge.example.com`) or an SSH target (`user@host[:remote-port]`). For SSH targets, the installer also installs a persistent `ssh -NL` tunnel as a systemd user service so the runner can reach `localhost:8160` without a public DNS name.
2. **enrollment passphrase** — single-use, minted on the server with `llm-bridge-server mint-enroll`. Trades for a durable per-machine token stored at `~/.config/llm-bridge-runner/config.json`.

The installer downloads a prebuilt binary (linux/{amd64,arm64}, darwin/{amd64,arm64}) from the bridge server's `/api/runner/binary` endpoint, enrolls the machine, and starts the runner as a service: a systemd user unit (`llm-bridge-runner.service`) on Linux, a launchd agent (`com.kayushkin.llm-bridge-runner`, logs in `~/Library/Logs/`) on macOS. An SSH endpoint gets a second unit or agent for the tunnel.

### In WSL

WSL stops its Linux VM soon after the last terminal closes, taking the runner with it. Run this inside the distro instead of `install.sh`:

```bash
curl -fsSL https://raw.githubusercontent.com/kayushkin/llm-bridge-runner/main/scripts/wsl-setup.sh | bash
```

It makes sure systemd and linger are on, puts a shortcut in the Windows Startup folder that holds the distro open, hidden (no administrator rights needed), and then runs `install.sh` (updating in place from the saved config, or enrolling when given `--endpoint` and `--passphrase`).

## Updates

The runner keeps itself current. Before each connection it asks the bridge for its own build and, when the bridge has a newer one, replaces `~/.local/bin/llm-bridge-runner` and exits; systemd (`Restart=always`) or launchd (`KeepAlive`) starts the new one. It does the same for each harness wrapper it installed, before every session. A runner run from anywhere else is never replaced.

## Computer use

On WSL the runner installs `llm-bridge-computer-use.exe`, the Windows build, under `%LOCALAPPDATA%\llm-bridge`, keeps it current, and reports it to the bridge. The bridge hands it to a Claude Code session that asks (`harness_config.computer_use: true`) only when the machine's owner has switched computer use on (`PUT /machines/{id}/computer-use {"allowed": true}`). Claude Code in WSL starts it through interop, and it drives the desktop of the Windows session WSL runs in; it cannot act on a locked screen or on administrator prompts.

## Mint a passphrase (server side)

On the host running llm-bridge-server:

```bash
llm-bridge-server mint-enroll --ttl 30m
```

That prints an 8-character passphrase. Enter it when the installer asks. The passphrase is single-use; once redeemed, future reconnects use the per-machine token instead.

## Subcommands

```
llm-bridge-runner enroll --server URL --passphrase XXXX [--name LABEL]
    One-time exchange: trade a passphrase for a per-machine token.
    Writes ~/.config/llm-bridge-runner/config.json.

llm-bridge-runner [run]
    Long-lived service. Reads the saved config and dials the server.
    Default subcommand.

llm-bridge-runner -version
    Print version.
```

## Service management

```bash
journalctl --user -u llm-bridge-runner -f      # logs
systemctl --user restart llm-bridge-runner     # restart
sudo loginctl enable-linger $USER              # survive logout
```

When the bridge endpoint is an SSH target, a second unit `llm-bridge-tunnel.service` keeps the SSH tunnel alive; the runner has `Requires=llm-bridge-tunnel.service`.

## Computer use

`cmd/llm-bridge-computer-use` is an MCP server, over stdio, with one tool, `computer`: screenshots of the main display, scaled to fit 1280×800, and mouse and keyboard actions in the shape of Anthropic's computer tool. `deploy.sh` publishes it beside the runner as `llm-bridge-computer-use-<os>-<arch>` for windows/amd64 and darwin/{arm64,amd64}. Fetch it with `/api/runner/binary?name=computer-use&os=…&arch=…`.

- **Name the MCP server anything but `computer-use`.** Claude Code reserves that name and skips such a server with only a debug-log warning. `bridge-computer-use` works.
- **macOS** uses `screencapture` and `osascript`, with no cgo. launchd starts `~/.local/bin/llm-bridge-runner-launcher`, which runs the runner as its child and restarts it after it updates itself. macOS gives privacy grants to the program launchd started and identifies an unsigned program by its file's hash, so the grants go to the launcher, which the installer never replaces once present: Accessibility (without it: "osascript is not allowed to send keystrokes (1002)"), Screen Recording, and the "bypass the system private window picker" dialog on the first capture. Proven on MacBook, 2026-10-14 (br_1792019214241724555), with the grants on the runner itself, before the launcher.
- **Windows** uses SendInput and BitBlt. Claude Code in WSL runs the `.exe` through interop, in the Windows session WSL was started from. Not yet tested on a real Windows desktop.

## Part of the llm-bridge ecosystem

See [llm-bridge](https://github.com/kayushkin/llm-bridge) for the canonical message types and the full architecture.

## License

Apache 2.0. See [LICENSE](./LICENSE).
