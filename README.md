<p align="center">
  <img src="assets/hero.jpg" alt="tezzer" width="100%">
  <br>
  <em>Persistent, not fast — it never loses its way, and never hides the trail.</em>
</p>

# <img src="assets/logo.png" alt="" width="28" align="top"> tezzer

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![ci](https://github.com/kuriyama/tezzer/actions/workflows/ci.yml/badge.svg)](https://github.com/kuriyama/tezzer/actions/workflows/ci.yml)

> 日本語のコンセプト概要は [README.ja.md](README.ja.md) にあります。

tezzer is a lightweight, SSH-like terminal transport with persistent sessions
and automatic reconnection. Like a snail, it carries its shell on its back —
so a dropped connection, a sleeping laptop, or a change of network is just
tezzer pulling back in for a moment. Sessions survive.

It forwards terminal output verbatim and intentionally avoids virtual terminal
emulation or screen management. If an application works correctly over raw SSH,
it is expected to work the same way over tezzer.

tezzer focuses on being a transparent transport layer while providing better
observability — especially for its QUIC transport (packet loss, reordering,
local drops, RTT).

> **If raw SSH would not do it, tezzer should not do it either.**

The same rule applies to security: tezzer introduces no authentication scheme
of its own. Trust is bootstrapped over SSH — the client obtains a session key
over an SSH-forwarded Unix socket, and the UDP port accepts only peers that
prove knowledge of that key (mutual TLS pinned to it). Reaching a tezzer
session requires the same SSH access you already have. Details in the
[security model](docs/security-model.md).

> ⚠️ tezzer is **pre-1.0**. The wire protocol and command-line interface may
> change without notice until a 1.0 release.

## Key characteristics

- SSH-like behavior and compatibility
- Verbatim PTY output forwarding (no escape-sequence rewriting, no VT emulation)
- Works naturally with full-screen apps such as `less`, `vim`, `tmux`, `zellij`, `screen`
- Persistent sessions that survive client disconnects
- Automatic reconnection with session resume, across sleep and roaming
- QUIC over UDP (TLS 1.3, mutual key pinning bootstrapped through SSH) with NAT
  traversal, plus a Unix-domain-socket control channel (forwarded over SSH for
  remote use)
- TCP port forwarding (`-L`) whose tunnels survive sleep and roaming
- Transport-level observability

## Installation

### From source

```bash
git clone https://github.com/kuriyama/tezzer
cd tezzer
make build
```

This produces `bin/tezzerd` (server) and `bin/tezzer` (client). Requires Go 1.25+.

Optionally install them, along with the SSH wrapper:

```bash
sudo install bin/tezzerd bin/tezzer /usr/local/bin/
sudo install scripts/tezzer-ssh /usr/local/bin/
```

## Quick start

### Local (same host)

```bash
tezzerd      # start the server (listens on a Unix domain socket)
tezzer       # connect; creates and attaches a session running your shell
```

### Remote (over SSH)

Use the `tezzer-ssh` wrapper. It starts `tezzerd` on the remote host if needed,
forwards the control socket over SSH, and connects the client (UDP is negotiated
automatically):

```bash
tezzer-ssh myserver run -- tmux        # myserver = any ssh(1) target
```

## Usage

### Server (`tezzerd`)

```bash
tezzerd                          # listen on the default Unix domain socket
tezzerd --listen-unix /path.sock # custom socket path
tezzerd --udp-port 7020          # fixed UDP port (otherwise auto-assigned)
tezzerd --stun-server host:port  # STUN server for NAT traversal
tezzerd --ipv4-only              # IPv4 only
tezzerd --max-sessions 20        # limit active sessions (0 = unlimited, default)
```

`tezzerd` accepts client connections over a Unix domain socket. Remote clients
reach it by forwarding that socket over SSH (see `tezzer-ssh`).

### Client (`tezzer`)

```bash
tezzer                       # create + attach a session running $SHELL (default /bin/bash)
tezzer -cmd zsh              # create a session running a specific command
tezzer -name work            # attach the session named "work", or create it
                             #   (attach-or-create, like tmux new -A -s work)
tezzer -- tmux new -A        # everything after -- is the command + args
tezzer -list                 # list sessions and exit
tezzer -list -json           # same, as JSON (for scripting; also works with -info/-stats)
tezzer -resume               # attach to the most recent session
tezzer -session <id>         # attach to a specific session
tezzer -info <id>            # show session info and exit
tezzer -kill <id>            # kill a session and exit
tezzer -wait -name work      # wait until the session's command exits, then exit
                             #   with its code (select via -session/-resume/-name)
tezzer -peek -name work      # read-only attach: watch the session without any
                             #   risk of sending keystrokes (or resizing its PTY)
tezzer -ipv4-only            # IPv4 only
tezzer -L 8080:localhost:3000   # forward local port 8080 to localhost:3000 on
                                # the server side (ssh -L compatible; repeatable;
                                # bind is loopback-only)
tezzer -N -name work -L 8080:localhost:3000
                                # forward-only attach to an existing session:
                                # no terminal I/O, just the tunnels (like ssh -N)
```

Port forwards ride the same QUIC connection as the terminal: a network change
or sleep that the connection survives (migration) keeps even in-flight TCP
connections alive; after a full reconnect the tunnel definition still works for
new connections. Forwarding can be disabled server-wide with
`tezzerd --no-tcp-forwarding`. `tezzer-ssh` passes `-L` through:
`tezzer-ssh myserver run --name work -L 8080:localhost:3000 -- zsh`.

To add a tunnel to a session you are already attached to, open one from another
terminal with `-N` (`tezzer-ssh myserver forward --name work -L ...`).

**Exit status is propagated like ssh.** When the session's command exits while
you are attached, the client exits with the same status code (signal death maps
to 128+signal, following shell convention). Detaching returns 0 — the session
is still running.

**Tunnel lifecycle is per client.** Closing a client — Ctrl-C on an `-N`, or
detaching an interactive session — closes its listeners and its in-flight
forwarded connections; other clients' tunnels are unaffected. There is no
command to cancel a single `-L` on a live client: to drop one from an
interactive session, detach (`Ctrl-^ .`) and resume without it — reattaching
is cheap, the session keeps running. Tunnels you expect to add and remove
individually are best opened with `-N` in the first place, so their lifetime
is independent of your interactive attachments.

### `tezzer-ssh` wrapper

```bash
tezzer-ssh <ssh-target> <subcommand> [options] [-- <cmd> [args...]]
```

Subcommands:

```bash
tezzer-ssh myserver run -- tmux new -A   # run a new session
tezzer-ssh myserver run --name work -- zsh   # named session (attach-or-create)
tezzer-ssh myserver list                 # list sessions
tezzer-ssh myserver resume               # attach the most recent session
tezzer-ssh myserver resume --session <id>
tezzer-ssh myserver resume --name work   # attach-or-create by name
tezzer-ssh myserver forward --name work -L 8080:localhost:3000   # tunnels only
tezzer-ssh myserver info --session <id>
tezzer-ssh myserver kill --session <id>
tezzer-ssh myserver wait --name build    # block until the session command exits
                                         # (exit code propagated)
tezzer-ssh myserver peek --name agent    # read-only attach (never sends input)
tezzer-ssh myserver run --ipv4-only -- tmux new -A
```

### Keyboard shortcuts (while connected)

The escape prefix is `Ctrl-^` (Ctrl-Shift-6) by default; change it with
`tezzer -escape-key`.

- `Ctrl-^ .` — detach from the session
- `Ctrl-^ i` — show connection status (state, RTT, output freshness)
- `Ctrl-^ h` — show help
- `Ctrl-^ s` — write stats to `~/.tezzer/stats/<session>.stats.json` (see `tezzer -stats`)
- `Ctrl-^ d` — toggle debug output
- `Ctrl-^ q` — kill the session
- `Ctrl-^ r` — reset scroll region and clear the screen
- `Ctrl-^ f` — force a full server-side redraw

All of these flash a single status line at the top of the terminal for a few
seconds; they do not restore whatever was there before — full-screen apps
(vim, tmux, etc.) repaint it on their own.

## Do you still need a remote multiplexer?

Many people run tmux or screen on the remote host for one reason: so their
shells survive a dropped connection. tezzer already does that. If persistence
is all you use a remote multiplexer for, you can let tezzer handle it and keep
window management in your local terminal — where tabs, splits, native
scrollback, and native copy-paste already work the way you expect, with no
prefix-key nesting and no tmux version differences between hosts.

The recipe is **one named session per local tab**:

```bash
tezzer-ssh myserver run --name work -- zsh    # tab 1
tezzer-ssh myserver run --name agent -- zsh   # tab 2
```

`--name` is attach-or-create: the same command creates the session the first
time and reconnects to it every time after — across sleep, network changes,
and client restarts. `resume --name work` does the same (creating with the
default shell if needed). To see what is running on a host:
`tezzer-ssh myserver list`.

Trade-offs compared to a remote tmux:

- The window layout lives in your local terminal, so a different device does
  not inherit it; you reattach by name instead.
- tmux features beyond persistence (remote panes, copy-mode, scripted layouts)
  are not replaced. Multiple clients *can* attach the same tezzer session, so
  simple session sharing still works.

This workflow is optional — tmux and screen run unchanged over tezzer
(`tezzer-ssh myserver run -- tmux new -A`) if you prefer them.

## Running AI agents (and other long-running tools)

A coding agent on a remote host is a session that runs for hours, prints a
lot, and occasionally needs your attention. That is the workload tezzer's
persistence model was built for — one named session per agent:

```bash
tezzer-ssh myserver run --name agent -A -- claude
```

Close the laptop, change networks, come back with
`tezzer-ssh myserver resume --name agent` — the agent keeps working while you
are detached, and `-A` (SSH agent forwarding) keeps its `git push` working
after every reattach. Long unattended batch runs compose with exit-status
propagation: `tezzer-ssh myserver run -- claude -p "..."` survives disconnects
that would kill a plain SSH session, and still returns the command's exit code.
To get notified when a detached session finishes, wait on it from anywhere:
`tezzer-ssh myserver wait --name agent && notify-send "agent done"`. And to
check on a working agent without the risk of stray keystrokes reaching it,
attach read-only: `tezzer-ssh myserver peek --name agent` — output flows,
input never does (detach with `Ctrl-^ .` as usual).

Attention signals also survive the trip. Because output is forwarded
verbatim, the terminal bell and desktop-notification sequences (OSC 9,
OSC 777, OSC 99) arrive at your local terminal byte-exact, so terminals that
support them (WezTerm, kitty, iTerm2, …) can turn "the agent wants input"
into a real notification. Measured against the alternatives: a remote tmux
passes only the bell unless you configure `allow-passthrough on` and the
sender wraps its sequences; mosh forwards the bell but drops the OSC forms
(details in the [mosh comparison](docs/mosh-comparison.md)).

## Running tezzerd as a systemd user service

```bash
mkdir -p ~/.config/systemd/user

cat > ~/.config/systemd/user/tezzerd.service <<'EOF'
[Unit]
Description=tezzer terminal server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/tezzerd --udp-port 7020
Restart=always
RestartSec=5
# Environment needed for screen/tmux to behave correctly:
Environment=TERM=xterm-256color
Environment=SHELL=/bin/bash
# If you use GNU screen and sessions are not found, screen's socket directory
# may differ when launched from systemd. Set SCREENDIR to match your system,
# e.g. (path varies by distribution):
#   Environment=SCREENDIR=/run/screen/S-%u

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now tezzerd
```

To keep the service running when you are not logged in:

```bash
loginctl enable-linger "$USER"
```

If GNU screen sessions are not found under systemd, check the actual socket
directory with `ls -la /run/screen/S-"$USER"/` and set `SCREENDIR` accordingly.

## Client log files

The client (including via `tezzer-ssh`) writes logs to
`~/.tezzer/logs/client-YYYYMMDD-HHmmss-<pid>.log`:

- Minimal logs (connection state, errors) are always written.
- With `TEZZER_DEBUG=1`, full debug output is also written.
- Files older than 7 days are deleted at startup.

## Documentation

- [Design philosophy](docs/design-philosophy.md)
- [tezzer vs mosh](docs/mosh-comparison.md)
- [Architecture](docs/architecture.md)
- [Protocol](docs/protocol.md)
- [Security model](docs/security-model.md)
- [Architecture Decision Records](docs/adr/)
- [Contributing](CONTRIBUTING.md), [Security policy](SECURITY.md)

## License

Licensed under the [Apache License 2.0](LICENSE).
