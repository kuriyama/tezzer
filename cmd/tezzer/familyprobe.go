package main

// familyprobe.go: v4/v6 の非アクティブ family 到達性を低頻度でバックグラウンド確認し、
// reconnect() が必要になった時点で候補リストの鮮度を保っておく
// （v4/v6 の片方だけが死ぬ環境で、生きている family へ早く移れるようにする）。
//
// reconnect() 自体の並行 Dial ロジック（internal/qtransport の dialParallel）は
// 変更しない。候補を間引くと、誤判定時に唯一生きている候補まで消してしまう
// リスクがあるため、候補リストの「更新」だけを行い「間引き」はしない
// （SetRoamingCandidates は常に丸ごと差し替えるだけの安価な操作）。
//
// 2段構成:
//   - 安価な事前フィルタ（hasLocalRoute）: そのfamilyのローカル経路が無ければ
//     STUN 問い合わせ自体をスキップする。IPv4-only/IPv6-only ホストで、存在しない
//     family へ延々と無駄な STUN 往復を送り続けないようにするため。
//   - 低頻度の実チェック: 経路があるfamilyだけ、既存の STUN 問い合わせ
//     （getClientSTUNAddr）を candidateRefreshInterval おきに回し、
//     候補リストを SetRoamingCandidates で更新する。

import (
	"log"
	"net"
	"strings"
	"time"
)

const candidateRefreshInterval = 90 * time.Second

// probeTargets は経路の有無だけを見る宛先。UDP dial はパケットを送らず経路解決の
// みなので、実際に到達可能である必要はない（cmd/tezzerd の getLocalAddrs と同じ
// 宛先を使い、経路解決トリックの実績に合わせる）。
var probeTargets = map[string]string{
	"udp4": "8.8.8.8:80",
	"udp6": "[2001:4860:4860::8888]:80",
}

// hasLocalRoute は network（"udp4"/"udp6"）へのローカル経路が target 宛に存在するかを、
// 実パケットを送らない UDP dial（経路解決のみ）で安価に判定する。
func hasLocalRoute(network, target string) bool {
	conn, err := net.Dial(network, target)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// startCandidateRefreshLoop は非アクティブ family を含む候補アドレスを低頻度で
// 再解決し続け、SetRoamingCandidates で更新する。quicInfo.STUNAddrs が空
// （STUN 未使用/固定ポート運用）なら family 冗長の余地がないので何もしない。
func (c *Client) startCandidateRefreshLoop(quicInfo *QUICInfo) {
	if len(quicInfo.STUNAddrs) == 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(candidateRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				c.refreshCandidatesOnce(quicInfo)
			}
		}
	}()
}

// refreshCandidatesOnce は1回分の再解決を行う。何も取得できなければ既存の候補を
// 維持する（安全側に倒す。取れなかった＝家族が本当に死んだのか一時的な失敗なのか
// この場では区別できないため、良かれと思って空にするのが一番危険）。
func (c *Client) refreshCandidatesOnce(quicInfo *QUICInfo) {
	ct := c.transport()
	if ct == nil {
		return
	}

	var clientSTUN []string
	if hasLocalRoute("udp4", probeTargets["udp4"]) {
		if addr, err := c.getClientSTUNAddr("udp4", quicInfo.STUNServer); err == nil {
			clientSTUN = append(clientSTUN, addr.String())
		}
	}
	if !c.ipv4Only && hasLocalRoute("udp6", probeTargets["udp6"]) {
		if addr, err := c.getClientSTUNAddr("udp6", quicInfo.STUNServer); err == nil {
			clientSTUN = append(clientSTUN, addr.String())
		}
	}

	fresh := buildQUICCandidateAddrs(quicInfo, clientSTUN)
	if len(fresh) == 0 {
		return
	}
	ct.SetRoamingCandidates(fresh)
	if c.isDebugEnabled() {
		log.Printf("QUIC: background candidate refresh: %s", strings.Join(fresh, ", "))
	}
}
