#!/bin/sh
# 在 main 的干净副本上看：文档里不再有 count.sh 不认的参数，写着的每条 count.sh 命令都能跑。
if grep -Eq -- '--lines|--words' docs/usage.md; then echo "docs/usage.md 还写着 count.sh 不认的参数"; exit 1; fi
grep -q -- 'count.sh -l' docs/usage.md && grep -q -- 'count.sh -w' docs/usage.md || { echo "docs/usage.md 没写对 -l、-w"; exit 1; }
grep -oE 'count\.sh +-[a-z-]+' docs/usage.md | while read -r _ flag; do
  printf 'a b\n' | sh count.sh "$flag" >/dev/null 2>&1 || { echo "文档里的 count.sh $flag 跑不通"; exit 1; }
done
