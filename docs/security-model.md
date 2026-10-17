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

## Remote port forwarding (`-R`)

`tezzer -R [bind:]port:host:hostport` makes tezzerd listen on the server's
loopback and relay each connection to the client, which dials the target from
its own machine.

- **Same exposure as `ssh -R`.** The server-side listener binds only to
  loopback (127.0.0.1 and ::1), but **any local user on the server host can
  connect to it**. For a Chrome DevTools Protocol port that means full control
  of the browser profile on the client machine (cookies, logged-in sessions).
  Use `-R` only on hosts where you trust the other local users, as you would
  with `ssh -R`. It is disabled together with `-L` by
  `tezzerd --no-tcp-forwarding`.
- **The server cannot choose the client's dial target.** The server only tells
  the client which of its ports received a connection; the client dials the
  target from its own `-R` definition and refuses ports it did not define.
- **Device ID is not authentication.** Clients send a device ID so the server
  can group clients of the same machine and refuse the same port from another
  machine. Claiming another machine's ID requires K, and anyone holding K can
  already run anything in the session, so spoofing adds no capability. The
  full ID is never displayed (logs and `-list` show its first 8 characters).
- **Device ID and privacy.** The ID is a hash of a random secret kept in
  `~/.tezzer/device-id` (mode 0600) and the hostname; neither the secret nor
  the hostname leaves the machine, and nothing hardware-derived is used.
  It is sent only to servers you use `-R` with, inside the encrypted QUIC
  handshake; the server keeps it in memory only (not on disk, not across
  restarts) and does not accept IDs longer than 64 bytes. Because it is
  stable, administrators of two servers you use `-R` with could tell that the
  connections came from the same machine — but those servers already see your
  IP address and SSH public key, which link you far more strongly. Delete the
  file to get a new ID.
- **Shared or cloned home directories.** Mixing in the hostname keeps machines
  that share a home directory (NFS, roaming profiles, dotfile sync, a restore
  onto another machine) from being treated as one device, which would relay
  connections to the wrong machine. A clone that also keeps the hostname (a
  whole-VM copy) still shares the ID; delete `~/.tezzer/device-id` on the
  clone.
- **Limits and audit.** At most 16 `-R` per client and 64 concurrent relayed
  connections per port; the server logs each accepted connection and the
  client it is relayed to.

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
