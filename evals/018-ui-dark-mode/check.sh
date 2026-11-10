#!/bin/sh
# 在 main 的干净副本上看：暗色规则真的写了、写在 CSS 里，页面没加脚本，内容没丢。
grep -Eq 'prefers-color-scheme:[[:space:]]*dark' index.html *.css 2>/dev/null || { echo "没有 prefers-color-scheme: dark"; exit 1; }
if grep -qi '<script' index.html; then echo "加了脚本"; exit 1; fi
exec sh "$EVAL_CASE/_project/.agentq/verify"
