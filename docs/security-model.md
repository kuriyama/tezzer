# Security Model

tezzer operates at the same level of abstraction as raw SSH: it moves terminal
input/output between a local terminal and a remote PTY. Its security model is
correspondingly thin and is described here so operators understand exactly what
is and isn't protected.

## Trust boundary: the bootstrap channel

The root of trust is the **bootstrap channel** — SSH plus a Unix domain socket
on the server. The client obtains a session's key over this channel before any
UDP/QUIC traffic flows.

Consequently, anyone who can reach the bootstrap channel (i.e. has the SSH/UDS
access the user already has) is trusted. tezzer does **not** add an
authentication layer beneath SSH; if SSH/UDS is compromised, so is tezzer.

## K — the shared key is a bearer capability

Each session (per-session mode) or each shared transport (fixed-port mode) has a
random 32-byte key **K**, delivered to the client over the bootstrap channel.

The QUIC connection is authenticated by **mutual proof of knowledge of K**, with
no PKI or certificate authority:

- K is expanded with HKDF-SHA256 (with a per-role label) into an Ed25519
  identity.
- Each side presents a self-signed certificate for its derived identity.
- mTLS pins the peer's public key to the key derived from K (custom
  `VerifyPeerCertificate`, `InsecureSkipVerify` for chain building only).

K is therefore a **bearer capability**: holding K grants full control of the
corresponding session(s). There is no per-user authorization below K.

- **Per-session mode**: K scopes to a single session.
- **Shared (fixed-port) mode**: one K covers every session on that transport.
  The `SessionID` in the handshake only *routes*; it does not *authorize*.
  Anyone holding that K can attach to any session on the transport by SessionID.

This matches the intended deployment: one `tezzerd` per user (e.g. a
`systemctl --user` service), so K is effectively a per-user capability.
Isolating multiple distinct users behind a single shared transport would require
per-session keys, which is not currently implemented.

## SessionID — a name, not a capability

`SessionID` identifies and routes a session. **Knowing a SessionID does not grant
access** — access is granted by K, which is obtained over the bootstrap channel.
Do not treat a SessionID as a secret or as a capability.

## ClientID — a routing hint, not auth

A client picks a 16-bit `ClientID`; on the shared transport it is combined with
the SessionID into a composite identity for input routing and output fan-out. It
is a **local routing identifier only** — not authentication, not authorization.
The 16-bit space is adequate for the intended scale (a few users, a few sessions
each, a few clients per session).

## PTY surface — not a sandbox

An authenticated client has full control of the PTY, exactly like an interactive
shell. tezzer is **not** a shell sandbox or a privilege boundary; it is a
transport for a terminal you already have the right to use.

## TCP port forwarding (`-L`)

`tezzer -L [bind:]port:host:hostport` forwards TCP connections over the QUIC
connection; the server dials the target on behalf of the client.

- **No new privilege.** A K holder already has the PTY (= arbitrary code
  execution); dialing from the server host is equivalent to running `curl` in
  that shell. Forwarding is therefore enabled by default (SSH parity), and can
  be disabled server-wide with `tezzerd --no-tcp-forwarding`.
- **Loopback-only binds.** The client listener binds only to loopback; there is
  intentionally no `GatewayPorts` equivalent.
- **Per-connection scope.** Forwards belong to the QUIC connection (the
  individual client), not the session. Clients attached to the same session
  cannot see or use each other's forwards — but this is a UX property, not a
  security boundary: anyone attached to the session holds K and controls the
  PTY. The boundary remains K.
- **Limits and audit.** At most 64 concurrent forwarded connections per client
  (excess is rejected); dial targets are always logged server-side.

## The QUIC listen port — pre-auth traffic exists

The UDP/QUIC port accepts unauthenticated packets before the handshake completes
(QUIC Initial packets, etc.). This is a **denial-of-service surface** (e.g.
Initial floods consuming CPU/memory), **not an authentication-bypass surface** —
without K, no session can be reached.

For small-scale and personal deployments this is acceptable. If needed, mitigate
operationally: firewall the port to known sources, rate-limit, or use a fixed
port behind a router/port-forward you control.

## Dependencies

Authentication and transport security rely on
[quic-go](https://github.com/quic-go/quic-go) (TLS 1.3, QUIC) and the Go standard
crypto libraries (HKDF, Ed25519, x509). A vulnerability in these would undermine
tezzer's guarantees, so keep dependencies current with security fixes.
