#!/bin/sh
# 在 main 的干净副本上看：有「用法」一节，两个参数都讲了，例子真能跑。
grep -Eq '^#+[[:space:]]*用法' README.md || { echo "README.md 没有「用法」一节"; exit 1; }
for f in -l -w; do
  line=$(grep -E "count\.sh $f" README.md | head -1)
  [ -n "$line" ] || { echo "README.md 没给 $f 的例子"; exit 1; }
done
# 例子要能直接跑：反引号片段和代码块里带 count.sh 的命令逐条照抄执行，有一条跑通就算给了能用的例子
# （参数表里的 `count.sh -l` 只是名字，跑不通不算错）
for f in -l -w; do
  ok=""
  tried=""
  cands=$( { grep -oE '`[^`]*count\.sh '"$f"'[^`]*`' README.md | tr -d '`'; grep -E "count\.sh $f" README.md | grep -v '`' | sed 's/^[[:space:]]*\$[[:space:]]*//'; } )
  while IFS= read -r cmd; do
    [ -n "$cmd" ] || continue
    if out=$(sh -c "$cmd" </dev/null 2>&1) && [ -n "$out" ]; then ok=1; break; fi
    tried="$tried
  $cmd → $out"
  done <<CANDS
$cands
CANDS
  [ -n "$ok" ] || { echo "$f 的例子没有一条照抄跑得通：$tried"; exit 1; }
done
