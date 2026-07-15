package main

// sessions_json.go
//
// -list / -info の JSON 出力（-json フラグ併用時）。
// stats.go と同じ流儀: 専用の ...JSON 構造体 + snake_case タグ + MarshalIndent。
// wire 構造体 (proto.SessionInfo) を直接 Marshal せず変換を挟むことで、
// JSON 出力の互換性を wire フォーマットの変更から切り離す。
// 時刻は RFC3339 文字列（stats.go と同様）。

import (
	"encoding/json"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
)

type sessionClientJSON struct {
	ID                  string   `json:"id"`
	Protocol            string   `json:"protocol"`
	RemoteAddr          string   `json:"remote_addr,omitempty"`
	QUICRemoteAddr      string   `json:"quic_remote_addr,omitempty"`
	UDPClientID         uint16   `json:"udp_client_id,omitempty"`
	UDPAddresses        []string `json:"udp_addresses,omitempty"`
	OutputSentBytes     int      `json:"output_sent_bytes,omitempty"`
	SlowOutputWrites    uint64   `json:"slow_output_writes,omitempty"`
	MaxOutputWriteMs    uint64   `json:"max_output_write_ms,omitempty"`
	OutputStallEpisodes uint64   `json:"output_stall_episodes,omitempty"`
	OutputStallMs       uint64   `json:"output_stall_ms,omitempty"`
	LastSeenAt          string   `json:"last_seen_at,omitempty"`
	// TCP ポートフォワード（-L）統計
	ForwardsActive         int    `json:"forwards_active,omitempty"`
	ForwardsOpened         uint64 `json:"forwards_opened,omitempty"`
	ForwardBytesToTarget   uint64 `json:"forward_bytes_to_target,omitempty"`
	ForwardBytesFromTarget uint64 `json:"forward_bytes_from_target,omitempty"`
}

type sessionJSON struct {
	SessionID          string              `json:"session_id"`
	Name               string              `json:"name,omitempty"`
	Cmd                string              `json:"cmd"`
	Rows               int                 `json:"rows"`
	Cols               int                 `json:"cols"`
	Status             string              `json:"status"` // "active" | "closed"
	ClientCount        int                 `json:"client_count"`
	CreatedAt          string              `json:"created_at"`
	LastDetachedAt     string              `json:"last_detached_at,omitempty"`
	LastActiveAt       string              `json:"last_active_at,omitempty"`
	LastOutputAt       string              `json:"last_output_at,omitempty"` // 最終 PTY 出力（attach 有無と無関係）
	LastInputAt        string              `json:"last_input_at,omitempty"`  // 最終 PTY 入力（同上）
	UDPEnabled         bool                `json:"udp_enabled"`
	UDPPort            int                 `json:"udp_port,omitempty"`
	OutputChunks       int                 `json:"output_chunks,omitempty"`
	OutputBufferBytes  int                 `json:"output_buffer_bytes,omitempty"`
	OutputColdSegments int                 `json:"output_cold_segments,omitempty"`
	OutputColdBytes    int                 `json:"output_cold_bytes,omitempty"`
	OutputColdRawBytes int                 `json:"output_cold_raw_bytes,omitempty"`
	OldestChunkAt      string              `json:"oldest_chunk_at,omitempty"`
	Clients            []sessionClientJSON `json:"clients"`
}

type sessionsListJSON struct {
	Sessions []sessionJSON `json:"sessions"`
}

// unixRFC3339 は Unix 秒を RFC3339 文字列にする。0 は「未設定」として空文字。
func unixRFC3339(ts int64, loc *time.Location) string {
	if ts == 0 {
		return ""
	}
	return time.Unix(ts, 0).In(loc).Format(time.RFC3339)
}

func toSessionJSON(s proto.SessionInfo, loc *time.Location) sessionJSON {
	status := "active"
	if s.PTYClosed {
		status = "closed"
	}

	// 全クライアントの LastSeen の最大値をセッションの最終活動時刻とする
	// （formatSessionsList と同じ定義。JSON では相対時間でなく絶対時刻で返す）
	var maxLastSeen int64
	clients := make([]sessionClientJSON, 0, len(s.Clients))
	for _, c := range s.Clients {
		if c.LastSeen > maxLastSeen {
			maxLastSeen = c.LastSeen
		}
		clients = append(clients, sessionClientJSON{
			ID:                     c.ID,
			Protocol:               c.Protocol,
			RemoteAddr:             c.RemoteAddr,
			QUICRemoteAddr:         c.QUICRemoteAddr,
			UDPClientID:            c.UDPClientID,
			UDPAddresses:           c.UDPAddresses,
			OutputSentBytes:        c.SendBufferBytes,
			SlowOutputWrites:       c.SlowOutputWrites,
			MaxOutputWriteMs:       c.MaxOutputWriteMs,
			OutputStallEpisodes:    c.OutputStallEpisodes,
			OutputStallMs:          c.OutputStallMs,
			LastSeenAt:             unixRFC3339(c.LastSeen, loc),
			ForwardsActive:         c.ForwardsActive,
			ForwardsOpened:         c.ForwardsOpened,
			ForwardBytesToTarget:   c.ForwardBytesToTarget,
			ForwardBytesFromTarget: c.ForwardBytesFromTarget,
		})
	}

	return sessionJSON{
		SessionID:          s.SessionID,
		Name:               s.Name,
		Cmd:                s.Cmd,
		Rows:               s.Rows,
		Cols:               s.Cols,
		Status:             status,
		ClientCount:        s.ClientCount,
		CreatedAt:          unixRFC3339(s.CreatedAt, loc),
		LastDetachedAt:     unixRFC3339(s.LastDetachedAt, loc),
		LastActiveAt:       unixRFC3339(maxLastSeen, loc),
		LastOutputAt:       unixRFC3339(s.LastOutputAt, loc),
		LastInputAt:        unixRFC3339(s.LastInputAt, loc),
		UDPEnabled:         s.UDPEnabled,
		UDPPort:            s.UDPPort,
		OutputChunks:       s.OutputChunks,
		OutputBufferBytes:  s.OutputBufferBytes,
		OutputColdSegments: s.OutputColdSegments,
		OutputColdBytes:    s.OutputColdBytes,
		OutputColdRawBytes: s.OutputColdRawBytes,
		OldestChunkAt:      unixRFC3339(s.OldestChunkTime, loc),
		Clients:            clients,
	}
}

// formatSessionsListJSON は -list -json の出力文字列を組み立てる純粋関数。
func formatSessionsListJSON(sessions []proto.SessionInfo, loc *time.Location) (string, error) {
	out := sessionsListJSON{Sessions: make([]sessionJSON, 0, len(sessions))}
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, toSessionJSON(s, loc))
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// formatSessionInfoJSON は -info <id> -json の出力文字列を組み立てる純粋関数。
func formatSessionInfoJSON(s proto.SessionInfo, loc *time.Location) (string, error) {
	data, err := json.MarshalIndent(toSessionJSON(s, loc), "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}
