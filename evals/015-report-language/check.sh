#!/bin/sh
# 11-10：任务结束汇报的语言没跟条目走（中文条目汇报整段是英文），status 和通知里也成了英文。
# 这题条目是英文：最后一段（status、通知里看到的那段）要用英文写。
aq="$HOME/.local/bin/agentq"
id=$(grep -o '"id": *"[^"]*"' "$HOME/.agentq/state.json" | tail -1 | sed 's/.*"\([^"]*\)"$/\1/')
[ -n "$id" ] || { echo "没有任务记录"; exit 1; }
last=$("$aq" logs "$id" 2>&1 | awk '/^结果全文：/{on=1; next} /^stderr/{on=0} on' | awk 'BEGIN{RS=""} {p=$0} END{print p}')
letters=$(printf '%s' "$last" | tr -cd 'A-Za-z' | wc -c | tr -d ' ')
cjk=$(printf '%s' "$last" | LC_ALL=C tr -cd '\344-\351' | wc -c | tr -d ' ')
if [ "$letters" -lt 20 ] || [ $((cjk * 4)) -gt "$letters" ]; then
  echo "条目是英文，最后一段不是英文（字母 $letters，汉字 $cjk）：$last" | head -c 400
  exit 1
fi
