# scripts

Helper scripts that ship with tezzer.

## `tezzer-ssh`

A wrapper for connecting to a remote `tezzerd` over SSH. It starts `tezzerd` on
the remote host if it is not already running, forwards the control socket over
SSH, and launches the `tezzer` client (UDP is negotiated automatically).

```bash
tezzer-ssh <ssh-target> <subcommand> [options] [-- <cmd> [args...]]
```

Subcommands: `run`, `list`, `resume`, `info`, `kill`. For example:

```bash
tezzer-ssh myserver run -- tmux new -A      # start a new session
tezzer-ssh myserver list                    # list sessions
tezzer-ssh myserver resume                  # attach the most recent session
tezzer-ssh myserver info --session <id>
tezzer-ssh myserver kill --session <id>
```

`<ssh-target>` is any host that `ssh(1)` understands (a hostname or a `Host`
alias from your SSH config). Run `tezzer-ssh` with no arguments for full usage.

## `e2e-docker.sh`

A manual end-to-end test that exercises sleep/resume recovery against real
binaries inside a container (see the `make e2e-docker` target). It is not part
of `make ci`. By default it uses `docker`; override with the `DOCKER`
environment variable for another runtime (for example rootless udocker:
`DOCKER=udocker ./scripts/e2e-docker.sh`).
