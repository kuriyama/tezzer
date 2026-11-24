package qtransport

// types.go: transport の利用側（cmd/tezzer・internal/session）と共有する型。

import "errors"

// ConnectionState is the client's connection state.
type ConnectionState int

const (
	StateConnecting ConnectionState = iota
	StateConnected
	StateRecovering // recovering from roaming or a transient disconnect
	StateClosed
)

// String returns the state name.
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

// Stats is a snapshot of client connection statistics, used for the Ctrl-^ s
// display and similar.
type Stats struct {
	RTT            float64 // recent RTT (ms; QUIC SmoothedRTT)
	LossRate       float64 // estimated loss rate (0..1, PacketsLost/PacketsSent, cumulative since connect)
	LastRecoveryMs float64 // duration of the most recent recovery (migration/reconnect), in ms
	RecoveryCount  uint64  // cumulative number of roaming/reconnect recoveries
	BytesSent      uint64
	BytesReceived  uint64
}

// ClientID uniquely identifies a client on a shared transport; it stays the
// same across reconnects.
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

// OutputChunk is a piece of session output with its logical offset.
// Client.Output delivers it, and the OnResyncNeeded callback returns it.
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

// ErrForwardRejected is wrapped by forwarding open errors when the peer
// explicitly refused the request (for example, its local dial failed), as
// opposed to the stream itself failing to open. Callers use errors.Is to
// decide whether retrying through another client is pointless.
var ErrForwardRejected = errors.New("forward rejected by peer")
