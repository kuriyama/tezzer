# Protocol

tezzer has two channels:

- a reliable **control channel** — a Unix domain socket (UDS), forwarded over
  SSH for remote use. Used for session management (create/attach/list/kill)
  and as the bootstrap channel that hands the client the QUIC parameters.
- an encrypted **QUIC channel** (UDP) — carries terminal I/O and TCP port
  forwarding, and survives roaming, sleep, and NAT rebinding.

The QUIC channel is **required** for terminal I/O on new sessions: the server
withholds PTY startup until QUIC is established (this prevents terminal query
responses, e.g. DA, from leaking down both paths), and aborts session creation
with a `QUIC_TIMEOUT` error if QUIC is not up within 15 seconds
(see [dev/da-response-leak.md](dev/da-response-leak.md)).

> tezzer is pre-1.0: this protocol changes without notice.

## Control channel (UDS)

- Transport: Unix domain socket only; remote access forwards it over SSH.
- Framing: `[u32be length][MessagePack payload]`, max frame 16 MiB.
- Authentication: the peer UID is checked via `SO_PEERCRED` / `LOCAL_PEERCRED`;
  only the same UID as the server may connect.
- Common fields: `type` (string), `v` (protocol version, currently 1).

### Connection sequence

```
Client                          Server
  |--- UDS connect ------------->|
  |<-- WELCOME ------------------|
  |--- HELLO ------------------->|
  |--- CREATE_SESSION or ------->|
  |    ATTACH_SESSION            |
  |<-- SESSION_CREATED ----------|
  |<-- OUTPUT (stream) ----------|   (terminal I/O rides QUIC once established;
  |--- INPUT ------------------->|    the UDS INPUT/OUTPUT path remains for attach)
```

### Messages

Client → server: `HELLO`, `CREATE_SESSION`, `ATTACH_SESSION`, `INPUT`, `RESIZE`,
`PING`, `LIST_SESSIONS`, `GET_SESSION_INFO`, `KILL_SESSION`, `GET_SERVER_META`,
`UDP_CLIENT_INFO`.

Server → client: `WELCOME`, `SESSION_CREATED`, `OUTPUT`, `PONG`, `NOTE`,
`ERROR`, `SERVER_META`, `SESSIONS_LIST`, `SESSION_INFO`, `SESSION_KILLED`.

Selected fields:

- `CREATE_SESSION.name` — optional session name (`-name`); unique among active
  sessions, rejected with `DUPLICATE_NAME` otherwise.
- `ATTACH_SESSION.from_seq` — request output from this sequence number onward.
- `SESSION_CREATED` carries the QUIC parameters: `udp_port`, `udp_key`
  (**K**, the 32-byte shared key distributed over the trusted control channel;
  see [security-model.md](security-model.md)), `udp_session_id`, `stun_addr`,
  and `local_addr` (for same-LAN connections).
- `UDP_CLIENT_INFO` — the client announces its 16-bit `client_id` and STUN
  address before dialing QUIC (needed for routing on a shared transport).
- `OUTPUT.data` — raw PTY bytes including escape sequences (relayed verbatim).
- `NOTE.kind` — `SESSION_CLOSED` (the PTY exited) or `OUTPUT_DROPPED` (the
  output ring buffer overflowed).
- `ERROR.code` — `BAD_REQUEST`, `NO_SUCH_SESSION`, `INTERNAL`, `BUSY`,
  `UNAUTHORIZED`, `DUPLICATE_NAME`.
- `SERVER_META.server_instance_id` — random ID used to detect server restarts.

The control channel also reports per-session and per-client stats
(`SessionInfo` / `ClientInfo`): name, client count, protocol, addresses,
output-buffer occupancy, backpressure indicators, forward stats, last-seen
times, etc. (`tezzer -list -json` / `-info -json` expose these as JSON.)

## QUIC channel

### Connection and encryption

The QUIC connection is authenticated by mutual proof of knowledge of **K**
(mTLS with pinned, K-derived Ed25519 identities — no PKI). Details in
[security-model.md](security-model.md). Transport security is QUIC's TLS 1.3.

Transport parameters (both sides): idle timeout 60 s, keep-alive 15 s,
`InitialPacketSize` 1200 B (RFC minimum, to survive PMTUD-blackhole paths),
QUIC DATAGRAM support enabled.

Two deployment modes:

- **per-session** (default): each session runs its own QUIC listener with its
  own K on an auto-assigned port.
- **shared** (`tezzerd --udp-port N`): one listener and one K for all sessions;
  connections are routed by the `(session_id, client_id)` pair from the Hello.

### Streams

Per connection:

| Stream | Direction | Opened by | Content |
|---|---|---|---|
| control (first bidi) | both | client | control frames (below) |
| input (uni) | C→S | client | raw PTY input bytes |
| output (uni) | S→C | server | output frames `[offset:u64be][len:u32be][data]`, max 16 MiB |
| forward (further bidis) | both | client | TCP port forwarding (below) |

Control frames are `[u32be length][MessagePack]`, max 1 MiB. Frame types:

| Type | Name | Direction | Purpose |
|---|---|---|---|
| 1 | Hello | C→S | bind connection to `(session_id, client_id)`; carries `last_offset` for resync |
| 2 | Resize | C→S | terminal resize |
| 3 | ServerMeta | S→C | build ID/time, instance ID, feature bits (bit 0 = tcp-forwarding) |
| 4 | SessionGone | S→C | session no longer exists (restart/kill); when the session's process exited, carries its exit code (`ec`, optional; signal death is 128+signal), which the client adopts as its own exit status like ssh |
| 5 | Status | S→C | status message for the user |
| 6 | Log | S→C | log message |
| 7 | FwdOpen | C→S | first frame of a forward stream: `msg` = dial target `"host:port"` |
| 8 | FwdOpenOK | S→C | dial succeeded; stream switches to raw relay |
| 9 | FwdOpenErr | S→C | dial failed / forwarding rejected; `msg` = reason |

Types 7–9 appear only at the head of forward streams, never on the control
stream (they share the frame encoding).

### Output offsets and resync

Output frames carry the **session-logical offset** (monotonic, never reset).
The client tracks the highest delivered offset and drops duplicates. On
(re)connect, the Hello carries `last_offset`; the server replays everything
after it from the session's output ring buffer before switching to live
output. Gaps that have aged out of the ring buffer are reported via a Status
frame (prompting a redraw) rather than stalling the stream.

### Input datagrams (speculative duplication)

Input is carried on the reliable input stream. Small writes (≤ 512 B, i.e.
keystrokes) are *additionally* sent as a QUIC DATAGRAM `[offset:u64be][data]`,
where `offset` is the cumulative byte position on the input stream (per
connection). Whichever copy arrives first is applied; the server deduplicates
by offset. This avoids the retransmission round-trip (head-of-line blocking)
for echo latency when a packet carrying a keystroke is lost. Datagram support
is negotiated by QUIC transport parameters; without it, input is stream-only.

### Recovery

- **Migration** (sleep resume, NAT rebind; connection still alive): the client
  probes a new local socket/path and switches to it. All streams — including
  in-flight forward streams — survive.
- **Reconnect** (connection dead, e.g. sleep longer than the idle timeout): a
  new connection is dialed in parallel to all candidate addresses; the Hello's
  `last_offset` drives output resync. The input-stream offset (and thus the
  datagram dedup state) resets with the new connection. Forward streams do not
  survive, but client-side listeners and tunnel definitions do.

Sleep is detected by wall-clock jumps (the monotonic clock stops during OS
suspend).

### TCP port forwarding

Design: [dev/port-forwarding.md](dev/port-forwarding.md); security notes in
[security-model.md](security-model.md).

For each accepted TCP connection the client opens a new bidi stream and sends
`FwdOpen{msg: "host:port"}`. The server dials the target (timeout 10 s),
replies `FwdOpenOK` or `FwdOpenErr{msg}`, and from then on the stream is a raw
byte relay. Half-close maps stream FIN ↔ TCP `shutdown(SHUT_WR)` in both
directions; abnormal teardown uses stream resets. Backpressure is QUIC's
per-stream flow control — forwards do not head-of-line block the terminal or
each other.

Limits: 64 concurrent forwarded connections per client (excess rejected with
`FwdOpenErr`). Servers started with `--no-tcp-forwarding` clear the feature
bit in ServerMeta *and* reject `FwdOpen`.
