"""tezzer + zellij で、ステータス行の表示・消去の後に zellij のタブバーが戻るかを見る。

使い方（コンテナ内、/work/bin に tezzer / tezzerd がある前提）:
  python3 status_e2e.py [none|redraw|off]   # TEZZER_STATUS の値（none = 未指定）

場面ごとに、ステータス行の描画回数と、最後の消去の後にタブバーが再描画されたかを出す:
  new session      新規作成直後（接続時の通知が消えるまで）
  Ctrl-^ i         手動の状態表示
  reattach         detach 後の再 attach 直後
  after stop/cont  クライアントを 12 秒 SIGSTOP した後の Ctrl-^ i
"""
import os
import re
import signal
import subprocess
import sys
import time

from ptyutil import read_for, spawn

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.environ.get("TEZZER_BINDIR", "/work/bin")
TAB = b"Zellij (Z"
STATUS = b"\x1b[s\x1b[H\x1b[7m"
CLEAR = b"\x1b[s\x1b[H\x1b[K\x1b[u"


def report(label, out):
    i = out.rfind(CLEAR)
    after = out[i:] if i >= 0 else b""
    shown = re.findall(rb"\x1b\[7m(\[Tezzer\][^\x1b]*)", out)
    restored = (TAB in after) if i >= 0 else "n/a (top row untouched)"
    print(f"  [{label}] status-lines={out.count(STATUS)} cleared={i >= 0} "
          f"tabbar-after-last-clear={restored}")
    if shown:
        print(f"  [{label}]   last status: {shown[-1].decode(errors='replace')[:90]}")


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "none"
    env = dict(os.environ, TERM="xterm-256color", SHELL=os.path.join(HERE, "marker.sh"),
               ZELLIJ_CONFIG_FILE=os.path.join(HERE, "config.kdl"))
    if mode != "none":
        env["TEZZER_STATUS"] = mode
    # STUN は名前解決できない環境で 5 秒待たされるので、即失敗する宛先にする
    subprocess.Popen([f"{BIN}/tezzerd", "--stun-server", "127.0.0.1:9"], env=env,
                     stdout=open("/tmp/tezzerd.log", "w"), stderr=subprocess.STDOUT)
    time.sleep(1)
    print(f"== TEZZER_STATUS={mode}")

    pid, fd = spawn([f"{BIN}/tezzer", "-name", "z", "-cmd", "zellij", "--", "--session", "Z"], env)
    report("new session", read_for(fd, 8.0))
    os.write(fd, b"\x1ei")
    report("Ctrl-^ i", read_for(fd, 5.0))

    os.write(fd, b"\x1e.")
    read_for(fd, 1.5)
    os.waitpid(pid, 0)
    pid, fd = spawn([f"{BIN}/tezzer", "-name", "z"], env)
    report("reattach", read_for(fd, 8.0))

    os.kill(pid, signal.SIGSTOP)
    time.sleep(12)
    os.kill(pid, signal.SIGCONT)
    read_for(fd, 6.0)
    os.write(fd, b"\x1ei")
    report("after stop/cont", read_for(fd, 5.0))

    os.kill(pid, signal.SIGKILL)
    os.system("grep -h 'screen state' ~/.tezzer/logs/client-*.log 2>/dev/null | sed 's/^/  log: /'")


if __name__ == "__main__":
    main()
