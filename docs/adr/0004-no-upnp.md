# 0004 — No UPnP automatic port mapping (for now)

## Status

Accepted (revisit if warranted).

## Context

Behind a symmetric NAT, STUN + UDP hole punching can fail. UPnP IGD automatic
port mapping could improve the connection success rate.

## Decision

**Do not implement UPnP for now (medium priority).**

## Rationale

- **STUN + hole punching already covers most cases** (full-cone and
  port-restricted NAT fully; symmetric NAT partially via bidirectional sends).
- **UPnP is complex**: SOAP-based IGD, SSDP discovery, router-dependent quirks,
  and lease management.
- **Limited reach**: UPnP is frequently disabled (security policy, corporate
  networks), so the payoff is small relative to the cost.
- **Good alternatives exist**: SSH port forwarding for the control channel
  (the recommended setup), and a fixed server UDP port (`--udp-port`) so only the
  client needs to traverse NAT — which also works for roaming.

## When to revisit

- If symmetric-NAT connection success is measured to be poor in practice.
- If there is sustained user demand.
- If there is real demand to run tezzer where SSH tunneling is unavailable.

## Recommended setup today

Local: Unix domain socket + UID auth. Remote: SSH tunnel + STUN + UDP hole
punching. Server: a fixed UDP port (`--udp-port`). This covers the large majority
of use cases.
