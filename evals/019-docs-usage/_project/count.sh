#!/bin/sh
# 数标准输入的行数或单词数。
case "$1" in
  -l) wc -l | tr -d ' ' ;;
  -w) wc -w | tr -d ' ' ;;
  *) echo "用法：count.sh -l|-w < 文件" >&2; exit 2 ;;
esac
