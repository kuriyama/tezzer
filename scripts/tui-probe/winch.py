"""サイズ不変の SIGWINCH で multiplexer が再描画するかを、resize trick と比較する。

  A: 同サイズで TIOCSWINSZ（カーネルはサイズ不変なら SIGWINCH を送らない。ベースライン）
  B: サイズ不変のまま前景プロセスグループへ kill(SIGWINCH)（2 回）
  C: tezzer の resize trick（cols-1 -> cols）

画面内の MARKER とステータスバーの文字列が出力に再登場したら「再描画された」とみなす。
使い方（コンテナ内）: python3 winch.py [tmux|tmux-top|screen|zellij ...]
同じコンテナで zellij を 2 回起動するとセッション名が衝突するので 1 コンテナ 1 回。
"""
import os
import signal
import sys
import time

from ptyutil import COLS, ROWS, drain, setsize, spawn

HERE = os.path.dirname(os.path.abspath(__file__))
MARK = b"MARKER"
CLEARS = (b"\x1b[2J", b"\x1b[H\x1b[J")


def summary(label, out, status_mark):
    clear = any(c in out for c in CLEARS)
    status = (status_mark in out) if status_mark else "-"
    print(f"  {label:<34} bytes={len(out):6d} marker={MARK in out!s:5} "
          f"status={status!s:5} clear={clear}")


def run(name, argv, status_mark, env):
    pid, fd = spawn(argv, env)
    init = drain(fd, quiet=3.0, limit=15.0)
    time.sleep(3)  # zellij は初期描画が遅れて届くことがあるので待ってから測る
    init += drain(fd, quiet=2.0)
    print(f"{name}: initial draw {len(init)} bytes, marker={MARK in init}")

    setsize(fd, ROWS, COLS)
    summary("A same-size TIOCSWINSZ", drain(fd), status_mark)

    pgrp = os.tcgetpgrp(fd)
    os.kill(-pgrp, signal.SIGWINCH)
    summary("B kill(-pgrp, SIGWINCH)", drain(fd), status_mark)
    os.kill(-pgrp, signal.SIGWINCH)
    summary("B2 kill SIGWINCH again", drain(fd), status_mark)

    setsize(fd, ROWS, COLS - 1)
    time.sleep(0.05)
    setsize(fd, ROWS, COLS)
    summary("C resize trick (cols-1 -> cols)", drain(fd), status_mark)

    os.kill(pid, signal.SIGKILL)
    os.waitpid(pid, 0)


def main():
    env = dict(os.environ, TERM="xterm-256color")
    inner = "printf 'MARKER\\n'; exec sleep 1000"
    which = sys.argv[1:] or ["tmux", "tmux-top", "screen", "zellij"]
    if "tmux" in which:
        run("tmux " + os.popen("tmux -V").read().strip(),
            ["tmux", "-L", "bottom", "-f", "/dev/null", "new-session", "-s", "PROBE",
             "sh", "-c", inner], b"PROBE", env)
    if "tmux-top" in which:
        run("tmux (status-position top)",
            ["tmux", "-L", "top", "-f", "/dev/null", "new-session", "-s", "PROBE",
             "sh", "-c", inner, ";", "set", "-g", "status-position", "top"], b"PROBE", env)
    if "screen" in which:
        run("screen", ["screen", "-c", "/dev/null", "sh", "-c", inner], None, env)
    if "zellij" in which:
        zenv = dict(env, SHELL=os.path.join(HERE, "marker.sh"))
        run("zellij " + os.popen("zellij --version").read().strip(),
            ["zellij", "--config", os.path.join(HERE, "config.kdl"), "--session", "PROBE"],
            b"PROBE", zenv)


if __name__ == "__main__":
    main()
