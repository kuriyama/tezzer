package netx

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// baseDir returns the base directory used for tezzer's UDS sockets.
// Priority: $XDG_RUNTIME_DIR > ~/.tezzer (both created 0700 if missing).
func baseDir() (string, error) {
	// Try $XDG_RUNTIME_DIR first (standard on modern Linux)
	if xdgRuntime := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntime != "" {
		if err := os.MkdirAll(xdgRuntime, 0700); err == nil {
			return xdgRuntime, nil
		}
	}

	// Fallback to ~/.tezzer
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	tezzerDir := filepath.Join(home, ".tezzer")
	if err := os.MkdirAll(tezzerDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create tezzer directory: %w", err)
	}
	return tezzerDir, nil
}

// GetDefaultSocketPath returns the default Unix Domain Socket path
// Priority: $XDG_RUNTIME_DIR/tezzer.sock > ~/.tezzer/tezzer.sock
func GetDefaultSocketPath() (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tezzer.sock"), nil
}

// GetAgentSocketPath returns the per-session UDS path used for SSH agent
// forwarding (-A). Sockets live in a dedicated subdirectory (0700) under the
// same base directory as GetDefaultSocketPath, so a compromised session's
// forwarding socket isn't reachable outside the owning user.
func GetAgentSocketPath(sessionID string) (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	agentDir := filepath.Join(dir, "tezzer-agent")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create agent socket directory: %w", err)
	}
	return filepath.Join(agentDir, sessionID+".sock"), nil
}

// GetPeerUID returns the UID of the peer connected to a Unix domain socket
// (SO_PEERCRED on Linux, LOCAL_PEERCRED on macOS and FreeBSD).
func GetPeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a Unix connection")
	}
	file, err := unixConn.File()
	if err != nil {
		return 0, fmt.Errorf("failed to get file descriptor: %w", err)
	}
	defer file.Close()
	uid, err := peerUID(int(file.Fd()))
	if err != nil {
		return 0, fmt.Errorf("failed to get peer credentials: %w", err)
	}
	return uid, nil
}
