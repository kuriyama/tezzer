package main

import (
	"net"
	"testing"
	"time"
)

func TestSTUNCache(t *testing.T) {
	c := newSTUNCache(100*time.Millisecond, 30*time.Millisecond)

	// ミス
	if _, ok := c.get("udp4|s"); ok {
		t.Fatal("empty cache should miss")
	}

	// 成功結果のヒット
	ip := net.IPv4(203, 0, 113, 5)
	c.put("udp4|s", ip)
	got, ok := c.get("udp4|s")
	if !ok || !got.Equal(ip) {
		t.Fatalf("expected hit with %v, got %v (ok=%v)", ip, got, ok)
	}

	// キーが違えばミス（family / STUN サーバーごとに独立）
	if _, ok := c.get("udp6|s"); ok {
		t.Fatal("different key should miss")
	}

	// 失敗のネガティブキャッシュ: ヒットするが ip=nil
	c.put("udp6|s", nil)
	got, ok = c.get("udp6|s")
	if !ok || got != nil {
		t.Fatalf("expected negative-cache hit (nil), got %v (ok=%v)", got, ok)
	}

	// 失敗は短い TTL で期限切れになり、再問い合わせ可能になる
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.get("udp6|s"); ok {
		t.Fatal("negative entry should expire after failureTTL")
	}
	// 成功側はまだ生きている（ttl > failureTTL）
	if _, ok := c.get("udp4|s"); !ok {
		t.Fatal("success entry should still be alive")
	}

	// 成功側も TTL で期限切れ
	time.Sleep(70 * time.Millisecond)
	if _, ok := c.get("udp4|s"); ok {
		t.Fatal("success entry should expire after ttl")
	}

	// 失敗を成功で上書きできる（復旧後の再問い合わせ）
	c.put("udp4|s", nil)
	c.put("udp4|s", ip)
	got, ok = c.get("udp4|s")
	if !ok || !got.Equal(ip) {
		t.Fatalf("expected success to overwrite failure, got %v (ok=%v)", got, ok)
	}
}
