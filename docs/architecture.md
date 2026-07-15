# Architecture

## Design goals

**tezzer does not try to reproduce "the correct screen." It aims to keep going,
let you reconnect, and stay pleasant to type on.**

- **Persistent sessions**: one session = one PTY. The PTY survives client
  disconnects.
- **Reconnection**: clients reattach by `session_id`; the most recent output
  chunks are replayed (no full screen restore).
- **Verbatim PTY relay**: input/output is relayed raw; no VT state is kept.
- **Multiple clients**: clients attached to the same PTY all receive its output.
- **Low CPU**: no VT parser, no diffing — just byte relay.
- **Compatibility**: screen/tmux/zsh/vim/less and friends run unmodified.
- **Robustness**: no shared bottlenecks or unbounded buffers, to minimize the
  risk of a stall affecting everyone.

## Components

### `tezzerd` (server)

- **Unix domain socket listener** for client connections (remote access is via
  an SSH-forwarded socket).
- **Session manager** and **per-session PTY runner**.
- **Output relay**: PTY output is delivered raw (no VT interpretation).
- **Client fan-out**: each client has an independent send queue.
- **UDP manager**: provides the encrypted UDP channel.

### `tezzer` (client)

- Connects over the Unix domain socket (SSH-forwarded for remote use).
- Creates or attaches a session; forwards stdin; displays PTY output.
- Detects disconnects and reconnects automatically; propagates terminal resizes.
- Runs a UDP manager for the parallel encrypted UDP channel.

## Session model

- Output from a PTY belongs to exactly one session and is delivered only to that
  session's attached clients.
- Each client has an independent send queue, so a slow/stuck client is delayed
  or dropped on its own without stalling the others.
- Per session, output is kept in a two-tier bounded **output ring buffer**: a
  hot tier of recent raw chunks (4 MB, the fast path for roaming/short-drop
  resync) and a cold tier of flate-compressed segments (32 MB compressed,
  the capacity path for multi-day sleep). Retention is capped at 72 hours.
  Beyond the caps the oldest data is dropped and clients are notified
  (`OUTPUT_DROPPED` / redraw prompt).

## Backpressure

- **Independent client queues** isolate a stuck client from the rest.
- **Bounded output ring buffer** prevents unbounded memory growth; overflow is
  surfaced rather than hidden.

## Data flow

Output (server → client): PTY output (raw bytes, including OSC sequences) →
output ring buffer → per-client fan-out → UDS/UDP. OSC sequences are kept inline,
not separated.

Input (client → server): stdin → input message → UDS/UDP → written to the PTY.

## Authentication

- **Local**: the Unix domain socket peer's UID is checked
  (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS); only the same UID as the
  server may connect.
- **Remote**: the socket is forwarded over SSH (`tezzer-ssh`), so SSH provides
  authentication. tezzer has no public-key auth of its own.

## UDP channel

- **Encryption**: AES-256-GCM.
- **Key derivation**: HKDF derives separate keys per direction (client→server,
  server→client) to avoid nonce reuse.
- **Key exchange**: the shared key is distributed over the trusted UDS/TCP
  control channel.
- See [protocol.md](protocol.md) for the packet format and the reconnection
  (REATTACH) flow.

## Reconnection

- **Control channel**: application-level Ping/Pong; missing Pongs trigger a
  disconnect.
- **UDP channel**: periodic KeepAlive; missing replies trigger a re-handshake.
- **Retry**: exponential backoff (1s → 2s → 4s → … capped at 60s).

## Memory

- Output ring buffer: per session, 4 MB raw (hot tier) + 32 MB compressed
  (cold tier; terminal output typically compresses 5–20×, so this covers
  hundreds of MB of raw output). Typical interactive use is far smaller.
- Per UDP client: a bounded send queue, a send buffer (≤16 MiB) for
  retransmission, and a receive buffer (≤8 MiB) for reordering. These are caps;
  real usage depends on output volume and disconnect duration.
