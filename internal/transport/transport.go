// Package transport defines the transport abstraction for terminal sessions.
//
// It captures only the transport-independent contract — "send input, receive
// output, fan out to multiple clients, observe connection liveness" — so that
// the application layers (cmd/tezzer, internal/session) stay decoupled from
// the concrete implementation (QUIC, internal/qtransport).
//
//   - Retransmission, ordering, migration, reconnection, hole punching, and
//     similar mechanisms are hidden inside the implementation.
//   - The output ring buffer keeps its transport-independent role as the
//     resynchronization source after reconnects; resync is wired through
//     OnResyncNeeded (offset-based).
package transport

import (
	"context"
	"net"
)

// ConnectionState is the connection state at a transport-independent
// granularity.
type ConnectionState int

const (
	StateConnecting ConnectionState = iota
	StateConnected
	StateRecovering // recovering from roaming or a transient disconnect
	StateClosed
)

func (s ConnectionState) String() string {
	switch s {
	case StateConnecting:
		return "Connecting"
	case StateConnected:
		return "Connected"
	case StateRecovering:
		return "Recovering"
	case StateClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

// Stats is a transport-independent snapshot used for the Ctrl-^ s display and
// similar. Implementation-specific details (congestion window, receive-buffer
// fill, ...) are intentionally excluded.
type Stats struct {
	RTT            float64 // recent RTT (ms; QUIC SmoothedRTT)
	LossRate       float64 // estimated loss rate (0..1, PacketsLost/PacketsSent, cumulative since connect)
	LastRecoveryMs float64 // duration of the most recent recovery (migration/reconnect), in ms
	RecoveryCount  uint64  // cumulative number of roaming/reconnect recoveries
	BytesSent      uint64
	BytesReceived  uint64
}

// ClientID uniquely identifies a client connection on a shared transport.
// Including Session means clients of different sessions cannot collide even
// when they pick the same Num (a client-chosen uint16), so input routing and
// output fan-out never cross wires.
type ClientID struct {
	Session string // owning session ID (a single value in per-session mode)
	Num     uint16 // client-chosen identifier; used for routing, carries no authorization
}

// Resize is a terminal size change.
type Resize struct {
	Client ClientID
	Rows   int
	Cols   int
}

// Input is client input as received on the server side.
type Input struct {
	Client ClientID
	Data   []byte
}

// ClientTransport is the client-side abstraction of a terminal transport.
type ClientTransport interface {
	// Start establishes the connection (quic: dial). It returns once the
	// connection is established or has failed.
	Start(ctx context.Context) error
	Close() error

	SendInput(data []byte) error
	SendResize(cols, rows int) error // (cols, rows) order matches the existing implementations
	// Output delivers server output (ordered). Offset is the session's
	// logical offset; callers use it for cross-path deduplication against
	// the Unix-socket path (OutputMsg.Seq, the same numbering space).
	Output() <-chan OutputChunk

	State() ConnectionState
	OnStateChange(fn func(old, new ConnectionState, msg string))
	OnStatusMessage(fn func(msg string))
	OnLogMessage(fn func(msg string))
	OnServerMeta(fn func(buildID, buildTime string, instanceID []byte))
	// OnSessionNotFound registers a callback for session-gone notifications.
	// exitCode is the exit status of the session process (-1 = unknown; not
	// set for kills and similar).
	OnSessionNotFound(fn func(msg string, exitCode int))

	Stats() Stats
	// SetRoamingCandidates provides reachable address candidates obtained via
	// STUN and similar (fallback addresses; migration hints).
	SetRoamingCandidates(addrs []string)
	// SetCandidatesRefresher registers a function called when dialing every
	// candidate has failed. It should regenerate the candidate list (e.g. by
	// re-querying STUN). Passing nil disables it.
	SetCandidatesRefresher(fn func() []string)
}

// ServerTransport is the server-side abstraction (including the fan-out from
// one PTY to multiple clients).
type ServerTransport interface {
	Start(ctx context.Context) error
	Close() error

	// SendOutput fans out one output chunk to the given clients.
	// offset is the session's logical offset (Session.seq; monotonically
	// increasing, never reset). The transport carries it and it anchors
	// resynchronization after reconnects. Implementations map it onto their
	// own sequencing space (quic: stream offset).
	SendOutput(offset uint64, data []byte, clients []ClientID) error
	Input() <-chan Input
	Resize() <-chan Resize

	OnClientConnect(fn func(client ClientID))
	OnClientDisconnect(fn func(client ClientID))

	// OnResyncNeeded is called when a client requests output from fromOffset
	// onwards (after a reconnect or a long sleep, to fill in what it is
	// missing locally). The session layer returns the retained chunks from
	// that offset and the transport re-sends them to the client. This keeps
	// the output ring buffer owned by the session layer.
	// The callback may return only a leading batch of the requested range
	// (bounded batches avoid decompressing the whole cold retention tier into
	// memory at once). The transport must call it repeatedly, with fromOffset
	// set to the last received offset + 1, until an empty slice is returned.
	// Returned chunks must be in ascending offset order and >= fromOffset
	// (this guarantees the call loop makes progress).
	OnResyncNeeded(fn func(client ClientID, fromOffset uint64) ([]OutputChunk, error))

	ActiveClients() []ClientID

	// ClientSendStats reports per-client output-send health (backpressure
	// observability). A blocked write to a slow client can stall the whole
	// PTY reader, so SlowWrites/MaxWriteMs identify which client is stalling
	// the session.
	ClientSendStats() []ClientSendStat

	// SetServerMeta sets the server information announced to clients on
	// connect (build ID/time, instance ID). It is delivered over the QUIC
	// path, independent of the Unix socket.
	SetServerMeta(buildID, buildTime string, instanceID []byte)

	// SendSessionGone notifies the given client that the session is gone
	// (restart/kill detection). Deliverable over the QUIC path even when the
	// Unix socket is dead.
	// exitCode is the session process's exit status (-1 = unknown; it is
	// >= 0 only when the close was caused by process exit, and then
	// propagates to the client's own exit status).
	SendSessionGone(client ClientID, reason string, exitCode int) error

	// SendStatus sends a status message to the given client (QUIC path).
	// Example: output gaps that resync cannot fill after a reconnect
	// (prompting a redraw).
	SendStatus(client ClientID, msg string) error

	Stats() Stats
}

// ClientSendStat is per-client output-send health (backpressure
// observability).
type ClientSendStat struct {
	Client         ClientID
	BytesSent      uint64
	LastSendUnix   int64  // last successful send time (Unix seconds; 0 = none)
	SlowWrites     uint64 // count of output writes exceeding the slow-write threshold
	MaxWriteMs     uint64 // largest observed write duration (ms)
	StallEpisodes  uint64 // cumulative episodes of writes blocked past the warning threshold
	CurrentStallMs uint64 // elapsed time of an in-progress stall (ms; 0 = not stalled)
	QUICRemoteAddr string // current remote address of the QUIC connection (may change with migration; empty = unknown)
	// Per-client TCP port-forwarding (-L) statistics.
	ForwardsActive         int32  // currently active forwarded connections
	ForwardsOpened         uint64 // cumulative forwarded connections (successful dials)
	ForwardBytesToTarget   uint64 // cumulative bytes, client -> target
	ForwardBytesFromTarget uint64 // cumulative bytes, target -> client
}

// OutputChunk is a piece of output with its logical offset, as returned by the
// session layer during resynchronization.
type OutputChunk struct {
	Offset uint64
	Data   []byte
}

// ForwardConn is one forwarded connection of a TCP port forward (a QUIC
// bidirectional stream). CloseWrite closes only the send direction (FIN),
// mirroring TCP half-close. Close closes both directions.
type ForwardConn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	CloseWrite() error
	Close() error
}

// TCPForwarder is an optional ClientTransport capability for opening -L port
// forwards. Only supporting implementations (quic) satisfy it; callers detect
// it with a type assertion.
type TCPForwarder interface {
	// ForwardingSupported reports whether the server advertises forwarding
	// support (known once serverMeta has been received; false until then).
	ForwardingSupported() bool
	// OpenForward opens a forwarding channel that makes the server dial a
	// TCP connection to target ("host:port"). ctx applies only to the open
	// handshake (stream open + dial response), not to the relay afterwards.
	OpenForward(ctx context.Context, target string) (ForwardConn, error)
}

// AgentForwarder is an optional ServerTransport capability for opening SSH
// agent forwarding (-A) relay channels. Only supporting implementations
// (quic) satisfy it; callers detect it with a type assertion.
// The direction is the reverse of -L's OpenForward: the server opens a
// bidirectional stream towards the session's current agent provider client.
type AgentForwarder interface {
	// OpenAgentStream opens a relay channel to the session's current agent
	// provider. It returns an error when no provider is set (no -A client
	// attached). ctx applies only to the open handshake.
	OpenAgentStream(ctx context.Context, sessionID string) (ForwardConn, error)
}

// ---- Named optional interfaces ----
// These are not part of the core ServerTransport / ClientTransport contracts;
// only supporting implementations (quic) satisfy them, and callers detect them
// with type assertions (the same pattern as TCPForwarder / AgentForwarder).
// They are collected and named here so the full set of optional contracts
// stays visible in one place instead of anonymous assertions scattered across
// call sites.

// ForwardingPolicy is an optional ServerTransport capability for allowing or
// denying forwarding features (the target of the server flags
// --no-tcp-forwarding / --no-agent-forwarding).
type ForwardingPolicy interface {
	// SetTCPForwarding allows or denies TCP port forwarding (-L).
	// Default: allowed.
	SetTCPForwarding(enabled bool)
	// SetAgentForwarding allows or denies SSH agent forwarding (-A).
	// Default: allowed.
	SetAgentForwarding(enabled bool)
}

// SocketHandover is an optional ServerTransport capability required by the
// zero-downtime restart (self re-exec): fd inheritance and explicit
// disconnection.
type SocketHandover interface {
	// DupUDPSocketFd returns a duplicate fd of the listening UDP socket
	// without CLOEXEC (so it is inherited across exec).
	DupUDPSocketFd() (int, error)
	// DisconnectAllClients immediately disconnects every client with
	// CONNECTION_CLOSE (avoiding the client-side idle-timeout wait and
	// triggering an immediate reconnect).
	DisconnectAllClients(reason string)
}

// Addresser is an optional ServerTransport capability exposing the actual
// listening address (to resolve the port after binding to :0, and to tell
// clients during bootstrap).
type Addresser interface {
	Addr() net.Addr
}

// ResyncSeeder is an optional ClientTransport capability for seeding, before
// Start, the output offset already received and rendered via another path
// (the Unix socket). It is reflected in the Hello's LastOffset and prevents
// the backlog from being transferred twice on attach.
type ResyncSeeder interface {
	// SeedResyncOffset must be called at most once, before Start().
	SeedResyncOffset(offset uint64)
}

// AgentForwardClient is the client-side optional capability for SSH agent
// forwarding (-A).
type AgentForwardClient interface {
	// AgentForwardingSupported reports whether the server advertises -A
	// support (known once serverMeta has been received; false until then).
	AgentForwardingSupported() bool
	// SetAgentSockPath sets the local agent socket path to relay from.
	// Call at most once, before Start(). An empty string disables -A.
	SetAgentSockPath(path string)
}
