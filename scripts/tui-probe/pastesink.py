#!/usr/bin/env python3
"""ペースト計測の受け側。raw モードで終端 'Z' まで読み、所要時間を出す。

PASTE_MODE=sink は読み捨て、echo は読んだものをそのまま出力に返す（シェルのエコー相当）。
zellij のペインのシェルとしても使えるよう、モードは引数でなく環境変数で受ける。
"""
import os
import sys
import time
import tty

echo = os.environ.get("PASTE_MODE", "sink") == "echo"
tty.setraw(0)
os.write(1, b"READY\r\n")
n, t0 = 0, None
while True:
    d = os.read(0, 65536)
    if not d:
        break
    if t0 is None:
        t0 = time.time()
    if echo:
        os.write(1, d)
    n += len(d)
    if b"Z" in d:
        break
os.write(1, b"\r\nDONE n=%d recv=%.3fs\r\n" % (n, time.time() - t0))
time.sleep(1000)
