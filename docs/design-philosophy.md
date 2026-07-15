# Design Philosophy

tezzer is designed to operate at the same level of abstraction as raw SSH. Its
job is to move terminal input and output between a local terminal and a remote
PTY with minimal interference. It prioritizes correctness, transparency, and
compatibility over features.

## The guiding rule

> **If raw SSH would not do it, tezzer should not do it either.**

This is the primary lens for evaluating new features, changes, and
contributions.

## What tezzer does

- Lightweight, SSH-like terminal transport
- Transparent forwarding of PTY output (escape sequences preserved as-is)
- Compatibility with full-screen terminal applications
- Transport-level observability, especially for UDP: packet loss, local drops,
  reordering, delayed arrival, and RTT metrics

tezzer behaves like the "wire" between a remote PTY and a local terminal.

## What tezzer does not do (non-goals)

tezzer intentionally does not implement:

- **VT state tracking or differential screen sync.** Screen correctness is left
  to the terminal emulator. PTY output is relayed raw; cursor position, screen
  contents, and scroll regions are not tracked.
- **Interpreting, rewriting, or optimizing escape sequences.**
- **Disk persistence.** Restoring a running PTY process is not practical; a
  server restart loses sessions (use systemd or similar to auto-restart). See
  [ADR 0002](adr/0002-no-disk-persistence.md).
- **Session multiplexing / window management.** That is left to tmux/screen.
- **Acting as a terminal UI framework.**
- **Its own public-key authentication.** tezzer relies on SSH for that.
- **Multi-host / cluster features.** Single host, single binary.

If a feature requires understanding or reconstructing terminal state, it is
considered out of scope.

## Explicit, short-lived local intervention

tezzer may take **explicit, short-lived** local control only when all of the
following hold: the user explicitly requested it (e.g. via a hotkey); the
intervention is temporary and self-contained; and it is clearly separated from
normal data transfer. Examples: the escape-key commands (`Ctrl-^ i/h/s/...`,
see [README](../README.md#keyboard-shortcuts-while-connected)) and detach /
fatal-error notices.

These interventions flash a single reverse-video status line at the top of the
terminal for a few seconds, then blank it. This does **not** save or restore
whatever was actually on that row beforehand — tezzer keeps no screen buffer
to restore from (see "What tezzer does not do" above). Full-screen
applications (vim, tmux, etc.) repaint the row on their own; in a plain shell
at the very top of a fresh terminal, the row is simply left blank until
something else overwrites it. Because of this, an intervention's message must
fit within a single terminal row (assume ~80 columns) — anything that wraps
leaves stray content on a second row that nothing will ever clear.

Outside these explicit cases, tezzer does not alter terminal behavior.

## Future direction

tezzer intentionally avoids becoming a terminal multiplexer, a remote UI
framework, or a virtual terminal emulator. Future work should preserve its role
as a transparent, predictable transport layer. Features that undermine this
should be rejected, or carefully isolated as explicit, opt-in behavior.
