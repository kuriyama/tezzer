# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it reaches a 1.0 release.

## [Unreleased]

Initial public release is in preparation. tezzer is pre-1.0 and its wire
protocol and command-line interface may change without notice until 1.0.

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
