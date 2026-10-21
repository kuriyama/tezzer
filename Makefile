GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S_UTC')
GIT_REVCOUNT := $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
# deb/rpm 用のパッケージバージョン。タグ運用を始めるまでは 0.0.0 ベースに、
# 単調増加するコミット数（apt/rpm のアップグレード判定に必要）とハッシュを付ける。
DIST_VERSION ?= 0.0.0+r$(GIT_REVCOUNT).$(GIT_COMMIT)
# バイナリに埋め込む表示用バージョン（-version）。リリース CI は make dist DIST_VERSION=x.y.z
# で呼ぶのでそれを使う。それ以外（開発ビルド）は最新リリースタグ + "+dev"（例: 0.1.1+dev）。
# リリースタグは publish.sh で書き出した公開ツリー側の commit に付くため develop の祖先に
# ならず git describe が使えない。正確な位置は併記する commit hash で示す。
LATEST_TAG := $(shell git tag --list 'v*' --sort=-v:refname 2>/dev/null | head -1 | sed 's/^v//')
ifneq ($(filter command line environment,$(origin DIST_VERSION)),)
VERSION ?= $(DIST_VERSION)
else
VERSION ?= $(if $(LATEST_TAG),$(LATEST_TAG)+dev,dev)
endif
# 未 commit の変更を含むビルドは commit hash に -dirty を付ける（-version 表示用）
GIT_DIRTY := $(shell git diff --quiet HEAD -- 2>/dev/null || echo -dirty)
# -s -w: シンボルテーブルと DWARF デバッグ情報を除去（バイナリを約 1/3 削減）。
# デバッグビルドが必要な場合は make build STRIP= で無効化できる。
STRIP := -s -w
# -X の対象パスは go.mod から取る。publish.sh が公開ツリーの module パスを書き換えるため、
# 固定で書くと公開ツリーでは存在しないシンボル指定になり、go build は黙って無視する。
MODULE := $(shell go list -m)
LDFLAGS := $(STRIP) -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT)$(GIT_DIRTY) -X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: build dist test test-race vet fmt fmt-check ci ci-nightly e2e e2e-docker test-chaos deadcode dupl clean install

build:
	go build -ldflags "$(LDFLAGS)" -o bin/tezzerd ./cmd/tezzerd
	go build -ldflags "$(LDFLAGS)" -o bin/tezzer ./cmd/tezzer

# dist は配布用のクロスコンパイル済み tarball を dist/ に生成する（CI artifact 向け）。
# 依存はすべて pure Go なので CGO_ENABLED=0 で全プラットフォームをビルドできる。
# ファイル名にバージョンを入れない（CI の「ref の最新 artifact」固定 URL を安定させるため。
# バージョン情報はバイナリ自体に LDFLAGS で埋め込まれる）。
define dist_build
	mkdir -p dist/tezzer_$(1)_$(2)
	CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) go build -ldflags "$(LDFLAGS)" -o dist/tezzer_$(1)_$(2)/tezzerd ./cmd/tezzerd
	CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) go build -ldflags "$(LDFLAGS)" -o dist/tezzer_$(1)_$(2)/tezzer ./cmd/tezzer
	install -m 0755 scripts/tezzer-ssh dist/tezzer_$(1)_$(2)/
	chmod 0755 dist/tezzer_$(1)_$(2)/tezzerd dist/tezzer_$(1)_$(2)/tezzer
	tar -C dist -czf dist/tezzer_$(1)_$(2).tar.gz tezzer_$(1)_$(2)
endef

# deb / rpm を nfpm で生成する（linux のみ）。nfpm は contents.src の環境変数を
# 展開しないため、対象アーキテクチャのバイナリを dist/.nfpm/ に集めてから呼ぶ。
define dist_pkg
	rm -rf dist/.nfpm && mkdir -p dist/.nfpm
	cp dist/tezzer_linux_$(1)/tezzer dist/tezzer_linux_$(1)/tezzerd dist/.nfpm/
	GOARCH=$(1) DIST_VERSION=$(DIST_VERSION) nfpm package -f nfpm.yaml -p deb --target dist/
	GOARCH=$(1) DIST_VERSION=$(DIST_VERSION) nfpm package -f nfpm.yaml -p rpm --target dist/
endef

dist:
	rm -rf dist
	$(call dist_build,linux,amd64)
	$(call dist_build,linux,arm64)
	$(call dist_build,darwin,amd64)
	$(call dist_build,darwin,arm64)
	$(call dist_build,freebsd,amd64)
	$(call dist_build,freebsd,arm64)
ifneq ($(shell command -v nfpm),)
	$(call dist_pkg,amd64)
	$(call dist_pkg,arm64)
else
	@echo "nfpm が見つからないため deb/rpm 生成をスキップ" \
		"(go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.46.3)"
endif
	rm -rf dist/.nfpm dist/tezzer_linux_amd64 dist/tezzer_linux_arm64 dist/tezzer_darwin_amd64 dist/tezzer_darwin_arm64 dist/tezzer_freebsd_amd64 dist/tezzer_freebsd_arm64
	cd dist && sha256sum tezzer* > SHA256SUMS

test:
	go test ./... 2>&1 | tee /tmp/make_test_output.txt

test-race:
	go test -race ./... 2>&1 | tee /tmp/make_test_race_output.txt

vet:
	go vet ./...

# e2e は実バイナリ + pty の最小 E2E スモーク（Phase 5c）。build tag e2e 付きで、
# make test / make ci には含めない（実時間・実 PTY 依存でフレークしうるため）。
# バイナリはテスト内でビルドされる。
e2e:
	go test -tags e2e -count=1 ./internal/e2e/ -v

# e2e-docker は rootless docker でスリープ復帰（SIGSTOP/CONT）を実時間・実カーネルで
# 確認する手動シナリオ（Phase 5d）。synctest の盲点（実ソケット rebind・スリープ検出）の
# 保険。1 シナリオ 40〜50 秒。CI には入れない。DOCKER 変数で docker コマンドを上書き可能。
e2e-docker:
	./scripts/e2e-docker.sh

# test-chaos は qtransport（quic-go）を実 UDP ソケット + chaosRelay（実プロキシ、
# internal/qtransport/chaos_relay_test.go）経由で動かし、ランダムな loss/dup/delay 下で
# 破綻しないことを実時間で検証する。quic-go は synctest 非対応（実タイマー）のため
# 仮想時間化できない。数秒で終わるが、build tag chaos 付きで CI には入れない。
# TEZZER_TEST_SEED で単一 seed を再現できる。
test-chaos:
	go test -tags chaos -count=1 -timeout 1m -v ./internal/qtransport/

fmt:
	gofmt -w .

# fmt-check は gofmt 差分があれば失敗する（CI 用、書き換えはしない）
fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt が必要なファイル:"; echo "$$out"; \
		echo "make fmt を実行してください"; \
		exit 1; \
	fi

# ci は毎プッシュ向けの高速チェック: gofmt 差分 + vet + 短縮テスト。
# -short で sleep_spike_test.go（実スリープ）・pty_integration_test.go（実PTY起動）を
# スキップする。internal/stun はループバックの最小STUNサーバーを使うため -short の
# 有無に関わらず常に実行される（外部ネットワーク不要、隔離環境でも緑になる）。
ci: fmt-check vet
	go test -short ./...

# ci-nightly は定期実行向けの重いチェック: race 付き全テスト（-short を外すので
# sleep_spike/pty_integration も実行される）+ カオステスト。いずれも外部ネットワーク
# 不要で、実時間依存は数秒〜十数秒程度に収まる。
ci-nightly: fmt-check vet
	go test -race ./...
	go test -tags chaos -race -count=1 ./internal/qtransport/

# deadcode は golang.org/x/tools/cmd/deadcode（Rapid Type Analysis で main から
# 到達不能な関数を検出）による未使用コードの手動チェック。cmd/tezzer と cmd/tezzerd
# は internal/qtransport 等を非対称に使う（クライアント専用/サーバー専用の関数がある）
# ため、両方の main パッケージをまとめて渡さないと片方専用のコードが誤検出される。
# -test でテストからのみ使う関数（atomicDuration.Set 等、テスト用の差し込み口）も
# reachable 扱いにする。go install はせず go run のバージョン指定実行にして
# go.mod を汚さない。ネットワーク（初回のみ）が要るため CI には組み込まず手動実行。
deadcode:
	go run golang.org/x/tools/cmd/deadcode@v0.36.0 -test ./cmd/tezzer/... ./cmd/tezzerd/... ./internal/...

# dupl はトークン列ベースのクローン検出（github.com/mibk/dupl）による重複コードの
# 手動チェック。*_test.go は除外する（テーブル駆動テストの定型的な繰り返しでノイズが
# 多いため）。閾値 50 トークンは「関数まるごと似ている」レベルの塊を拾う目安（既定
# 100 だと短めの重複を見逃す）。GOOS 別ファイル（*_linux.go/*_bsd.go 等の対）は
# 構造上似て当然の誤検出として出るので、指摘ごとに実際に統合すべきか判断すること。
# ネットワーク（初回のみ）が要るため CI には組み込まず手動実行。
dupl:
	go run github.com/mibk/dupl@v1.1.0 -t 50 $(shell find . -name "*.go" -not -name "*_test.go" -not -path "./.git/*")

clean:
	rm -rf bin/ dist/

install:
	go install ./cmd/tezzerd
	go install ./cmd/tezzer
