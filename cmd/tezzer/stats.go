package main

import (
	"encoding/json"
	"fmt"
	"github.com/kuriyama/tezzer/internal/version"
	"os"
	"path/filepath"
	"time"
)

type clientStatsJSON struct {
	Timestamp     string              `json:"timestamp"`
	SessionID     string              `json:"session_id"`
	Client        clientInfoJSON      `json:"client"`
	Server        *serverInfoJSON     `json:"server,omitempty"`
	Transport     *transportStatsJSON `json:"transport,omitempty"`
	Notifications []notifEntryJSON    `json:"notifications"`
	LogFile       string              `json:"log_file,omitempty"`
}

type clientInfoJSON struct {
	BuildID      string `json:"build_id"`
	BuiltAt      string `json:"built_at"`
	TerminalCols int    `json:"terminal_cols"`
	TerminalRows int    `json:"terminal_rows"`
	DebugOutput  bool   `json:"debug_output"`
}

type serverInfoJSON struct {
	BuildID string `json:"build_id"`
	BuiltAt string `json:"built_at"`
}

type transportStatsJSON struct {
	State          string  `json:"state"`
	RTTMs          float64 `json:"rtt_ms"`
	LossRate       float64 `json:"loss_rate"`
	BytesSent      uint64  `json:"bytes_sent"`
	BytesReceived  uint64  `json:"bytes_received"`
	RecoveryCount  uint64  `json:"recovery_count"`
	LastRecoveryMs float64 `json:"last_recovery_ms"`
}

type notifEntryJSON struct {
	Time    string `json:"time"`
	Message string `json:"message"`
}

func statsFilePath(sessionID string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".tezzer", "stats", sessionID+".stats.json"), nil
}

func (c *Client) writeStatsFile() {
	c.metaMu.RLock()
	hasMeta := c.serverMeta != nil
	c.metaMu.RUnlock()
	if !hasMeta {
		_ = c.fetchServerMeta()
	}

	stats := clientStatsJSON{
		Timestamp: time.Now().Format(time.RFC3339),
		SessionID: c.sessionID,
		Client: clientInfoJSON{
			BuildID:      version.GetVersion(),
			BuiltAt:      version.GetBuildTime(),
			TerminalCols: c.width,
			TerminalRows: c.height,
			DebugOutput:  c.isDebugEnabled(),
		},
	}

	c.metaMu.RLock()
	meta := c.serverMeta
	c.metaMu.RUnlock()
	if meta != nil {
		stats.Server = &serverInfoJSON{
			BuildID: meta.ServerBuildID,
			BuiltAt: meta.ServerBuildTime,
		}
	}

	if ct := c.transport(); ct != nil {
		s := ct.Stats()
		stats.Transport = &transportStatsJSON{
			State:          ct.State().String(),
			RTTMs:          s.RTT,
			LossRate:       s.LossRate,
			BytesSent:      s.BytesSent,
			BytesReceived:  s.BytesReceived,
			RecoveryCount:  s.RecoveryCount,
			LastRecoveryMs: s.LastRecoveryMs,
		}
	}

	logs := c.getLogMessages()
	stats.Notifications = make([]notifEntryJSON, 0, len(logs))
	for _, e := range logs {
		stats.Notifications = append(stats.Notifications, notifEntryJSON{
			Time:    e.timestamp.Format(time.RFC3339),
			Message: e.message,
		})
	}

	if c.logFile != nil {
		stats.LogFile = c.logFile.Name()
	}

	statsPath, err := statsFilePath(c.sessionID)
	if err != nil {
		c.setStatusMessage(fmt.Sprintf("Stats: %v", err))
		return
	}
	if err := os.MkdirAll(filepath.Dir(statsPath), 0700); err != nil {
		c.setStatusMessage(fmt.Sprintf("Stats: mkdir: %v", err))
		return
	}

	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		c.setStatusMessage(fmt.Sprintf("Stats: marshal: %v", err))
		return
	}
	if err := os.WriteFile(statsPath, data, 0600); err != nil {
		c.setStatusMessage(fmt.Sprintf("Stats: write: %v", err))
		return
	}

	c.setStatusMessage(fmt.Sprintf("Stats: %s", statsPath))
}

func showSessionStats(sessionID string, jsonOut bool) error {
	statsPath, err := statsFilePath(sessionID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(statsPath)
	if err != nil {
		return fmt.Errorf("stats not found for %s (use Ctrl-^ s to generate): %w", sessionID, err)
	}
	if jsonOut {
		fmt.Print(string(data))
		return nil
	}
	var s clientStatsJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("parse error: %w", err)
	}
	fmt.Printf("Session:  %s\n", s.SessionID)
	fmt.Printf("Updated:  %s\n", s.Timestamp)
	fmt.Printf("\n--- Client ---\n")
	fmt.Printf("Build:    %s (%s)\n", s.Client.BuildID, s.Client.BuiltAt)
	fmt.Printf("Terminal: %dx%d\n", s.Client.TerminalCols, s.Client.TerminalRows)
	fmt.Printf("Debug:    %v\n", s.Client.DebugOutput)
	if s.Server != nil {
		fmt.Printf("\n--- Server ---\n")
		fmt.Printf("Build:    %s (%s)\n", s.Server.BuildID, s.Server.BuiltAt)
	}
	if s.Transport != nil {
		t := s.Transport
		fmt.Printf("\n--- Transport ---\n")
		fmt.Printf("State:    %s\n", t.State)
		fmt.Printf("RTT:      %.2f ms\n", t.RTTMs)
		fmt.Printf("Loss:     %.2f %%\n", t.LossRate*100)
		fmt.Printf("Sent:     %d bytes\n", t.BytesSent)
		fmt.Printf("Received: %d bytes\n", t.BytesReceived)
		fmt.Printf("Recoveries: %d (last %.0f ms)\n", t.RecoveryCount, t.LastRecoveryMs)
	}
	if s.LogFile != "" {
		fmt.Printf("\nLog:      %s\n", s.LogFile)
	}
	if len(s.Notifications) > 0 {
		fmt.Printf("\n--- Recent Notifications ---\n")
		for i := len(s.Notifications) - 1; i >= 0; i-- {
			e := s.Notifications[i]
			fmt.Printf("%s  %s\n", e.Time, e.Message)
		}
	}
	return nil
}
