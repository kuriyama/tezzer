# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it reaches a 1.0 release.

## [Unreleased]

## [0.2.1] - 2026-10-21

### Changed

- **Incompatible with 0.2.0 and earlier releases:** the QUIC ALPN identifier
  of release binaries changes from `github.com/kuriyama/tezzer/1` to
  `tezzer/1`, the identifier the source has always declared (the release
  build rewrote it by mistake). A client and a `tezzerd` from either side of
  this change cannot connect — the handshake fails with `CRYPTO_ERROR 0x178`
  (`no application protocol`) — so upgrade both ends together.

### Fixed

- Release binaries (GitHub Releases / Homebrew) now carry their version and
  build time: `-version` showed `dev` and the commit time instead of the
  release version and build time, because the build-time variables were not
  set when building from the public module path.

## [0.2.0] - 2026-10-17

### Added

- Remote port forwarding: `tezzer -R [bind:]port:host:hostport` (ssh-compatible,
  repeatable, loopback-only bind) makes `tezzerd` listen on the server's
  `localhost:port` and relay each connection to the client, which dials
  `host:hostport` from its own machine — for example to drive a local Chrome
  over the DevTools Protocol from the server. The listener belongs to the
  server rather than to one client: clients on the same machine (identified by
  a random secret in `~/.tezzer/device-id` hashed with the hostname, so
  machines sharing a home directory stay distinct) share it, so the same `-R`
  can go in every terminal and the tunnel lasts while any of them is
  connected. Another
  machine asking for the same port waits and takes it over once the first
  machine's clients have exited; a port held by another program is retried
  every 30 seconds; when a machine's clients lose their connection rather than
  exit, the port stays bound (refusing connections) for up to 24 hours. Shown
  at the end of `tezzer -list` and as `remote_forwards` in `-list -json`;
  `Ctrl-^ i` shows `rfwd:<active>/<defined>`. Works with `-N` and
  `tezzer-ssh ... forward`, and is disabled by `tezzerd --no-tcp-forwarding`.
  As with `ssh -R`, other local users on the server can connect to the port.
  Needs both client and server updated.
- `tezzer -version` and `tezzerd -version` print the release version
  (`x.y.z`; development builds show the latest release plus `+dev`), the
  git commit (with `-dirty` for uncommitted changes), the build time, and
  the Go version and platform.
- `tezzer -server-version` asks the running `tezzerd` for its version, and
  `tezzer-ssh HOST version` prints the local `tezzer`, the running remote
  `tezzerd`, and the installed remote `tezzerd` side by side (a mismatch
  between the last two means the server was upgraded but not restarted).
  `tezzer-ssh -version` prints the local version only.
- When `-name` attaches to an existing session but a different command was
  given (e.g. `tezzer-ssh host run --name work -- zsh` for a session started
  with `zellij`), the client now says the session already exists, what it is
  running, and that the given command is not used, instead of silently
  ignoring it. Running the same command line again (same command, no
  arguments) attaches without this message.
- `Ctrl-^ i` now shows the session name (`name:work sess:abc123 | ...`) for
  named sessions, however you attached (`-name`, `-resume`, or `-session`).
  Names longer than 20 characters are truncated. With an older server the
  name is shown only when you attached with `-name`.
- Experimental `TEZZER_STATUS=redraw` (opt-in): after the status line is
  cleared, the client nudges the remote app to repaint (the `Ctrl-^ f`
  resize trick), but only while it has seen the app switch to the alternate
  screen. This brings back top bars that the status line blanked and that
  the app would not otherwise repaint soon (e.g. Zellij's tab bar, tmux with
  `status-position top`). The inferred screen state is shown in `Ctrl-^ i`.
- Experimental `TEZZER_STATUS=off` (opt-in): unsolicited notifications are
  written only to the client log instead of the top status line; status lines
  requested with a key (`Ctrl-^ i`, `Ctrl-^ h`, ...) are still shown.
- `tezzerd --check-nat` now also reports the same-LAN candidate address and
  which route family (IPv4/IPv6) it resolved from, so IPv4-only/IPv6-only/
  dual-stack behavior can be checked with a single command.
- The client now refreshes its QUIC reconnect candidate list in the
  background every 90s (cheap local-route check first, then a real STUN
  query only for families that appear to have a route), instead of only
  re-deriving candidates reactively after every stored candidate has
  already failed. On dual-stack links where one address family dies
  independently (e.g. IPv4 CGN port exhaustion) while the other stays up,
  reconnect can pick up the still-healthy family without needing the
  slower reactive refresh path.
- On dual-stack links, the client now notices within about 10 seconds when
  the address family it is connected over (IPv4 or IPv6) stops responding,
  and switches to the other family. Previously this took over a minute
  (the QUIC idle timeout). The client only checks when the other family is
  available, and if both are down it waits as before rather than retrying
  early. It does not switch back when the original family recovers.
  Requires an updated `tezzerd`.

### Changed

- A bare `tezzer` (no `-cmd` and nothing after `--`) now runs the server
  user's login shell (from `/etc/passwd`, else tezzerd's `$SHELL`, else
  `/bin/sh`) instead of always `/bin/bash`, like `ssh host`: it is started as
  a login shell (`argv[0]` such as `-zsh`, so profile files are read) in the
  home directory. Older servers keep running `/bin/bash`, and older clients
  keep getting `/bin/bash`.
- Detaching (or exiting) while a full-screen app such as zellij, tmux or vim
  is running now returns the terminal to the screen you had before starting
  `tezzer`, like quitting tmux or mosh. Previously the remote app's last
  screen stayed visible after detaching. See the related entry under Fixed.
- After a sleep of 45 seconds or more, the client now reconnects right
  away instead of first trying to move the existing connection to a new
  socket. By then the server has usually dropped the connection, so that
  attempt only added a 4-second wait before reconnecting.

### Fixed

- The client no longer crashes with `panic: connection ID manager is closed`
  when the connection dies while it is trying to roam to a new network path
  (for example on wake from sleep). It now falls back to reconnecting.
- Attaching again to a session running a full-screen app such as zellij
  (after `Ctrl-^ .`, or from another terminal) now brings back the modes
  the app had turned on: mouse support, bracketed paste, focus events and
  the alternate screen. Previously the mouse stayed off until the app was
  restarted, because apps turn these on only once at startup. Requires an
  updated `tezzerd`.
- Starting a new session with a command that `tezzerd` cannot find (e.g.
  `tezzer-ssh host run -- zellij` when `zellij` is installed in a directory
  that is not in `tezzerd`'s `PATH`, which is common under systemd) left the
  client stuck after `connected to tezzerd` with nothing on screen. The
  client now exits with an error that names the command and shows
  `tezzerd`'s `PATH`. If the command is found but fails to start, the
  client is also told and the session is closed.
- After the client recovered from a sleep or network change by moving the
  existing connection to a new local socket (`roaming: migrated` in the
  log), the connection broke about 30 seconds later and the client had to
  reconnect (`reconnecting: connection lost`). The connection now stays up.
  A failed attempt to move the connection also no longer breaks a
  connection that is still working.
- `tezzer -- CMD [args]` (without `-cmd`) ran `/bin/bash CMD [args]`, so
  bash tried to read `CMD` as a script and the session exited immediately.
  The first argument after `--` is now the command, as the README already
  described. `-cmd CMD -- ARGS` (the form `tezzer-ssh` uses) is unchanged.
- The server's same-LAN candidate address (used to avoid a public-STUN
  round-trip when client and server share a LAN) was resolved via an
  IPv4-only route probe, so it was silently omitted on IPv6-only hosts.
  It now falls back to an IPv6 route probe when IPv4 is unavailable
  (unless `-ipv4-only` is set).
- After detaching (or when the session ended) from a full-screen app such
  as zellij, the local terminal was left in the modes the app had enabled,
  so moving the mouse typed garbage like `^[[<35;12;5M` into the local
  shell, and the shell kept running on the alternate screen. The client now
  resets mouse reporting, focus events, bracketed paste, the kitty keyboard
  protocol, application cursor/keypad modes and the alternate screen on
  exit, before printing the exit message.
- Pasting a large amount of text (a few hundred KB) into an app that echoes
  its input, such as a shell, could deadlock the server: the session froze
  and `tezzerd` stopped accepting new sessions until it was restarted. The
  server held a session lock while writing input to the PTY, which blocks
  until the app reads it, while the app was itself blocked waiting for the
  server to read its echoed output.

## [0.1.1] - 2026-07-22

### Changed

- README.ja.md's "no persistence" caveat now reflects zero-downtime restart
  (`SIGUSR2`): a planned binary-update restart preserves the session, while
  an unexpected crash (OOM kill, SIGKILL) still loses it. The previous
  wording predated the self-re-exec restart feature and said any server
  restart lost the session.
- README documents a `systemctl kill` pitfall for zero-downtime restarts:
  the default `--kill-who=all` signals every process in the unit's cgroup,
  silently killing the shells/PTYs tezzerd manages instead of preserving
  them. Use `systemctl --user kill --kill-who=main -s SIGUSR2 tezzerd` (or
  signal the main PID directly) instead.

### Fixed

- A client could print `[Tezzer] Session closed: ...` twice when a PTY
  session ended, because the UDS and QUIC notification paths could each
  announce it independently. Only the first to arrive is now shown.

## [0.1.0] - 2026-07-19

### Added

- FreeBSD builds (`freebsd/amd64`, `freebsd/arm64`) in release artifacts and
  `make dist`. Peer-credential checks on the Unix socket use `LOCAL_PEERCRED`
  (as on macOS). Cross-compiled; not yet CI-tested on real hardware.

### Changed

- The godoc surface — package comments and documentation comments on exported
  identifiers — is now written in English. Inline implementation comments
  remain primarily in Japanese; CONTRIBUTING.md documents the
  comment-language policy.
- SECURITY.md now describes the actual transport (QUIC with mutual TLS 1.3,
  keyed from a per-session secret bootstrapped over an SSH-forwarded Unix
  socket). The stale description of a TCP fallback channel is gone; tezzer
  has no TCP listener.
- README installation instructions now lead with Linux and include
  copy-pasteable tarball commands (release asset URLs are stable across
  versions), with macOS (Homebrew) and FreeBSD sections alongside.

## [0.0.2] - 2026-07-18

### Added

- `tezzerd --check-nat`: one-shot NAT diagnosis and exit. Probes two STUN
  servers from a single socket per address family and reports your public
  address, the NAT mapping behavior (endpoint-independent "cone" vs
  destination-dependent "symmetric"), port preservation, and an actionable
  verdict (whether STUN-derived direct QUIC will work, or whether to set up
  `--udp-port` with router port forwarding). The comparison server defaults to
  Cloudflare and can be overridden with `--stun-server2`.
- The server now tells clients which STUN server it uses
  (`SESSION_CREATED.stun_server`), so client-side STUN queries follow
  `tezzerd --stun-server` instead of always querying Google. Old
  clients/servers interoperate (the field is optional).

### Changed

- A client that stops reading output for 30 seconds is now disconnected, so a
  single stalled client (e.g. a laptop asleep mid-burst) can no longer freeze
  the session's output for everyone else. The disconnected client reconnects
  and resynchronizes automatically with no data loss. This completes the
  backpressure work started with the stall warnings in 0.0.1.
- Reconnect attempts now back off exponentially (1s doubling to a 60s cap)
  after repeated failures, instead of redialing (and re-querying STUN) in a
  tight loop while the server is unreachable. Typing, sleep/wake, and a
  successful reconnect reset the backoff, so an active user is never delayed.
- Resynchronization after long sleeps streams the retained backlog in bounded
  batches instead of decompressing all of it into memory at once.
- The server caches STUN lookup results (5 minutes on success, 30 seconds on
  failure), removing repeated blocking queries — and repeated 5-second
  timeouts in STUN-blocked environments — from session create/attach.
- Client status and log messages now say "QUIC" instead of the legacy "UDP"
  wording (including the `-list` / `-info` client detail labels).

### Fixed

- `tezzerd --stun-server` was ignored: STUN queries always went to hardcoded
  Google/Cloudflare servers regardless of the flag.
- Removed the NAT-type detection that misreported almost any NAT as
  "symmetric" (it compared mapped ports across different local sockets, which
  is meaningless). Correct same-socket probing now backs `--check-nat`.
- The client now exits cleanly with a clear message when the server reports it
  cannot continue the session (e.g. QUIC establishment timeout, with a hint to
  check UDP reachability), instead of sitting attached to a dead session.
- Fixed rare frame corruption races on the Unix socket when responses and
  streamed output were written (or read) concurrently.
- Fixed a UDP socket leak: transports retired by reconnect/migration were kept
  open until the client exited (one leaked socket per recovery).
- Fixed a 1-in-65536 client ID collision with the reserved value 0 that could
  cause session output to be sent twice (over both the SSH path and QUIC).

### Removed

- The client-side `tezzer -udp-port` flag. It was accepted but had no effect;
  scripts passing it must drop it (**breaking**). The server-side
  `tezzerd --udp-port` is unchanged.

## [0.0.1] - 2026-07-16

Initial public release. tezzer is pre-1.0 and its wire protocol and
command-line interface may change without notice until 1.0.

### Added

- Forward-only attach: `tezzer -N -name work -L ...` attaches to an existing
  session without terminal I/O, carrying only port forwards (like `ssh -N`).
  Use it to add tunnels to a running session from another terminal;
  `tezzer-ssh <host> forward --name work -L ...` wraps it.
- TCP port forwarding: `tezzer -L [bind:]port:host:hostport` (ssh-compatible,
  repeatable). Tunnels ride the QUIC connection: connection migration keeps
  in-flight TCP connections alive across sleep/roaming, and tunnel definitions
  survive full reconnects. Binds are loopback-only by design; disable
  server-wide with `tezzerd --no-tcp-forwarding`. Dial targets are logged on
  the server. See docs/security-model.md.
- Named sessions: `tezzer -name work` attaches the session named "work" if it
  exists, otherwise creates it (like `tmux new -A -s work`). Names are unique
  among active sessions. `tezzer-ssh` supports `run --name` / `resume --name`,
  and names appear in `-list` / `-info` (including JSON output).
- `tezzer -list -json` / `tezzer -info <id> -json`: machine-readable JSON output
  for scripting (status bars, prompts, monitoring). Timestamps are RFC3339.
- STUN discovery now queries IPv4 and IPv6 independently and reports every
  address family that succeeds, instead of whichever one the OS resolver
  happened to pick. `SESSION_CREATED.stun_addr` (single string) is replaced by
  `stun_addrs` (list, 0–2 entries); this increases the initial-connection
  candidate set on dual-stack hosts. `--ipv4-only` still disables the IPv6
  query on both client and server.
