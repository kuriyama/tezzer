# Architecture Decision Records

This directory records the significant design decisions behind tezzer — the
"why," including options that were considered and rejected. The overarching
philosophy is in [../design-philosophy.md](../design-philosophy.md).

| # | Decision |
|---|---|
| [0001](0001-no-vt-emulation.md) | No virtual-terminal emulation; relay PTY output verbatim |
| [0002](0002-no-disk-persistence.md) | No disk persistence; bounded in-memory buffers only |
| [0003](0003-custom-reliable-udp-not-kcp.md) | Keep the custom reliable-UDP layer instead of adopting KCP |
| [0004](0004-no-upnp.md) | No UPnP automatic port mapping (for now) |
| [0005](0005-output-retransmit-session-layer-fallback.md) | Recover aged-out output from the session-layer ring buffer |
