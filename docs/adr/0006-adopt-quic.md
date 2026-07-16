# 0006 — Replace the custom reliable-UDP layer with QUIC (quic-go)

## Status

Accepted. Supersedes [0003](0003-custom-reliable-udp-not-kcp.md) and
[0005](0005-output-retransmit-session-layer-fallback.md).

## Context

[0003](0003-custom-reliable-udp-not-kcp.md) chose to keep the home-grown
reliable-UDP layer (sequence numbers, NACK-based retransmission, a send buffer
for recovery) rather than rebuild it on top of KCP, on the grounds that the
recurring bugs lived at the session/transport boundary rather than in ARQ
latency tuning, and that KCP's own sequence space would just add a second
bookkeeping problem on top of tezzer's.

That reasoning about KCP specifically still holds, but a review of the
transport concluded that a hand-rolled ARQ/reordering/roaming implementation
was itself the wrong level to keep maintaining — not because of any one bug,
but because every one of these responsibilities (retransmission, ordering,
congestion control, connection migration across address changes, TLS) is
exactly what QUIC provides as a standard, independently-maintained protocol.

## Decision

**Replace the custom reliable-UDP layer with QUIC, via
[quic-go](https://github.com/quic-go/quic-go).**

Terminal I/O and TCP port forwarding now ride QUIC streams and datagrams;
QUIC's built-in TLS 1.3 handshake, ARQ, congestion control, and connection
migration replace the corresponding hand-rolled logic. Session persistence,
multi-client fan-out, and the output ring buffer (recovering output the
transport no longer holds) remain session-layer concerns, described in
[protocol.md](../protocol.md) and [architecture.md](../architecture.md).

## Rationale

- **The maintained-by-us surface shrinks to what's actually tezzer-specific.**
  Sequence-number bookkeeping, NACK filtering, and send-buffer eviction — the
  source of the recurring bugs in 0003's context — go away entirely; they were
  never the part of the system unique to tezzer.
- **Address roaming and sleep/resume are QUIC's connection migration**, not a
  custom epoch/REATTACH negotiation. Below QUIC's idle timeout, an address
  change is transparent to every open stream.
- **Session identity is a separate axis from transport identity.** The shared
  key **K** now authenticates the QUIC connection directly (mutual TLS pinned
  to a K-derived identity — see [security-model.md](../security-model.md)),
  so there is no reliance on a home-grown handshake for trust.
- **quic-go is an independently-maintained, widely-used implementation.** It
  carries its own security-update cadence, which tezzer now depends on instead
  of owning that surface itself (this dependency, and its risk, is called out
  in [security-model.md](../security-model.md)).

## Consequences

- 0005's specific recovery mechanism (querying the session layer on NACK) no
  longer applies: QUIC handles retransmission below the session layer. What
  remains from that decision is recovering output that has aged out of the
  ring buffer entirely, which is now driven by the client's last-received
  offset on (re)connect (see "Output offsets and resync" in
  [protocol.md](../protocol.md)).
- tezzer now depends on quic-go for its security and reliability guarantees;
  a defect there is a defect in tezzer.
- The old `internal/udp` package, and the UDP-specific docs describing
  sequence numbers, NACK, and the send buffer, have been removed.
