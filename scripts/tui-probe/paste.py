"""巨大ペーストの所要時間を、直接 PTY と tezzer 経由で比べる。

使い方（コンテナ内、/work/bin に tezzer / tezzerd がある前提）:
  python3 paste.py [--rtt=MS] [--loss=PCT] [KB ...]   # ペーストサイズ（KB、既定 256 1024）

--rtt を付けると lo に tc netem で片道 MS/2 の遅延を入れる（要 NET_ADMIN）。
tezzer の QUIC は lo を通るので RTT が MS になる。UDS と direct の場面は影響を受けない。
--loss は同じく lo で片道 PCT% のパケットロスを入れる。

受け側は pastesink.py。場面:
  sink    raw モードで読み捨て（入力経路のみ）
  echo    読んだものをそのまま返す（入力 + 出力経路。シェルへのペースト相当）
  zellij  zellij のペイン内で echo（bracketed paste で包む。WezTerm > zellij 相当）
ペーストは端末と同様に 1 回の write で渡す（PTY が受け付けた分ずつ書き進める）。
時間は書き込み開始から DONE 表示まで。
"""
import os
import re
import select
import subprocess
import sys
import time

from ptyutil import read_for, spawn

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.environ.get("TEZZER_BINDIR", "/work/bin")
SINK = os.path.join(HERE, "pastesink.py")
# zellij は画面を描き直すので DONE 行の途中にも CSI が挟まる。判定前に取り除く
CSI = re.compile(rb"\x1b\[[0-9;?]*[ -/]*[@-~]")
DONE = re.compile(rb"DONE n=\d+ recv=[0-9.]+s")
seq = 0


def payload(kb):
    # 80 桁の行を \r 区切りで（端末がペーストを送る形）。ESC と Ctrl-^ を含めない
    line = (b"abcdefghijklmnopqrstuvwxyz0123456789" * 3)[:79] + b"\r"
    data = (line * (kb * 1024 // len(line) + 1))[: kb * 1024 - 1]
    return data + b"Z"


def wait_for(fd, mark, limit):
    buf, end = b"", time.time() + limit
    while time.time() < end and mark not in buf:
        r, _, _ = select.select([fd], [], [], 0.1)
        if r:
            try:
                buf += os.read(fd, 65536)
            except OSError:
                break
    return mark in buf


def paste(fd, data, limit=120):
    """data を書きつつ出力を読み、DONE 行が出るまでの秒数と DONE 行を返す。"""
    os.set_blocking(fd, False)
    t0, off, out = time.time(), 0, b""
    while time.time() - t0 < limit:
        w = [fd] if off < len(data) else []
        r, w, _ = select.select([fd], w, [], 0.1)
        if r:
            try:
                out += os.read(fd, 65536)
            except (BlockingIOError, OSError):
                pass
            m = DONE.search(CSI.sub(b"", out))
            if m:
                return time.time() - t0, m.group(0).decode()
            out = out[-8192:]
        if w:
            try:
                off += os.write(fd, data[off:off + 65536])
            except BlockingIOError:
                pass
    return None, f"timeout (wrote {off}/{len(data)})"


def run(label, argv, env, data):
    global seq
    seq += 1
    pid, fd = spawn(argv, env)
    if not wait_for(fd, b"READY", 15):
        print(f"  {label:<22} failed: READY not seen")
    else:
        read_for(fd, 0.5)
        secs, line = paste(fd, data)
        mbps = f"{len(data) / secs / 1e6:6.2f} MB/s" if secs else "-"
        print(f"  {label:<22} {secs or 0:7.2f}s {mbps}  {line}")
    os.kill(pid, 9)
    os.waitpid(pid, 0)
    # zellij はサーバーが残るので次の計測に影響しないよう片付ける
    subprocess.run(["zellij", "kill-all-sessions", "-y"], env=env,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def tezzer(argv):
    # セッションの環境は tezzerd 側から引き継がれるので、受け側の設定は env で渡す
    return [f"{BIN}/tezzer", "-cmd", argv[0], "--"] + argv[1:]


def main():
    rtt, loss = 0, 0.0
    sizes = []
    for a in sys.argv[1:]:
        if a.startswith("--rtt="):
            rtt = int(a[len("--rtt="):])
        elif a.startswith("--loss="):
            loss = float(a[len("--loss="):])
        else:
            sizes.append(int(a))
    sizes = sizes or [256, 1024]
    if rtt or loss:
        netem = ["delay", f"{rtt / 2}ms"] + (["loss", f"{loss}%"] if loss else [])
        subprocess.run(["tc", "qdisc", "add", "dev", "lo", "root", "netem"] + netem, check=True)
        print(f"== RTT {rtt} ms, loss {loss}% (netem on lo)")
    env = dict(os.environ, TERM="xterm-256color",
               ZELLIJ_CONFIG_FILE=os.path.join(HERE, "config.kdl"))
    subprocess.Popen([f"{BIN}/tezzerd", "--stun-server", "127.0.0.1:9"], env=env,
                     stdout=open("/tmp/tezzerd.log", "w"), stderr=subprocess.STDOUT)
    time.sleep(1)
    for kb in sizes:
        data = payload(kb)
        bp = b"\x1b[200~" + data + b"\x1b[201~"
        print(f"== {kb} KB")
        for mode in ("sink", "echo"):
            sink = ["env", f"PASTE_MODE={mode}", SINK]
            run(f"direct  {mode}", sink, env, data)
            run(f"tezzer  {mode}", tezzer(sink), env, data)
        zellij = ["env", "PASTE_MODE=echo", f"SHELL={SINK}", "zellij", "--session", f"p{seq}"]
        run("direct  zellij", zellij, env, bp)
        run("tezzer  zellij", tezzer(zellij), env, bp)


if __name__ == "__main__":
    main()
