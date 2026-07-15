// Package qtransport は QUIC ベースのトランスポート実装。
//
// 認証は PKI を使わず、SSH 経由 UDS bootstrap で配送される共有鍵 K に紐づく
// self-signed 証明書を mTLS で相互 pinning する（Spike A で feasibility 実証済み、
// docs/dev/quic-transport-feasibility.md §4 参照）。
package qtransport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"time"

	"golang.org/x/crypto/hkdf"
)

// ALPN は tezzer QUIC のアプリケーションプロトコル識別子。
const ALPN = "github.com/kuriyama/tezzer/1"

const (
	labelServerIdentity = "tezzer server identity"
	labelClientIdentity = "tezzer client identity"
)

// deriveIdentity は共有鍵 K と用途ラベルから決定的に Ed25519 鍵を導出する。
// 両端が同じ K と同じラベルから同一の鍵を再現できる。
func deriveIdentity(k []byte, label string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	r := hkdf.New(sha256.New, k, nil, []byte(label))
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(r, seed); err != nil {
		return nil, nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv, nil
}

// selfSignedCert は pub/priv を載せた自己署名証明書（PKI なし）を作る。
func selfSignedCert(pub ed25519.PublicKey, priv ed25519.PrivateKey) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tezzer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// pinVerify は相手証明書の公開鍵が expected と一致するかだけを検証する
// （CA チェーンは見ない＝PKI 不要、共有鍵由来の鍵で peer を認証）。
func pinVerify(expected ed25519.PublicKey) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("no peer certificate")
		}
		c, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		got, ok := c.PublicKey.(ed25519.PublicKey)
		if !ok {
			return errors.New("peer key is not ed25519")
		}
		if !got.Equal(expected) {
			return errors.New("peer public key does not match shared-key-derived identity")
		}
		return nil
	}
}

// tlsConfig は own/expectPeer を分けて mTLS 設定を作る内部ヘルパー。
// （通常は own==expectPeer==K。異なる K を渡すと相互認証に失敗する＝テストで利用。）
func tlsConfig(isServer bool, ownK, expectPeerK []byte) (*tls.Config, error) {
	ownLabel, peerLabel := labelClientIdentity, labelServerIdentity
	if isServer {
		ownLabel, peerLabel = labelServerIdentity, labelClientIdentity
	}
	ownPub, ownPriv, err := deriveIdentity(ownK, ownLabel)
	if err != nil {
		return nil, err
	}
	cert, err := selfSignedCert(ownPub, ownPriv)
	if err != nil {
		return nil, err
	}
	peerPub, _, err := deriveIdentity(expectPeerK, peerLabel)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates:          []tls.Certificate{cert},
		VerifyPeerCertificate: pinVerify(peerPub),
		NextProtos:            []string{ALPN},
		MinVersion:            tls.VersionTLS13,
	}
	if isServer {
		cfg.ClientAuth = tls.RequireAnyClientCert
	} else {
		cfg.InsecureSkipVerify = true // CA 検証を無効化し pinVerify で pinning
	}
	return cfg, nil
}

// ServerTLS は共有鍵 K からサーバ側 mTLS 設定を作る。
func ServerTLS(k []byte) (*tls.Config, error) { return tlsConfig(true, k, k) }

// ClientTLS は共有鍵 K からクライアント側 mTLS 設定を作る。
func ClientTLS(k []byte) (*tls.Config, error) { return tlsConfig(false, k, k) }
