package session

// handover.go: tezzerd の無停止再起動（self re-exec 方式）の状態シリアライズ・復元。
//
// 旧プロセスは WriteHandover で全セッションのロックを取ったまま（= world stop）状態を
// 書き出し、PTY master と QUIC UDP ソケットの fd を dup（CLOEXEC なし）して execve で
// 自分自身を新バイナリに置き換える。exec はプロセス置き換えなので PID・親子関係が
// 維持され、子プロセスの exit code 回収（proc.Wait）も新プロセスでそのまま機能する。
// QUIC の接続状態（ユーザー空間）だけは失われ、クライアントは既存の reconnect 機構で
// 復旧する（ポートと共有鍵 K は継承されるため同じ宛先に再接続できる）。
//
// ロックを保持したまま返る WriteHandover の契約に注意: 成功時は呼び出し元が exec する
// 前提でロックは解放されない（プロセスごと消える）。exec に失敗した場合のみ、返された
// abort() を呼んでロック解放と dup 済み fd のクローズを行い、通常運転を継続する。

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/vmihailenco/msgpack/v5"
)

// HandoverVersion は状態フォーマットの版数。互換性のない変更をしたら上げる。
// 新プロセスは不一致の状態を復元しない（セッションは失われるがサーバは起動する）。
const HandoverVersion = 1

// HandoverEnvVar は継承する状態ファイルの fd 番号を新プロセスへ伝える環境変数。
const HandoverEnvVar = "TEZZER_HANDOVER_FD"

type handoverChunk struct {
	Seq  uint64 `msgpack:"s"`
	TS   int64  `msgpack:"t"` // UnixNano（age evict の判定を引き継ぐ）
	Data []byte `msgpack:"d"`
}

type handoverColdSegment struct {
	StartSeq uint64 `msgpack:"ss"`
	EndSeq   uint64 `msgpack:"es"`
	RawBytes int    `msgpack:"rb"`
	Data     []byte `msgpack:"d"`
	OldestNS int64  `msgpack:"o"`
	NewestNS int64  `msgpack:"n"`
}

type handoverSession struct {
	ID           string   `msgpack:"id"`
	Name         string   `msgpack:"name"`
	Cmd          string   `msgpack:"cmd"`
	Args         []string `msgpack:"args"`
	Rows         int      `msgpack:"rows"`
	Cols         int      `msgpack:"cols"`
	CreatedAtNS  int64    `msgpack:"created"`
	Seq          uint64   `msgpack:"seq"`
	ChildPID     int      `msgpack:"pid"`
	PtyFd        int      `msgpack:"ptyfd"`
	AgentForward bool     `msgpack:"agent"`

	QUICClientIDsOut []uint16 `msgpack:"ucids"`
	LastDetachedNS   int64    `msgpack:"detached"`
	LastOutputNS     int64    `msgpack:"lastout"` // 旧フォーマットには無い（復元時 0 = 未設定）
	LastInputNS      int64    `msgpack:"lastin"`  // 同上

	// per-session transport（共有モードのセッションでは QuicFd = -1）
	Port   int    `msgpack:"port"`
	Key    []byte `msgpack:"key"`
	QuicFd int    `msgpack:"quicfd"`

	Hot         []handoverChunk       `msgpack:"hot"`
	ColdPending []handoverChunk       `msgpack:"coldp"`
	Cold        []handoverColdSegment `msgpack:"cold"`
}

type handoverState struct {
	Version    int    `msgpack:"v"`
	InstanceID []byte `msgpack:"iid"`
	// 共有 transport モード（nil/-1 = 非共有）
	SharedKey    []byte            `msgpack:"skey"`
	SharedPort   int               `msgpack:"sport"`
	SharedQuicFd int               `msgpack:"sfd"`
	Sessions     []handoverSession `msgpack:"sessions"`
}

// dupFdNoCloexec は syscall.Conn から fd を dup する（dup は FD_CLOEXEC を複製しない
// = 新 fd は exec を跨いで継承される）。os.File.Fd() と違い副作用がない。
func dupFdNoCloexec(sc syscall.Conn) (int, error) {
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var dupErr error
	if err := raw.Control(func(cfd uintptr) {
		fd, dupErr = syscall.Dup(int(cfd))
	}); err != nil {
		return -1, err
	}
	return fd, dupErr
}

func chunksToHandover(chunks []OutputChunk) []handoverChunk {
	out := make([]handoverChunk, len(chunks))
	for i, ch := range chunks {
		out[i] = handoverChunk{Seq: ch.Seq, TS: ch.Timestamp.UnixNano(), Data: ch.Data}
	}
	return out
}

func chunksFromHandover(hcs []handoverChunk) ([]OutputChunk, int) {
	out := make([]OutputChunk, len(hcs))
	bytes := 0
	for i, hc := range hcs {
		out[i] = OutputChunk{Seq: hc.Seq, Data: hc.Data, Timestamp: time.Unix(0, hc.TS)}
		bytes += len(hc.Data)
	}
	return out, bytes
}

// WriteHandover は全セッションのロックを取得したまま（= 出力の追記を止めて）状態を w へ
// 書き出す。成功時: ロックは保持されたまま返る（呼び出し元が exec する前提）。exec に
// 失敗した場合のみ abort() を呼ぶこと（ロック解放 + dup 済み fd のクローズ）。
// 失敗時: 内部で後始末してから err を返す（abort は nil）。
func (m *Manager) WriteHandover(w io.Writer) (abort func(), err error) {
	m.mu.Lock()
	var locked []*Session
	var dupFds []int
	release := func() {
		for _, fd := range dupFds {
			_ = syscall.Close(fd)
		}
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].mu.Unlock()
		}
		m.mu.Unlock()
	}
	fail := func(e error) (func(), error) {
		release()
		return nil, e
	}

	st := handoverState{
		Version:      HandoverVersion,
		InstanceID:   m.serverInstanceID[:],
		SharedQuicFd: -1,
	}
	if m.sharedTransport != nil {
		d, ok := m.sharedTransport.(transport.SocketHandover)
		if !ok {
			return fail(fmt.Errorf("shared transport does not support fd handover"))
		}
		fd, err := d.DupUDPSocketFd()
		if err != nil {
			return fail(fmt.Errorf("dup shared udp socket: %w", err))
		}
		dupFds = append(dupFds, fd)
		st.SharedQuicFd = fd
		st.SharedKey = m.sharedKey
		st.SharedPort = m.sharedPort
	}

	for _, s := range m.sessions {
		s.mu.Lock()
		locked = append(locked, s)
		if s.ptyClosed || s.ptyMaster == nil || s.proc == nil {
			continue // cleanup 待ちの残骸は引き継がない
		}
		ptyFd, err := dupFdNoCloexec(s.ptyMaster)
		if err != nil {
			return fail(fmt.Errorf("session %s: dup pty fd: %w", s.ID, err))
		}
		dupFds = append(dupFds, ptyFd)

		hs := handoverSession{
			ID:               s.ID,
			Name:             s.Name,
			Cmd:              s.Cmd,
			Args:             s.Args,
			Rows:             s.Rows,
			Cols:             s.Cols,
			CreatedAtNS:      s.CreatedAt.UnixNano(),
			Seq:              s.seq,
			ChildPID:         s.proc.Pid,
			PtyFd:            ptyFd,
			AgentForward:     s.agentListener != nil,
			QUICClientIDsOut: s.quicClientIDsOut,
			QuicFd:           -1,
			Hot:              chunksToHandover(s.outputChunks),
			ColdPending:      chunksToHandover(s.coldPending),
		}
		if !s.lastDetachedAt.IsZero() {
			hs.LastDetachedNS = s.lastDetachedAt.UnixNano()
		}
		if !s.lastOutputAt.IsZero() {
			hs.LastOutputNS = s.lastOutputAt.UnixNano()
		}
		if !s.lastInputAt.IsZero() {
			hs.LastInputNS = s.lastInputAt.UnixNano()
		}
		for _, seg := range s.coldSegments {
			hs.Cold = append(hs.Cold, handoverColdSegment{
				StartSeq: seg.startSeq,
				EndSeq:   seg.endSeq,
				RawBytes: seg.rawBytes,
				Data:     seg.data,
				OldestNS: seg.oldestTime.UnixNano(),
				NewestNS: seg.newestTime.UnixNano(),
			})
		}
		if s.st != nil && !s.usesSharedTransport {
			d, ok := s.st.(transport.SocketHandover)
			if !ok {
				return fail(fmt.Errorf("session %s: transport does not support fd handover", s.ID))
			}
			fd, err := d.DupUDPSocketFd()
			if err != nil {
				return fail(fmt.Errorf("session %s: dup udp socket: %w", s.ID, err))
			}
			dupFds = append(dupFds, fd)
			hs.QuicFd = fd
			hs.Port = s.quicPort
			hs.Key = s.quicKey
		}
		st.Sessions = append(st.Sessions, hs)
	}

	if err := msgpack.NewEncoder(w).Encode(&st); err != nil {
		return fail(fmt.Errorf("encode handover state: %w", err))
	}

	// クライアントの QUIC 接続を CONNECTION_CLOSE で明示的に切る。exec で黙って消えると
	// クライアントは idle timeout（60秒）まで接続死に気づけないため、即時 reconnect を
	// 誘発する。exec 失敗時（abort 経路）でもクライアントは旧プロセスへ再接続するだけで
	// 実害はない。
	disconnect := func(t transport.ServerTransport) {
		if d, ok := t.(transport.SocketHandover); ok {
			d.DisconnectAllClients("tezzerd restarting")
		}
	}
	if m.sharedTransport != nil {
		disconnect(m.sharedTransport)
	}
	for _, s := range m.sessions {
		if s.st != nil && !s.usesSharedTransport {
			disconnect(s.st)
		}
	}

	return release, nil
}

// packetConnFromFd は継承した fd から net.PacketConn を作る（fd 自体は複製後に閉じる）。
func packetConnFromFd(fd int, name string) (net.PacketConn, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, fmt.Errorf("invalid fd %d", fd)
	}
	defer f.Close() // FilePacketConn は fd を複製するので継承分は閉じてよい
	return net.FilePacketConn(f)
}

// RestoreHandover は旧プロセスが書き出した状態を読み込み、セッションを復元する。
// 戻り値は復元できたセッション数。共有 transport も状態に含まれていれば復元する。
func (m *Manager) RestoreHandover(r io.Reader) (int, error) {
	var st handoverState
	if err := msgpack.NewDecoder(r).Decode(&st); err != nil {
		return 0, fmt.Errorf("decode handover state: %w", err)
	}
	if st.Version != HandoverVersion {
		return 0, fmt.Errorf("handover state version mismatch: got %d want %d", st.Version, HandoverVersion)
	}
	// インスタンス ID を引き継ぐ（クライアントから見て「同じサーバ」に見せる）
	copy(m.serverInstanceID[:], st.InstanceID)

	if st.SharedQuicFd >= 0 {
		pc, err := packetConnFromFd(st.SharedQuicFd, "shared-quic-udp")
		if err != nil {
			return 0, fmt.Errorf("restore shared udp socket: %w", err)
		}
		sst, err := qtransport.NewServerFromPacketConn(st.SharedKey, pc)
		if err != nil {
			_ = pc.Close()
			return 0, fmt.Errorf("restore shared transport: %w", err)
		}
		if err := m.adoptSharedTransport(sst, st.SharedPort, st.SharedKey); err != nil {
			return 0, fmt.Errorf("adopt shared transport: %w", err)
		}
	}

	restored := 0
	for i := range st.Sessions {
		if err := m.restoreSession(&st.Sessions[i]); err != nil {
			log.Printf("handover: session %s restore failed (skipping): %v", st.Sessions[i].ID, err)
			continue
		}
		restored++
	}
	return restored, nil
}

// restoreSession は 1 セッションを復元する。失敗時は継承した fd を閉じ、子プロセスは
// PTY を失って自然終了する（zombie 化しないよう回収だけ行う）。
func (m *Manager) restoreSession(hs *handoverSession) error {
	proc, err := os.FindProcess(hs.ChildPID)
	if err != nil {
		_ = syscall.Close(hs.PtyFd)
		if hs.QuicFd >= 0 {
			_ = syscall.Close(hs.QuicFd)
		}
		return fmt.Errorf("find child process %d: %w", hs.ChildPID, err)
	}
	failCleanup := func() {
		// PTY を手放すと子はやがて EIO/SIGHUP で終わる。zombie 化しないよう回収する。
		go func() { _, _ = proc.Wait() }()
	}

	hot, hotBytes := chunksFromHandover(hs.Hot)
	coldPending, coldPendingBytes := chunksFromHandover(hs.ColdPending)

	s := &Session{
		ID:                hs.ID,
		Name:              hs.Name,
		Cmd:               hs.Cmd,
		Args:              hs.Args,
		Rows:              hs.Rows,
		Cols:              hs.Cols,
		CreatedAt:         time.Unix(0, hs.CreatedAtNS),
		ptyMaster:         os.NewFile(uintptr(hs.PtyFd), "pty-"+hs.ID),
		proc:              proc,
		seq:               hs.Seq,
		outputChunks:      hot,
		outputBufferBytes: hotBytes,
		coldPending:       coldPending,
		coldPendingBytes:  coldPendingBytes,
		clients:           make(map[string]*Client),
		quicClientIDsOut:  hs.QUICClientIDsOut,
		manager:           m,
		done:              make(chan struct{}),
		quicReadyCh:       make(chan struct{}),
		exitCode:          -1,
		debug:             IsDebugEnabled(),
	}
	if hs.LastDetachedNS != 0 {
		s.lastDetachedAt = time.Unix(0, hs.LastDetachedNS)
	}
	if hs.LastOutputNS != 0 {
		s.lastOutputAt = time.Unix(0, hs.LastOutputNS)
	}
	if hs.LastInputNS != 0 {
		s.lastInputAt = time.Unix(0, hs.LastInputNS)
	}
	for _, seg := range hs.Cold {
		s.coldSegments = append(s.coldSegments, coldSegment{
			startSeq:   seg.StartSeq,
			endSeq:     seg.EndSeq,
			rawBytes:   seg.RawBytes,
			data:       seg.Data,
			oldestTime: time.Unix(0, seg.OldestNS),
			newestTime: time.Unix(0, seg.NewestNS),
		})
		s.coldBytes += len(seg.Data)
		s.coldRawBytes += seg.RawBytes
	}
	// 復元セッションは確立済み（PTY 稼働中）なので QUIC pending ゲートは通過済み扱い。
	s.quicReady.Store(true)

	// agent forwarding（-A）: 同じパスで UDS を作り直す（sessionID 不変なので PTY 内の
	// SSH_AUTH_SOCK はそのまま有効。旧ソケットファイルの残骸は bind 前に削除される）。
	if hs.AgentForward {
		if ln, path, err := newAgentListener(s.ID); err != nil {
			log.Printf("handover: session %s: agent socket rebind failed (continuing without -A): %v", s.ID, err)
		} else {
			s.agentListener = ln
			s.agentSockPath = path
			go s.acceptAgentConns()
		}
	}

	// transport: 共有モードなら manager のものを参照、per-session なら fd から復元。
	if m.IsSharedTransportEnabled() {
		s.st = m.sharedTransport
		s.usesSharedTransport = true
		s.quicEnabled = true
		s.quicPort = m.GetSharedPort()
		s.quicKey = m.GetSharedKey()
	} else if hs.QuicFd >= 0 {
		pc, err := packetConnFromFd(hs.QuicFd, "quic-udp-"+hs.ID)
		if err != nil {
			_ = s.ptyMaster.Close()
			failCleanup()
			return fmt.Errorf("restore udp socket: %w", err)
		}
		st, err := qtransport.NewServerFromPacketConn(hs.Key, pc)
		if err != nil {
			_ = pc.Close()
			_ = s.ptyMaster.Close()
			failCleanup()
			return fmt.Errorf("restore transport: %w", err)
		}
		if err := s.adoptPerSessionTransport(st, hs.Port, hs.Key); err != nil {
			_ = s.ptyMaster.Close()
			failCleanup()
			return fmt.Errorf("adopt transport: %w", err)
		}
	}

	go s.ptyReader()

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	log.Printf("handover: session %s restored (pid=%d, seq=%d, name=%q)", s.ID, hs.ChildPID, hs.Seq, hs.Name)
	return nil
}
