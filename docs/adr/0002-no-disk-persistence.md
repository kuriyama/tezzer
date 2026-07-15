# 0002 — No disk persistence; bounded in-memory buffers only

## Status

Accepted.

## Context

To improve session durability we considered persisting PTY process state, screen
snapshots, and session history to disk, so sessions could survive a server
restart.

## Decision

**Do not persist to disk. Use bounded in-memory buffers only.**

## Rationale

- **Restoring a PTY process is not practical.** Saving and restoring the runtime
  state of `bash`/`zsh`/`vim` (file descriptors, network connections, kernel
  state) is generally infeasible; tools like CRIU are complex and fragile.
- **No VT state to persist.** Per [ADR 0001](0001-no-vt-emulation.md), tezzer
  keeps no VT state, so there is nothing screen-related to persist.
- **Avoiding unbounded growth.** Persisting output would reintroduce the
  unbounded-buffer / disk-exhaustion class of problems and require cleanup
  policies that add complexity.
- **Simplicity.** Disk persistence brings file locking, transactions, and
  corruption handling — more bugs and maintenance for little benefit here.

## Strategy

- A bounded **output ring buffer** per session (oldest chunks dropped on
  overflow; clients notified via `OUTPUT_DROPPED`). On reconnect the buffer's
  chunks are replayed, giving recent context without a full restore.
- OSC sequences are delivered live but not replayed from history, to avoid
  re-running side effects (e.g. clipboard operations).
- Independent per-client send queues so a stuck client cannot stall others.

## Consequences

A server restart loses sessions; rely on systemd (or similar) to auto-restart
the daemon, and run tmux/screen inside tezzer for a second persistence layer.
In exchange, memory use is predictable and there is no disk I/O or corruption
risk.
