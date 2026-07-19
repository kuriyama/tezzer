GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S_UTC')
GIT_REVCOUNT := $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
# deb/rpm 用のパッケージバージョン。タグ運用を始めるまでは 0.0.0 ベースに、
# 単調増加するコミット数（apt/rpm のアップグレード判定に必要）とハッシュを付ける。
DIST_VERSION ?= 0.0.0+r$(GIT_REVCOUNT).$(GIT_COMMIT)
# -s -w: シンボルテーブルと DWARF デバッグ情報を除去（バイナリを約 1/3 削減）。
# デバッグビルドが必要な場合は make build STRIP= で無効化できる。
STRIP := -s -w
LDFLAGS := $(STRIP) -X tezzer/internal/version.GitCommit=$(GIT_COMMIT) -X tezzer/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: build dist test test-race vet fmt fmt-check ci ci-nightly e2e e2e-docker clean install

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
# -short で internal/stun の実 STUN ネットワークテストをスキップする（隔離環境でも緑になる）。
ci: fmt-check vet
	go test -short ./...

# ci-nightly は定期実行向けの重いチェック: race 付き全テスト。
ci-nightly: fmt-check vet
	go test -race -short ./...

clean:
	rm -rf bin/ dist/

install:
	go install ./cmd/tezzerd
	go install ./cmd/tezzer
