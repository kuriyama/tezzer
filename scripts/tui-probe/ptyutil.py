"""tui-probe 共通: PTY で子プロセスを起動し、出力を時間で区切って読む。"""
import fcntl
import os
import pty
import select
import struct
import termios
import time

ROWS, COLS = 30, 100


def setsize(fd, rows=ROWS, cols=COLS):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def spawn(argv, env):
    """argv を ROWSxCOLS の PTY で起動し (pid, master fd) を返す。"""
    pid, fd = pty.fork()
    if pid == 0:
        setsize(0)
        os.execvpe(argv[0], argv, env)
    setsize(fd)
    return pid, fd


def read_for(fd, secs):
    """secs 秒間、届いた出力をすべて読む。"""
    buf, end = b"", time.time() + secs
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.05)
        if r:
            try:
                d = os.read(fd, 65536)
            except OSError:
                break
            if not d:
                break
            buf += d
    return buf


def drain(fd, quiet=1.5, limit=8.0):
    """出力が quiet 秒途切れるまで（最長 limit 秒）読む。"""
    buf, end, last = b"", time.time() + limit, time.time()
    while time.time() < end and time.time() - last < quiet:
        r, _, _ = select.select([fd], [], [], 0.1)
        if r:
            try:
                d = os.read(fd, 65536)
            except OSError:
                break
            if not d:
                break
            buf += d
            last = time.time()
    return buf
