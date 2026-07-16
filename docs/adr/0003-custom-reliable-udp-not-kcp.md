# 0003 — Keep the custom reliable-UDP layer instead of adopting KCP

## Status

Superseded by [0006](0006-adopt-quic.md): the custom reliable-UDP layer this
decision was about has since been replaced by QUIC. The reasoning below about
KCP specifically is kept as a historical record.

## Context

Recurring bugs in the home-grown reliable-UDP layer — sequence-number drift after
sleep, NACKed output not arriving — prompted us to evaluate replacing it with a
KCP-based implementation (e.g. `xtaci/kcp-go`). KCP is an application-layer ARQ
protocol that offers lower latency than TCP with window and retransmission
management.

## Decision

**Keep the custom reliable-UDP layer. Fix the specific problem areas (output ring
buffer eviction policy, SSN state management, NACK filtering) individually.**

## Rationale

- **tezzer's needs sit above what KCP provides.** KCP gives per-connection
  reliability, but tezzer also needs session persistence across sleep/roaming,
  multi-client fan-out of one PTY's output, epoch management for reconnection
  (REATTACH/HELLO), address roaming, and integration with the output ring buffer
  to recover packets the transport no longer holds. All of these would have to be
  rebuilt on top of KCP — and KCP's own sequence space would have to be reconciled
  with tezzer's server sequence numbers (a double-bookkeeping problem).
- **The real problems are not in ARQ latency tuning.** The recurring bugs lived at
  the boundary between the session layer and the transport layer, and in epoch
  transitions (e.g. the buffer evicting too early to answer a NACK, ambiguous
  `latestSSN=0` resetting the send sequence, sequence drift after wake). KCP would
  not address these.
- **Migration cost.** Rewriting the whole protocol to KCP's API is high-risk and
  high-effort relative to fixing the actual defects directly.

## Consequences

We retain ownership (and maintenance) of the reliable-UDP layer, but keep the
freedom to integrate it tightly with session persistence and recovery. The
problem areas are addressed directly and covered by the test suite, including a
deterministic virtual-time simulation of the transport.
