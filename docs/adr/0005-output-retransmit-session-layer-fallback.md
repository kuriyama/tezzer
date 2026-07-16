# 0005 — Recover aged-out output from the session-layer ring buffer

## Status

Superseded by [0006](0006-adopt-quic.md): the UDP send buffer and NACK-based
recovery this decision was about no longer exist under QUIC. The ring-buffer
recovery idea survives in a different form — see 0006's Consequences.

## Context

The UDP retransmission send buffer holds *encrypted* packets, so for memory
reasons it is bounded in size and age. After a long disconnect (e.g. sleep), a
NACK can arrive for output that has already aged out of the send buffer. Those
packets need to be recovered from the longer-lived session-layer output ring
buffer.

The complication: the UDP layer's server sequence numbers (SSN) and the session
layer's output sequence are independent spaces, and the mapping between them was
not recorded.

## Options considered

1. **An SSN mapping table on the session** (clientID → SSN → output seq).
   Reuses the ring buffer but is complex per-client and increases memory use.
2. **Extend the send buffer's max age** (e.g. to 8 hours). No code change, but
   retaining encrypted packets that long can reach gigabytes of memory.
3. **Attach SSNs to each output chunk** (`OutputChunk.ClientSSNs`), recorded at
   send time. Clear mapping, but awkward when large output is split into chunks.
4. **Query the session layer on NACK**: when the send buffer misses, the UDP
   manager calls back into the session, which returns the raw data to be
   re-encrypted and resent. Minimal structural impact and memory-efficient.

## Decision

**Adopt option 4**, using option 3's `OutputChunk.ClientSSNs` record for the
SSN→data mapping.

## Rationale

- Memory-efficient: the ring buffer holds *unencrypted* data with a size cap, so
  no long-term retention of encrypted packets.
- Reuses the existing output ring buffer.
- Avoids large changes to the send-buffer design.
- As long as the data is still in the ring buffer, recovery works regardless of
  how long the client was asleep.

The trade-off is the cost of re-encryption on recovery and an added dependency
from the session layer to the UDP layer.
