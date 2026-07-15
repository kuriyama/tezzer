# tezzer vs mosh

Both tezzer and [mosh](https://mosh.org/) solve the same headline problem:
a terminal connection that survives roaming, sleep, and flaky networks, over
encrypted UDP, bootstrapped through SSH. The difference is *how* — and that
one architectural choice explains almost every practical difference between
them.

Facts about mosh below are as of mosh 1.4.0 (October 2022, the latest release
at the time of writing).

## The structural difference

**mosh is a terminal emulator that synchronizes screen state.** The server
interprets PTY output into an in-memory screen, and the State Synchronization
Protocol (SSP) transmits the *current visible screen* to the client. Output is
not forwarded; it is re-rendered.

**tezzer is a byte pipe.** PTY output is forwarded verbatim to your local
terminal, exactly as raw SSH would deliver it. tezzer never interprets escape
sequences (see [design philosophy](design-philosophy.md)).

Neither approach is strictly better — they trade different things away.

## What verbatim forwarding buys you (tezzer)

- **Native scrollback.** mosh synchronizes only the visible screen, so your
  terminal's scrollback stays empty; the usual advice is to run tmux/screen
  remotely just to scroll. tezzer delivers the full output stream, so local
  scrollback works as over SSH.
- **Every escape sequence your terminal supports.** Images (sixel, kitty
  graphics, iTerm2 inline images), OSC 8 hyperlinks, OSC 52 clipboard in both
  directions, tmux passthrough — if it works over raw SSH, it works over
  tezzer. mosh's emulator drops anything it does not model (as of 1.4 that
  includes images and hyperlinks; OSC 52 *copy* arrived in 1.4.0).
- **New terminal features work on day one.** With mosh, a new capability must
  be implemented in mosh's emulator and shipped in a release — true color, for
  example, landed in 2022 after years of requests. tezzer has nothing to
  update: the bytes already pass through.
- **Notification sequences reach your terminal.** Long-running tools that
  signal for attention — AI coding agents waiting for approval are the current
  example — can ring the bell or emit desktop-notification sequences (OSC 9,
  OSC 777, OSC 99). We measured what survives each transport: tezzer delivers
  all of them byte-exact; mosh 1.4.0 forwards the bell but its emulator drops
  every OSC notification form; a remote tmux (3.6b) likewise passes only the
  bell unless you configure `allow-passthrough on` *and* the sender wraps its
  sequences in a DCS passthrough envelope — cooperation most tools do not
  provide. To be precise about what was measured: byte arrival at the client's
  stdout; turning OSC 9 into a popup is then your terminal's job (WezTerm,
  kitty, iTerm2, and others do).
- **One less opinion about character width.** Rendering CJK, East Asian
  Ambiguous, and emoji correctly requires the application and the terminal to
  agree on character widths — already fragile in modern terminals. mosh adds a
  third participant: its emulator keeps its own width tables, which can
  disagree with both ends (and lag behind new Unicode versions). tezzer does
  not solve width mismatches, but it removes that third opinion — the problem
  is back to the same two parties as over raw SSH.
- **Detach, reattach, share.** A tezzer session lives on the server; you can
  detach, reattach from a different machine, or attach multiple clients to the
  same session. A mosh session is bound to the client that started it — if you
  lose that client, you cannot pick the session up elsewhere.
- **Transport observability.** tezzer exposes RTT, packet loss, reordering,
  and per-client backpressure (`Ctrl-^ i`, `tezzer -info`, `-stats -json`),
  because on a bad link you want to know *why* the terminal feels slow.

## What screen synchronization buys you (mosh)

Honesty requires the other column:

- **Predictive local echo.** mosh's client predicts the effect of your
  keystrokes and shows them before the server confirms — on a 300 ms satellite
  link, typing still feels instant. tezzer deliberately does not do this (it
  would require the VT emulation it avoids); your echo latency is the network
  RTT. tezzer narrows the gap on lossy links (speculative input delivery over
  QUIC datagrams), but it cannot beat physics the way prediction can.
- **Constant-size updates on massive output.** If you `cat` a huge file over a
  slow link, mosh transmits only the final screen — intermediate output is
  simply skipped. tezzer, like SSH, delivers every byte.
- **Maturity and ubiquity.** mosh has been in production since 2012 and is
  packaged everywhere. tezzer is pre-1.0 and its protocol still changes.

## Quick reference

| | tezzer | mosh 1.4 |
|---|---|---|
| Architecture | verbatim byte transport | VT emulation + screen state sync |
| Native scrollback | yes | no (visible screen only) |
| Images (sixel / kitty / iTerm2) | pass-through | no |
| OSC 8 hyperlinks | pass-through | no |
| OSC 52 clipboard | pass-through (both directions) | copy since 1.4.0 |
| Terminal bell (BEL) | pass-through | forwarded |
| Desktop notifications (OSC 9 / 777 / 99) | pass-through | no (dropped by emulator) |
| True color | pass-through | since 1.4.0 |
| Character width (ambiguous / emoji) | app + terminal only, as over SSH | third width table in mosh's emulator |
| Future terminal features | pass-through | need mosh support |
| Predictive local echo | no (by design) | yes |
| Huge output on slow links | full stream (like SSH) | skips to latest screen |
| Detach / reattach | yes, from any client | no (session bound to client) |
| Multiple clients per session | yes | no |
| Transport stats (RTT, loss, …) | yes (`-info`, `-stats -json`) | lag indicator only |
| Transport | QUIC (UDP), AES-256-GCM, mTLS pinned via SSH | SSP (UDP), AES-128-OCB, key via SSH |
| Server ports | one UDP port per server (or per session), NAT traversal via STUN | UDP 60000–61000 range by default |
| Maturity | pre-1.0 | stable since 2012 |

## Which should you use?

- Your pain is **"mosh broke my scrollback / my images / my clipboard /
  my new terminal's features"** → tezzer is built exactly for you.
- Your pain is **typing latency on a 200 ms+ link** → mosh's predictive echo
  is the better tool, and tezzer will not replicate it.
- You want **sessions you can pick up from any machine** without remote
  tmux → tezzer (see the
  [workflow in the README](../README.md#do-you-still-need-a-remote-multiplexer)).
- You leave **long-running AI agents or builds** on remote hosts and want
  their attention signals (bell, OSC 9 notifications) to reach your local
  terminal → tezzer forwards them verbatim (see the
  [README recipe](../README.md#running-ai-agents-and-other-long-running-tools)).
