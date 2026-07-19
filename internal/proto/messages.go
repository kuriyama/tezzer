package proto

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// Common fields
type BaseMsg struct {
	Type string `msgpack:"type"`
	V    int    `msgpack:"v"`
	TS   int64  `msgpack:"ts,omitempty"`
}

// Client -> Server messages

type HelloMsg struct {
	Type       string `msgpack:"type"`
	V          int    `msgpack:"v"`
	ClientName string `msgpack:"client_name"`
	Cols       int    `msgpack:"cols"`
	Rows       int    `msgpack:"rows"`
}

type CreateSessionMsg struct {
	Type string            `msgpack:"type"`
	Name string            `msgpack:"name,omitempty"` // session name (-name); unique among active sessions
	Cmd  string            `msgpack:"cmd"`
	Args []string          `msgpack:"args,omitempty"`
	Env  map[string]string `msgpack:"env,omitempty"`
	Cwd  string            `msgpack:"cwd,omitempty"`
	Cols int               `msgpack:"cols"`
	Rows int               `msgpack:"rows"`
	// AgentForward requests SSH agent forwarding (-A). It only has meaning at
	// creation time and is immutable for the session's lifetime
	// (docs/dev/agent-forwarding.md).
	AgentForward bool `msgpack:"agent_forward,omitempty"`
}

type AttachSessionMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	FromSeq   uint64 `msgpack:"from_seq"`
	Cols      int    `msgpack:"cols"`
	Rows      int    `msgpack:"rows"`
	ClientID  uint16 `msgpack:"client_id,omitempty"` // client identifier for the QUIC path
}

type InputMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	Data      []byte `msgpack:"data"`
}

type ResizeMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	Cols      int    `msgpack:"cols"`
	Rows      int    `msgpack:"rows"`
}

type PingMsg struct {
	Type  string `msgpack:"type"`
	Nonce uint64 `msgpack:"nonce"`
}

type ListSessionsMsg struct {
	Type string `msgpack:"type"`
}

type GetSessionInfoMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

type KillSessionMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

// WaitSessionMsg is WAIT_SESSION: wait for the session's command to exit
// (-wait). The server withholds its response until the session ends, then
// replies with a NOTE (SESSION_CLOSED, with exit_code).
type WaitSessionMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

// UDPClientInfoMsg announces the client's ClientID to the server (registering
// it as an output fan-out target; required for routing in shared-transport
// mode). The wire type name keeps its legacy "UDP" prefix for compatibility.
type UDPClientInfoMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	ClientID  uint16 `msgpack:"client_id"` // client identifier for the QUIC path
}

// GetServerMetaMsg requests the server's metadata.
type GetServerMetaMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id,omitempty"` // set to request session-specific information
}

// Server -> Client messages

type WelcomeMsg struct {
	Type       string `msgpack:"type"`
	ServerName string `msgpack:"server_name"`
	SessionID  string `msgpack:"session_id,omitempty"`
}

type SessionCreatedMsg struct {
	Type       string `msgpack:"type"`
	SessionID  string `msgpack:"session_id"`
	UDPEnabled bool   `msgpack:"udp_enabled,omitempty"` // whether the QUIC (UDP) transport is enabled
	UDPPort    int    `msgpack:"udp_port,omitempty"`    // server-side UDP port
	UDPKey     []byte `msgpack:"udp_key,omitempty"`     // shared key (32 bytes, used for mTLS pinning)
	PTYClosed  bool   `msgpack:"pty_closed,omitempty"`  // whether the PTY has already ended
	// InitialOutput is the buffered output for a PTY that exited immediately
	// (non-interactive commands such as env).
	InitialOutput []byte `msgpack:"initial_output,omitempty"`
	// STUNAddrs are the server's public address candidates discovered via
	// STUN, one per address family (at most 2: IPv4/IPv6; a family that is
	// unavailable is omitted). Example:
	// ["203.0.113.1:54321", "[2001:db8::1]:54321"].
	STUNAddrs []string `msgpack:"stun_addrs,omitempty"`
	LocalAddr string   `msgpack:"local_addr,omitempty"` // server's local address for same-LAN connects (e.g. "192.168.1.10")
	// STUNServer is the STUN server the server itself queried (the value of
	// tezzerd --stun-server). The client uses the same server for its own
	// STUN queries, so a self-hosted STUN server applies to both ends.
	// Empty from old servers = client falls back to its default.
	STUNServer string `msgpack:"stun_server,omitempty"`
}

type ErrorMsg struct {
	Type    string `msgpack:"type"`
	Code    string `msgpack:"code"`
	Message string `msgpack:"message"`
}

type PongMsg struct {
	Type  string `msgpack:"type"`
	Nonce uint64 `msgpack:"nonce"`
}

// OutputMsg carries the raw byte stream of PTY output.
type OutputMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	Seq       uint64 `msgpack:"seq"`  // sequence number (gap detection)
	Data      []byte `msgpack:"data"` // raw bytes read from the PTY
}

type NoteMsg struct {
	Type string `msgpack:"type"`
	Kind string `msgpack:"kind"`
	Msg  string `msgpack:"msg,omitempty"` // additional message (used by OUTPUT_DROPPED and others)
	// ExitCode is the session process's exit status. Only set on
	// SESSION_CLOSED; nil = unknown (notifications from old servers omit it).
	ExitCode *int `msgpack:"exit_code,omitempty"`
}

// ClientInfo describes one connected client.
type ClientInfo struct {
	ID             string   `msgpack:"id"`
	Protocol       string   `msgpack:"protocol"`                   // "UDS", "TCP", "UDP"
	RemoteAddr     string   `msgpack:"remote_addr,omitempty"`      // remote address (TCP/UDP)
	QUICRemoteAddr string   `msgpack:"quic_remote_addr,omitempty"` // current QUIC remote address (may change with migration)
	UDPClientID    uint16   `msgpack:"udp_client_id,omitempty"`    // QUIC client identifier (legacy field name)
	UDPAddresses   []string `msgpack:"udp_addresses,omitempty"`    // list of UDP addresses (IP:port; legacy, unset by current servers)
	// Send statistics (QUIC connections only).
	SendBufferBytes int   `msgpack:"send_buffer_bytes,omitempty"` // bytes sent
	LastSeen        int64 `msgpack:"last_seen,omitempty"`         // last send/receive time (Unix seconds)
	// Backpressure observability (QUIC): slow output writes indicate a slow
	// client stalling the PTY reader.
	SlowOutputWrites uint64 `msgpack:"slow_output_writes,omitempty"`  // output writes exceeding the slow-write threshold
	MaxOutputWriteMs uint64 `msgpack:"max_output_write_ms,omitempty"` // largest observed write duration (ms)
	// Stall observability (QUIC): output writes blocked past the warning
	// threshold (including one in progress).
	OutputStallEpisodes uint64 `msgpack:"output_stall_episodes,omitempty"` // cumulative episodes
	OutputStallMs       uint64 `msgpack:"output_stall_ms,omitempty"`       // elapsed ms of an in-progress stall (0 = none)
	// TCP port-forwarding (-L) statistics.
	ForwardsActive         int    `msgpack:"forwards_active,omitempty"`           // currently active forwarded connections
	ForwardsOpened         uint64 `msgpack:"forwards_opened,omitempty"`           // cumulative forwarded connections
	ForwardBytesToTarget   uint64 `msgpack:"forward_bytes_to_target,omitempty"`   // cumulative bytes, client -> target
	ForwardBytesFromTarget uint64 `msgpack:"forward_bytes_from_target,omitempty"` // cumulative bytes, target -> client
}

type SessionInfo struct {
	SessionID   string       `msgpack:"session_id"`
	Name        string       `msgpack:"name,omitempty"` // session name (from -name; empty if unset)
	Cmd         string       `msgpack:"cmd"`
	Rows        int          `msgpack:"rows"`
	Cols        int          `msgpack:"cols"`
	CreatedAt   int64        `msgpack:"created_at"`
	ClientCount int          `msgpack:"client_count"`
	Clients     []ClientInfo `msgpack:"clients,omitempty"` // currently connected clients
	UDPEnabled  bool         `msgpack:"udp_enabled,omitempty"`
	UDPPort     int          `msgpack:"udp_port,omitempty"` // session's UDP listening port
	PTYClosed   bool         `msgpack:"pty_closed,omitempty"`
	// Detach tracking.
	LastDetachedAt int64  `msgpack:"last_detached_at,omitempty"` // when the client count last dropped to 0 (Unix seconds)
	LastUDPAddr    string `msgpack:"last_udp_addr,omitempty"`    // last valid UDP source IP (legacy, unset by current servers)
	// Freshness: the session's last PTY output/input times (Unix seconds).
	// Updated regardless of whether any client is attached (unlike
	// ClientInfo.LastSeen).
	LastOutputAt int64 `msgpack:"last_output_at,omitempty"`
	LastInputAt  int64 `msgpack:"last_input_at,omitempty"`
	// Output ring buffer statistics.
	OutputChunks      int   `msgpack:"output_chunks,omitempty"`       // chunk count in the hot tier
	OutputBufferBytes int   `msgpack:"output_buffer_bytes,omitempty"` // raw bytes in the hot tier + compression queue
	OldestChunkTime   int64 `msgpack:"oldest_chunk_time,omitempty"`   // timestamp of the oldest retained chunk across tiers (Unix seconds)
	// Cold-tier (flate-compressed older output) statistics.
	OutputColdSegments int `msgpack:"output_cold_segments,omitempty"`  // segment count
	OutputColdBytes    int `msgpack:"output_cold_bytes,omitempty"`     // compressed bytes (approximates memory use)
	OutputColdRawBytes int `msgpack:"output_cold_raw_bytes,omitempty"` // uncompressed bytes (approximates retained output)
}

type SessionsListMsg struct {
	Type     string        `msgpack:"type"`
	Sessions []SessionInfo `msgpack:"sessions"`
}

type SessionInfoMsg struct {
	Type    string      `msgpack:"type"`
	Session SessionInfo `msgpack:"session"`
}

type SessionKilledMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

// ServerMetaMsg returns the server's metadata.
type ServerMetaMsg struct {
	Type string `msgpack:"type"`
	// Server-wide metadata.
	ServerBuildID    string `msgpack:"server_build_id,omitempty"`    // server build ID (git hash)
	ServerBuildTime  string `msgpack:"server_build_time,omitempty"`  // server build time
	ServerInstanceID []byte `msgpack:"server_instance_id,omitempty"` // server instance ID (8 bytes; restart detection)
}

// Encode encodes a message to msgpack bytes
func Encode(msg interface{}) ([]byte, error) {
	return msgpack.Marshal(msg)
}

// Decode decodes msgpack bytes into a message based on "type" field
func Decode(data []byte) (interface{}, error) {
	// First decode just the type field
	var base BaseMsg
	if err := msgpack.Unmarshal(data, &base); err != nil {
		return nil, err
	}

	// Then decode the full message based on type
	switch base.Type {
	case "HELLO":
		var m HelloMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "CREATE_SESSION":
		var m CreateSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "ATTACH_SESSION":
		var m AttachSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "INPUT":
		var m InputMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "RESIZE":
		var m ResizeMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "PING":
		var m PingMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "LIST_SESSIONS":
		var m ListSessionsMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "GET_SESSION_INFO":
		var m GetSessionInfoMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "KILL_SESSION":
		var m KillSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "WAIT_SESSION":
		var m WaitSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "UDP_CLIENT_INFO":
		var m UDPClientInfoMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "GET_SERVER_META":
		var m GetServerMetaMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "WELCOME":
		var m WelcomeMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSION_CREATED":
		var m SessionCreatedMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "ERROR":
		var m ErrorMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "PONG":
		var m PongMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "NOTE":
		var m NoteMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSIONS_LIST":
		var m SessionsListMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSION_INFO":
		var m SessionInfoMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSION_KILLED":
		var m SessionKilledMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SERVER_META":
		var m ServerMetaMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "OUTPUT":
		var m OutputMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("unknown message type: %s", base.Type)
	}
}

// Error codes
const (
	ErrBadRequest      = "BAD_REQUEST"
	ErrNoSuchSession   = "NO_SUCH_SESSION"
	ErrInternal        = "INTERNAL"
	ErrBusy            = "BUSY"
	ErrUnauthorized    = "UNAUTHORIZED"      // authentication failure
	ErrDuplicateName   = "DUPLICATE_NAME"    // an active session with the same name already exists
	ErrTooManySessions = "TOO_MANY_SESSIONS" // active session count reached the --max-sessions limit
)

// ValidateSessionName validates the form of a session name given via -name.
// Used on both the client and the server. Names are restricted to
// alphanumerics plus . _ - and at most 63 characters, to keep them friendly
// to list columns and scripting.
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("session name cannot be empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("session name too long (max 63 chars): %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("invalid session name %q (use letters, digits, '.', '_', '-')", name)
		}
	}
	return nil
}
