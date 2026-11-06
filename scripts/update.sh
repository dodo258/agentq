#!/bin/sh
# 安装新版本 agentq 的固定步骤（改了 agentq 代码、合并进 main 后，在项目主目录执行 scripts/update.sh）：
#   1. go build 到临时目录；
#   2. 用临时 HOME 跑一次 `agentq status` 和一次空跑 `agentq tick` 自检，任何一步非 0 就不替换；
#   3. 通过才替换：旧的 ~/.local/bin/agentq 留成 agentq.prev，新的原子改名过去，清零 launchd 保护脚本的连续失败计数。
# 用法：scripts/update.sh [源码目录，默认是本脚本上一级]
set -eu

src=${1:-$(cd "$(dirname "$0")/.." && pwd)}
if [ -f "$HOME/.local/share/agentq/receipt.json" ]; then
 exec /bin/sh "$src/scripts/bootstrap.sh" setup --yes
fi
bindir=$HOME/.local/bin
bin=$bindir/agentq
fails=$HOME/.agentq/guard-fails

tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentq-update.XXXXXX")
trap 'rm -rf "$tmp" "$bin.prev.tmp" "$bin.new.tmp"' EXIT

echo "构建：$src → $tmp/agentq"
if ! (cd "$src" && go build -o "$tmp/agentq" .); then
	echo "构建失败，没有替换 $bin"
	exit 1
fi

# 自检用临时 HOME：不读写真实的 ~/.agentq、~/projects、~/.claude。tick 里照常巡逻（读真实的 launchctl list，只读），
# 但不启动 claude -p 分析
mkdir -p "$tmp/home/.agentq"
echo '{"patrol_analyze": false}' >"$tmp/home/.agentq/config.json"
for cmd in status tick; do
	if ! HOME=$tmp/home "$tmp/agentq" $cmd >"$tmp/check.log" 2>&1; then
		echo "自检失败：agentq $cmd 非 0 退出，没有替换 ${bin}。输出："
		cat "$tmp/check.log"
		exit 1
	fi
done
echo "自检通过（临时 HOME 下 agentq status、agentq tick 都返回 0）"

mkdir -p "$bindir"
# 现在装着的版本在 launchd 下已经失败过（计数非空）就不拿它覆盖 agentq.prev，免得回滚时换回一个坏的
if [ -s "$fails" ] && [ -f "$bin.prev" ]; then
	echo "注意：现在的 $bin 在 launchd 下已连续失败 $(cat "$fails") 次，保留原来的 $bin.prev 不覆盖"
elif [ -e "$bin" ]; then
	cp -p "$bin" "$bin.prev.tmp"
	mv -f "$bin.prev.tmp" "$bin.prev"
fi
cp "$tmp/agentq" "$bin.new.tmp"
mv -f "$bin.new.tmp" "$bin"
rm -f "$fails"
echo "已安装 ${bin}（旧版本留在 $bin.prev，launchd 保护脚本连续失败 2 次会自动换回它）"
