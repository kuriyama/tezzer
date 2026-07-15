# 0001 — No virtual-terminal emulation

## Status

Accepted.

## Context

An early design considered keeping virtual-terminal (VT) state on the server so
that a reconnecting client could be shown a fully restored screen. That would
have required a VT engine, full-screen snapshots, a frame/diff history ring, line
hashing, and Unicode width calculation (including East Asian Width).

## Decision

**Do not implement VT emulation. Relay PTY output verbatim.**

## Rationale

- **Complexity and bug risk.** A correct VT engine is large (ANSI/VT100/VT102/
  xterm), and Unicode width handling is terminal-dependent and hard to get right.
  Partial implementations tend to produce rendering glitches.
- **Lessons from similar tools.** mosh embeds a VT engine and has many reported
  interaction problems with tmux/screen; EternalTerminal has reports of unbounded
  buffer growth. Both stem from trying to hold "correct" screen state.
- **SSH compatibility.** Raw SSH keeps no VT state and restores no screen; it
  just forwards PTY output. Matching that behavior gives the same compatibility
  and predictability.
- **Tool compatibility.** tmux/screen already have their own VT implementations,
  and vim/less/top and TUI frameworks drive the terminal directly. They assume
  bytes are forwarded as-is.
- **CPU and memory.** No VT parser, no screen state, no diffing — so tezzer stays
  light and can handle many sessions.

## Consequences

We lose full screen restoration on reconnect and recovery of output that
overflowed while a client was away. Instead, the most recent output chunks are
replayed, and the user can redraw with `Ctrl-L` or re-run a command. Running
tmux/screen inside tezzer gives a second layer of persistence if desired.

In exchange we get simplicity, high compatibility with existing tools, low CPU
and memory use, predictable behavior, and greatly reduced bug risk.
