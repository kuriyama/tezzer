#!/usr/bin/env bash
# scripts/tui-probe.sh
#
# ステータス行と再描画まわりの実測用ドライバ（手動専用、make ci には含めない）。
# tmux / screen / zellij を入れたイメージ（scripts/tui-probe/Dockerfile）をビルドし、
# コンテナ内で測定スクリプトを動かす。
#
#   scripts/tui-probe.sh winch [tmux|tmux-top|screen|zellij ...]
#       サイズ不変の SIGWINCH と resize trick で各 multiplexer が再描画するかを比較
#   scripts/tui-probe.sh status [none|redraw|off]
#       tezzer + zellij で、ステータス行の消去後にタブバーが戻るか（TEZZER_STATUS ごと）
#   scripts/tui-probe.sh paste [--rtt=MS] [--loss=PCT] [KB ...]
#       巨大ペーストの所要時間を直接 PTY と tezzer 経由（素 / エコーあり / zellij 内）で比較
#       --rtt / --loss で tc netem による遅延・ロスを入れる（コンテナに NET_ADMIN を付ける）
#
# DOCKER 変数で docker コマンドを上書き可能（例: DOCKER=udocker scripts/tui-probe.sh winch）。
# イメージのビルドで apt と GitHub Releases（zellij）にアクセスする。
set -euo pipefail

cd "$(dirname "$0")/.."

DOCKER="${DOCKER:-docker}"
IMAGE="${TUI_PROBE_IMAGE:-tezzer-tui-probe}"
probe="${1:-}"
shift || true

case "$probe" in
	winch | status | paste) ;;
	*)
		sed -n '4,16p' "$0" | sed 's/^# \{0,1\}//' >&2
		exit 1
		;;
esac

build_args=()
for v in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do
	if [ -n "${!v:-}" ]; then
		build_args+=(--build-arg "$v=${!v}")
	fi
done

echo ">> building image $IMAGE" >&2
"$DOCKER" build --network=host "${build_args[@]}" -t "$IMAGE" scripts/tui-probe >&2

if [ "$probe" = winch ]; then
	exec "$DOCKER" run --rm -v "$PWD/scripts/tui-probe:/probe:ro" "$IMAGE" \
		python3 -u /probe/winch.py "$@"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo ">> building static binaries into $TMP" >&2
CGO_ENABLED=0 go build -o "$TMP/tezzerd" ./cmd/tezzerd
CGO_ENABLED=0 go build -o "$TMP/tezzer" ./cmd/tezzer
script=status_e2e.py
caps=()
if [ "$probe" = paste ]; then
	script=paste.py
	caps=(--cap-add NET_ADMIN)
fi
"$DOCKER" run --rm "${caps[@]}" -v "$PWD/scripts/tui-probe:/probe:ro" -v "$TMP:/work/bin:ro" "$IMAGE" \
	python3 -u "/probe/$script" "$@"
