package main

// stuncache.go: STUN 問い合わせ結果（公開 IP）の TTL キャッシュ。
//
// resolveUDPAddrs は CREATE/ATTACH のたびに同期 STUN を問い合わせ、その間
// SESSION_CREATED 応答がブロックされていた（attach 連発のスクリプト運用で毎回
// 往復が乗る。STUN がブラックホールされる環境では 5 秒タイムアウト × family 分）。
// 公開 IP は数分単位では変わらないため、成功は stunCacheTTL、失敗も
// stunCacheFailureTTL のネガティブキャッシュで記憶して再問い合わせを抑える。
//
// サーバホストのネットワークが変わった直後は最大 TTL 分だけ古い IP を広告しうるが、
// クライアント側には LAN/loopback 候補と全候補失敗時のリフレッシャーがあるため
// 致命的ではない（サーバ側のアドレス変更自体がまれ）。

import (
	"net"
	"sync"
	"time"
)

const (
	stunCacheTTL        = 5 * time.Minute  // 成功結果の保持
	stunCacheFailureTTL = 30 * time.Second // 失敗（不通）の再問い合わせ抑制
)

type stunCacheEntry struct {
	ip      net.IP // nil = 失敗を記憶（ネガティブキャッシュ）
	expires time.Time
}

// stunCache は key（network + STUN サーバー）ごとの TTL キャッシュ。
// ミス時の実問い合わせはロック外で行うため、コールドミスが並行すると重複
// 問い合わせになりうるが、無害（同じ結果を上書きするだけ）なので許容する。
type stunCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	failureTTL time.Duration
	entries    map[string]stunCacheEntry
}

func newSTUNCache(ttl, failureTTL time.Duration) *stunCache {
	return &stunCache{
		ttl:        ttl,
		failureTTL: failureTTL,
		entries:    make(map[string]stunCacheEntry),
	}
}

// get はキャッシュを引く。ok=true のとき ip=nil は「失敗を記憶している」の意。
func (c *stunCache) get(key string) (ip net.IP, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if !found || time.Now().After(e.expires) {
		return nil, false
	}
	return e.ip, true
}

// put は問い合わせ結果を記録する（ip=nil は失敗として短い TTL で保持）。
func (c *stunCache) put(key string, ip net.IP) {
	ttl := c.ttl
	if ip == nil {
		ttl = c.failureTTL
	}
	c.mu.Lock()
	c.entries[key] = stunCacheEntry{ip: ip, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// stunMappedIPs はプロセス全体で共有するキャッシュ（queryStunMappedIP が使う）。
var stunMappedIPs = newSTUNCache(stunCacheTTL, stunCacheFailureTTL)
