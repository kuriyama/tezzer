# tui-probe: tmux / screen / zellij を入れた測定用イメージ（scripts/tui-probe.sh から使う）
FROM debian:12
RUN apt-get update \
 && apt-get install -y --no-install-recommends tmux screen python3 curl ca-certificates iproute2 \
 && rm -rf /var/lib/apt/lists/*
ARG ZELLIJ_VERSION=0.44.3
RUN curl -fsSL "https://github.com/zellij-org/zellij/releases/download/v${ZELLIJ_VERSION}/zellij-x86_64-unknown-linux-musl.tar.gz" \
  | tar xz -C /usr/local/bin
