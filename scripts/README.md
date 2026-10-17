# scripts

Helper scripts that ship with tezzer.

## `tezzer-ssh`

A wrapper for connecting to a remote `tezzerd` over SSH. It starts `tezzerd` on
the remote host if it is not already running, forwards the control socket over
SSH, and launches the `tezzer` client (UDP is negotiated automatically).

```bash
tezzer-ssh <ssh-target> <subcommand> [options] [-- <cmd> [args...]]
```

Subcommands:

| Subcommand | What it does |
|---|---|
| `run [--name N] -- CMD [args]` | Create a session running `CMD` (with `--name`, attach if `N` exists) |
| `list` | List sessions |
| `resume [--session ID \| --name N]` | Attach the most recent session, or a specific one (`--name` attaches or creates) |
| `forward [--session ID \| --name N] -L ...` | Forward ports only, without a terminal (`tezzer -N`) |
| `info --session ID` | Show session details |
| `kill --session ID` | Kill a session |
| `wait [--session ID \| --name N]` | Block until the session's command exits; its exit code is propagated |
| `peek [--session ID \| --name N]` | Read-only attach (never sends input) |
| `version` | Show the local `tezzer`, the running remote `tezzerd`, and the installed remote `tezzerd` (does not start `tezzerd`) |

For example:

```bash
tezzer-ssh myserver run -- tmux new -A      # start a new session
tezzer-ssh myserver run --name work -- zsh  # named session (attach-or-create)
tezzer-ssh myserver resume --name work      # reattach by name
tezzer-ssh myserver forward --name work -L 8080:localhost:3000
tezzer-ssh myserver wait --name build && notify-send done
tezzer-ssh myserver version
tezzer-ssh -version                         # local tezzer version only
```

Environment variables: `TEZZER_CLIENT` / `TEZZER_SERVER` (binary paths),
`TEZZER_SERVER_ARGS` (arguments for an auto-started `tezzerd`, e.g. a fixed
`--udp-port`), and `TEZZER_INSTANCE` (run a separate `tezzerd` side by side).

`<ssh-target>` is any host that `ssh(1)` understands (a hostname or a `Host`
alias from your SSH config). Run `tezzer-ssh --help` for full usage, and see the
[README](../README.md#tezzer-ssh-wrapper) for details.

## `e2e-docker.sh`

A manual end-to-end test that exercises sleep/resume recovery against real
binaries inside a container (see the `make e2e-docker` target). It is not part
of `make ci`. By default it uses `docker`; override with the `DOCKER`
environment variable for another runtime (for example rootless udocker:
`DOCKER=udocker ./scripts/e2e-docker.sh`).

## `tui-probe.sh`

Manual measurements for the status line, redraw, and paste behavior (not part
of `make ci`). It builds an image with tmux, screen, and Zellij
(`tui-probe/Dockerfile`) and runs one of three probes inside it:

```bash
scripts/tui-probe.sh winch            # does each multiplexer repaint on a same-size
                                      # SIGWINCH, compared with the resize trick?
scripts/tui-probe.sh status redraw    # tezzer + Zellij: is the tab bar back after the
                                      # status line clears? (none / redraw / off)
scripts/tui-probe.sh paste 256 1024   # how long does a large paste (KB) take, direct
                                      # PTY vs. tezzer (sink / echo / inside Zellij)?
scripts/tui-probe.sh paste --rtt=100 --loss=2 1024
                                      # same, with tc netem delay / loss on the
                                      # container's loopback (needs NET_ADMIN)
```

Like `e2e-docker.sh`, override the runtime with `DOCKER` (for example
`DOCKER=udocker scripts/tui-probe.sh winch`). Building the image needs network
access to the Debian mirrors and GitHub Releases (for Zellij).
