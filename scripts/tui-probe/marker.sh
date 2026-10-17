#!/bin/sh
# zellij のペイン内シェルの代わり。再描画で MARKER が出直すかを判定に使う
printf 'MARKER\n'
exec sleep 1000
