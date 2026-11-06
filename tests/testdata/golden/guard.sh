#!/bin/sh
# agentq launchd 保护脚本，由 agentq install 生成，改了会被下次 install 覆盖。
bin='/srv/agentq-home/.local/bin/agentq'
fails='/srv/agentq-home/.agentq/guard-fails'
nightly_dir='/srv/agentq-home/.claude/nightly'

"$bin" tick
code=$?
if [ "$code" -eq 0 ]; then
	rm -f "$fails"
	exit 0
fi
n=$(cat "$fails" 2>/dev/null)
case "$n" in '' | *[!0-9]*) n=0 ;; esac
n=$((n + 1))
echo "$n" >"$fails"
echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：agentq tick 退出码 $code（连续第 $n 次非 0）"
[ "$n" -eq 2 ] || exit "$code"

summary() {
	day=$(date +%Y-%m-%d)
	f="$nightly_dir/$day-agentq.md"
	mkdir -p "$nightly_dir"
	[ -s "$f" ] || printf '# agentq 任务汇总 %s\n\n' "$day" >>"$f"
	printf -- '- %s %s\n' "$(date +%H:%M)" "$1" >>"$f"
}
prev_time=$(<GUARD_PREV_TIME_CMD>)
if [ -f "$bin.prev" ] && { cp -p "$bin" "$bin.bad" 2>/dev/null; mv -f "$bin.prev" "$bin"; }; then
	rm -f "$fails"
	echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：已自动回滚，$bin.prev（$prev_time 构建）换回 $bin，出错的版本留在 $bin.bad"
	summary "已自动回滚：agentq tick 连续 2 次非 0 退出（最后一次退出码 $code），已把 $bin.prev（$prev_time 构建）换回 $bin，出错的版本留在 $bin.bad；原因看 ~/.agentq/launchd.log"
else
	echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：没有可换回的 $bin.prev（或换回失败），没有回滚"
	summary "agentq tick 连续 2 次非 0 退出（最后一次退出码 $code），没有可换回的 $bin.prev，没有回滚，要人看 ~/.agentq/launchd.log"
fi
exit "$code"
