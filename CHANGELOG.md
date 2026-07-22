# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it reaches a 1.0 release.

## [Unreleased]

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
