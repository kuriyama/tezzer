package session

// rforward.go: リモートポートフォワード（-R）のサーバ側 listener と登録表。
// 設計は docs/dev/port-forwarding.md の「Phase 2 設計: `-R`」。
//
// listen ポートはセッションではなく tezzerd（ホスト）全体の資源なので、セッション別
// transport・共有 transport をまたいで Manager が 1 つの登録表で持つ。
//
//   - 同じ端末（device ID）から同じポートを登録したクライアントは provider として束ねる。
//     着信は接続中の provider のうち最後に Hello したものへ中継する。
//   - 別の端末が同じポートを登録した場合は後勝ちにせず「待機」させる。listener を持つ
//     端末の provider がいなくなった時点で、待機中の端末へ引き継ぐ。
//   - bind に失敗したポート（ホスト上の別プロセスが使用中）は rfwdRetryInterval ごとに
//     bind をやり直す。
//   - 明示的に終了した provider は即座に外し、接続を失った provider は休眠として
//     rfwdDormantTTL の間残す（休眠中も listener を手放さず、着信は即 close する。
//     空いたポートを別プロセスに取られて復帰後に再登録できなくなるのを防ぐ）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/transport"
)

const (
	// rfwdDormantTTL は接続を失った provider を休眠として残す期間。
	rfwdDormantTTL = 24 * time.Hour
	// rfwdRetryInterval は bind 失敗の再試行と休眠の失効を確認する間隔。
	rfwdRetryInterval = 30 * time.Second
	// rfwdOpenTimeout は provider への中継路確立（OpenRemoteForward）のタイムアウト。
	// クライアント側の dial（10 秒）を含む。
	rfwdOpenTimeout = 15 * time.Second
	// maxRFwdConnsPerPort は 1 ポートあたりの同時中継接続数の上限。
	maxRFwdConnsPerPort = 64
	// maxRFwdPortsPerClient は 1 クライアントが登録できるポート数の上限（超過分は無視）。
	maxRFwdPortsPerClient = 16
)

// rfwdProv は 1 ポートに対する 1 クライアントの登録。
type rfwdProv struct {
	st      transport.ServerTransport
	id      transport.ClientID
	device  string
	live    bool      // 接続中（false = 休眠）
	helloAt time.Time // 最後の Hello（provider 選出に使う）
	lostAt  time.Time // 接続を失った時刻（休眠の失効判定）
	notice  string    // 最後にクライアントへ通知した状態（同じ通知を繰り返さない）
}

// rfwdPort は 1 つの listen ポートの状態。
type rfwdPort struct {
	port    uint16
	owner   string // listener を持つ端末の device ID（"" = 未 bind）
	lns     []net.Listener
	bindErr string // 直近の bind 失敗の理由（owner == "" の間のみ意味を持つ）
	provs   map[transport.ClientID]*rfwdProv

	active    atomic.Int32
	opened    atomic.Uint64
	bytesIn   atomic.Uint64 // サーバ側の接続元 → クライアント側の dial 先
	bytesOut  atomic.Uint64 // クライアント側の dial 先 → サーバ側の接続元
	ownerFrom time.Time     // owner がこの端末になった時刻
}

// rfwdNotice はロック外で送るクライアントへの通知。
type rfwdNotice struct {
	st  transport.ServerTransport
	id  transport.ClientID
	msg string
}

// rfwdRegistry は -R の登録表。
type rfwdRegistry struct {
	mu    sync.Mutex
	ports map[uint16]*rfwdPort

	// テストで差し替える。
	listen func(network, addr string) (net.Listener, error)
	now    func() time.Time
}

func newRFwdRegistry() *rfwdRegistry {
	return &rfwdRegistry{
		ports:  make(map[uint16]*rfwdPort),
		listen: net.Listen,
		now:    time.Now,
	}
}

// wire は transport に -R のコールバックを登録する（-R 対応 transport のみ）。
func (r *rfwdRegistry) wire(st transport.ServerTransport) {
	rf, ok := st.(transport.RemoteForwarder)
	if !ok {
		return
	}
	rf.OnRemoteForward(
		func(id transport.ClientID, device string, ports []uint16) { r.hello(st, id, device, ports) },
		r.gone,
	)
}

// hello はクライアントの登録（初回接続・reconnect）を反映する。
func (r *rfwdRegistry) hello(st transport.ServerTransport, id transport.ClientID, device string, ports []uint16) {
	if len(ports) > maxRFwdPortsPerClient {
		ports = ports[:maxRFwdPortsPerClient]
	}
	want := make(map[uint16]bool, len(ports))
	for _, p := range ports {
		if p != 0 {
			want[p] = true
		}
	}
	now := r.now()

	r.mu.Lock()
	var notices []rfwdNotice
	// 登録内容が変わった場合（通常は起きない）に備え、外れたポートの登録を消す。
	for port, p := range r.ports {
		if _, ok := p.provs[id]; ok && !want[port] {
			delete(p.provs, id)
			notices = append(notices, r.reconcileLocked(p)...)
		}
	}
	for port := range want {
		p := r.ports[port]
		if p == nil {
			p = &rfwdPort{port: port, provs: make(map[transport.ClientID]*rfwdProv)}
			r.ports[port] = p
		}
		pr := p.provs[id]
		if pr == nil || pr.device != device {
			pr = &rfwdProv{id: id, device: device}
			p.provs[id] = pr
		}
		pr.st = st
		pr.live = true
		pr.helloAt = now
		pr.lostAt = time.Time{}
		// reconnect 後は状態を改めて伝える（切断中に状況が変わっていても分かるように）。
		pr.notice = ""
		notices = append(notices, r.reconcileLocked(p)...)
	}
	r.mu.Unlock()
	r.send(notices)
}

// gone はクライアントの接続終了を反映する。explicit = クライアントが意図して閉じた。
func (r *rfwdRegistry) gone(id transport.ClientID, explicit bool) {
	now := r.now()
	r.mu.Lock()
	var notices []rfwdNotice
	for _, p := range r.ports {
		pr, ok := p.provs[id]
		if !ok {
			continue
		}
		if explicit || pr.device != p.owner {
			// 明示的終了、または listener を持たない側（待機中）は休眠させる意味がない。
			delete(p.provs, id)
		} else {
			pr.live = false
			pr.lostAt = now
		}
		notices = append(notices, r.reconcileLocked(p)...)
	}
	r.mu.Unlock()
	r.send(notices)
}

// dropSession はセッション削除時に、そのセッションのクライアントの登録を外す。
func (r *rfwdRegistry) dropSession(sessionID string) {
	r.mu.Lock()
	var notices []rfwdNotice
	for _, p := range r.ports {
		changed := false
		for id := range p.provs {
			if id.Session == sessionID {
				delete(p.provs, id)
				changed = true
			}
		}
		if changed {
			notices = append(notices, r.reconcileLocked(p)...)
		}
	}
	r.mu.Unlock()
	r.send(notices)
}

// tick は休眠の失効と bind の再試行を行う（rfwdRetryInterval ごと）。
func (r *rfwdRegistry) tick() {
	r.mu.Lock()
	var notices []rfwdNotice
	for _, p := range r.ports {
		notices = append(notices, r.reconcileLocked(p)...)
	}
	r.mu.Unlock()
	r.send(notices)
}

// closeAll は全 listener を閉じる（Manager.Close 用）。
func (r *rfwdRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for port, p := range r.ports {
		closeListeners(p)
		delete(r.ports, port)
	}
}

func closeListeners(p *rfwdPort) {
	for _, ln := range p.lns {
		_ = ln.Close()
	}
	p.lns = nil
	p.owner = ""
}

// reconcileLocked はポートの状態を登録内容に合わせる（r.mu 保持で呼ぶ）。
// 休眠の失効 → listener の解放 → 待機中の端末への引き継ぎ（bind）→ 通知の順。
func (r *rfwdRegistry) reconcileLocked(p *rfwdPort) []rfwdNotice {
	now := r.now()
	for id, pr := range p.provs {
		if !pr.live && now.Sub(pr.lostAt) > rfwdDormantTTL {
			delete(p.provs, id)
		}
	}

	// listener を持つ端末の登録（接続中・休眠のどちらでも）が残っていなければ手放す。
	if p.owner != "" && !p.hasDevice(p.owner) {
		log.Printf("rforward: port %d: released (no client of device %s left)", p.port, shortDevice(p.owner))
		closeListeners(p)
	}

	// 未 bind なら、接続中のうち最後に Hello した端末のために bind する。
	if p.owner == "" {
		if cand := p.latestLive(""); cand != nil {
			lns, err := r.bind(p.port)
			if err != nil {
				if p.bindErr != err.Error() {
					log.Printf("rforward: port %d: bind failed (retrying every %v): %v", p.port, rfwdRetryInterval, err)
				}
				p.bindErr = err.Error()
			} else {
				p.owner = cand.device
				p.lns = lns
				p.bindErr = ""
				p.ownerFrom = now
				log.Printf("rforward: port %d: listening for device %s", p.port, shortDevice(p.owner))
				for _, ln := range lns {
					go r.acceptLoop(p, ln)
				}
			}
		}
	}

	var notices []rfwdNotice
	for _, pr := range p.provs {
		if !pr.live {
			continue
		}
		state, msg := p.stateFor(pr)
		if state == pr.notice {
			continue
		}
		pr.notice = state
		notices = append(notices, rfwdNotice{st: pr.st, id: pr.id, msg: msg})
	}

	if len(p.provs) == 0 {
		closeListeners(p)
		delete(r.ports, p.port)
	}
	return notices
}

// stateFor は provider から見たポートの状態（通知の重複判定用のキーと本文）を返す。
func (p *rfwdPort) stateFor(pr *rfwdProv) (string, string) {
	switch {
	case p.owner == pr.device:
		return "active", fmt.Sprintf("-R %d: listening on server localhost:%d", p.port, p.port)
	case p.owner != "":
		return "busy", fmt.Sprintf("-R %d: port in use by another device; waiting until it is released", p.port)
	case p.bindErr != "":
		return "bindfail:" + p.bindErr, fmt.Sprintf("-R %d: cannot listen on server (%s); retrying every %v", p.port, p.bindErr, rfwdRetryInterval)
	default:
		return "", ""
	}
}

func (p *rfwdPort) hasDevice(device string) bool {
	for _, pr := range p.provs {
		if pr.device == device {
			return true
		}
	}
	return false
}

// latestLive は接続中の provider のうち最後に Hello したものを返す
// （device が空でなければその端末に限る）。
func (p *rfwdPort) latestLive(device string) *rfwdProv {
	var best *rfwdProv
	for _, pr := range p.provs {
		if !pr.live || (device != "" && pr.device != device) {
			continue
		}
		if best == nil || pr.helloAt.After(best.helloAt) {
			best = pr
		}
	}
	return best
}

// bind は loopback（127.0.0.1 と ::1）で listen する。127.0.0.1 の失敗はエラー、
// ::1 の失敗（IPv6 無効のホスト等）は無視する。
func (r *rfwdRegistry) bind(port uint16) ([]net.Listener, error) {
	ps := strconv.Itoa(int(port))
	ln4, err := r.listen("tcp4", net.JoinHostPort("127.0.0.1", ps))
	if err != nil {
		return nil, err
	}
	lns := []net.Listener{ln4}
	if ln6, err := r.listen("tcp6", net.JoinHostPort("::1", ps)); err == nil {
		lns = append(lns, ln6)
	}
	return lns, nil
}

func (r *rfwdRegistry) send(notices []rfwdNotice) {
	for _, n := range notices {
		_ = n.st.SendStatus(n.id, n.msg)
	}
}

// rfwdTarget は中継先の候補（ロック外で使うためのコピー）。
type rfwdTarget struct {
	rf transport.RemoteForwarder
	id transport.ClientID
}

// targets は p の listener を持つ端末の接続中 provider を、Hello の新しい順に返す。
func (r *rfwdRegistry) targets(p *rfwdPort) []rfwdTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	var live []*rfwdProv
	for _, pr := range p.provs {
		if pr.live && pr.device == p.owner {
			live = append(live, pr)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].helloAt.After(live[j].helloAt) })
	out := make([]rfwdTarget, 0, len(live))
	for _, pr := range live {
		if rf, ok := pr.st.(transport.RemoteForwarder); ok {
			out = append(out, rfwdTarget{rf: rf, id: pr.id})
		}
	}
	return out
}

func (r *rfwdRegistry) acceptLoop(p *rfwdPort, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener close（解放・引き継ぎ・終了）で抜ける
		}
		go r.handleConn(p, conn.(*net.TCPConn))
	}
}

// handleConn は着信 1 本を provider へ中継する。接続中の provider がいなければ即 close。
// ストリームを開けなかった場合だけ、別の provider で 1 回やり直す（dial 失敗は同じ端末
// なので結果が変わらず、やり直さない）。
func (r *rfwdRegistry) handleConn(p *rfwdPort, tconn *net.TCPConn) {
	defer tconn.Close()
	if p.active.Add(1) > maxRFwdConnsPerPort {
		p.active.Add(-1)
		log.Printf("rforward: port %d: too many connections (max %d)", p.port, maxRFwdConnsPerPort)
		return
	}
	defer p.active.Add(-1)

	targets := r.targets(p)
	if len(targets) == 0 {
		return
	}
	if len(targets) > 2 {
		targets = targets[:2]
	}
	var fc transport.ForwardConn
	for _, t := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), rfwdOpenTimeout)
		c, err := t.rf.OpenRemoteForward(ctx, t.id, p.port)
		cancel()
		if err == nil {
			log.Printf("rforward: port %d: %s -> client %d (session %s)", p.port, tconn.RemoteAddr(), t.id.Num, t.id.Session)
			fc = c
			break
		}
		log.Printf("rforward: port %d: client %d (session %s): %v", p.port, t.id.Num, t.id.Session, err)
		if errors.Is(err, transport.ErrForwardRejected) {
			return
		}
	}
	if fc == nil {
		return
	}
	defer fc.Close()
	p.opened.Add(1)
	pipeForward(tconn, fc, &p.bytesIn, &p.bytesOut)
}

// halfCloser は pipeForward が接続側に要求するインタフェース（*net.TCPConn / *net.UnixConn）。
type halfCloser interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// pipeForward は接続と QUIC 中継路を双方向に中継する。
// qtransport.forwardPipe と同じ半クローズ規約（正常 EOF は CloseWrite、エラーは Close）。
// toPeer / fromPeer には転送バイト数を計上する（nil なら計上しない）。
func pipeForward(c halfCloser, fc transport.ForwardConn, toPeer, fromPeer *atomic.Uint64) {
	done := make(chan struct{}, 2)
	go func() {
		n, err := io.Copy(fc, c)
		if toPeer != nil {
			toPeer.Add(uint64(n))
		}
		if err == nil {
			_ = fc.CloseWrite()
		} else {
			_ = fc.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		n, err := io.Copy(c, fc)
		if fromPeer != nil {
			fromPeer.Add(uint64(n))
		}
		if err == nil {
			_ = c.CloseWrite()
		} else {
			_ = c.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// shortDevice はログ・表示用に device ID の先頭 8 文字を返す。
func shortDevice(d string) string {
	if len(d) > 8 {
		return d[:8]
	}
	return d
}

func (r *rfwdRegistry) snapshot() []proto.RemoteForwardInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]proto.RemoteForwardInfo, 0, len(r.ports))
	for _, p := range r.ports {
		info := proto.RemoteForwardInfo{
			Port:     p.port,
			Device:   shortDevice(p.owner),
			Active:   int(p.active.Load()),
			Opened:   p.opened.Load(),
			BytesIn:  p.bytesIn.Load(),
			BytesOut: p.bytesOut.Load(),
		}
		if p.owner == "" {
			info.BindError = p.bindErr
		} else {
			info.ListenFrom = p.ownerFrom.Unix()
		}
		for _, pr := range p.provs {
			switch {
			case p.owner == "" || pr.device != p.owner:
				info.Waiting++
			case pr.live:
				info.Live++
			default:
				info.Dormant++
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}
