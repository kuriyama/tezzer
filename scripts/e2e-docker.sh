#!/usr/bin/env bash
# scripts/e2e-docker.sh
#
# Docker シナリオ E2E（スリープ復帰）のドライバ。手動テスト用。
# 静的バイナリ（tezzerd / tezzer）と e2e_docker テストバイナリを host でビルドし、
# 最小コンテナ（alpine）内で実行する。コンテナ内では Go ツールチェーン不要
# （TEZZER_E2E_BINDIR で事前ビルド済みバイナリを参照）。
#
# 手動専用。1 シナリオ（SIGSTOP/CONT スリープ復帰）で 40〜50 秒かかる。
# DOCKER 変数で docker コマンドを上書き可能（既定: docker。rootless の udocker 等を
# 使う場合は `DOCKER=udocker ./scripts/e2e-docker.sh`）。
set -euo pipefail

cd "$(dirname "$0")/.."

DOCKER="${DOCKER:-docker}"
IMAGE="${E2E_IMAGE:-alpine:3.20}"

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

echo ">> building static binaries + test binary into $TMP"
CGO_ENABLED=0 go build -o "$TMP/tezzerd" ./cmd/tezzerd
CGO_ENABLED=0 go build -o "$TMP/tezzer" ./cmd/tezzer
CGO_ENABLED=0 go test -c -tags e2e_docker -o "$TMP/e2e.test" ./internal/e2e
chmod +x "$TMP"/tezzerd "$TMP"/tezzer "$TMP"/e2e.test

echo ">> ensuring image $IMAGE is available"
"$DOCKER" pull "$IMAGE" >/dev/null 2>&1 || true

echo ">> running sleep-recovery scenario in container ($DOCKER, $IMAGE)"
"$DOCKER" run --rm \
	-v "$TMP:/work" \
	-e TEZZER_E2E_BINDIR=/work \
	"$IMAGE" \
	/work/e2e.test -test.run TestE2EDockerSleepRecovery -test.v -test.timeout 5m
