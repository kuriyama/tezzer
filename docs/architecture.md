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
- **Compatibility**: tmux/zellij/screen/zsh/vim/less and friends run unmodified.
- **Robustness**: no shared bottlenecks or unbounded buffers, to minimize the
  risk of a stall affecting everyone.

## Components

### `tezzerd` (server)

- **Unix domain socket listener** for client connections (remote access is via
  an SSH-forwarded socket).
- **Session manager** and **per-session PTY runner**.
- **Output relay**: PTY output is delivered raw (no VT interpretation).
- **Client fan-out**: each client has an independent send queue.
- **QUIC transport manager**: establishes and maintains the QUIC connection
  (terminal I/O and TCP port forwarding).

### `tezzer` (client)

- Connects over the Unix domain socket (SSH-forwarded for remote use).
- Creates or attaches a session; forwards stdin; displays PTY output.
- Detects disconnects and reconnects automatically; propagates terminal resizes.
- Runs a QUIC transport manager for the parallel encrypted channel.

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

## QUIC channel

- **Transport**: QUIC (via [quic-go](https://github.com/quic-go/quic-go)) over
  UDP, carrying terminal I/O and TCP port forwarding.
- **Encryption**: QUIC's TLS 1.3. The connection is authenticated by mutual
  proof of knowledge of the shared key **K** (mTLS pinned to K-derived Ed25519
  identities; no PKI) — see [security-model.md](security-model.md).
- **Key exchange**: K is distributed over the trusted UDS control channel.
- See [protocol.md](protocol.md) for the stream layout, frame types, and the
  migration/reconnect flow.

## Reconnection

- **Control channel**: application-level Ping/Pong; missing Pongs trigger a
  disconnect.
- **QUIC channel**: idle timeout 60 s, keep-alive 15 s. Below the idle timeout,
  an address change (roaming, NAT rebind, sleep/resume) is handled as
  **migration** — the connection, and every stream on it, survives. Beyond it,
  the client performs a full **reconnect**: a new connection dialed in
  parallel to all candidate addresses, with output resync driven by the last
  received offset.
- **Retry**: exponential backoff (1s → 2s → 4s → … capped at 60s).

## Memory

- Output ring buffer: per session, 4 MB raw (hot tier) + 32 MB compressed
  (cold tier; terminal output typically compresses 5–20×, so this covers
  hundreds of MB of raw output). Typical interactive use is far smaller.
- Per QUIC client: send/receive buffering and retransmission/reordering are
  managed by quic-go (per-stream flow-control windows); tezzer does not keep
  its own buffers on this path. Real usage depends on output volume and
  disconnect duration.
