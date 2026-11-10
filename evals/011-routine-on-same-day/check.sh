#!/bin/sh
# 11-10 13:13 打开夜班、复盘、巡逻：巡逻立刻报「还没有当天的早报/夜班报告」并起了一次分析，tick 还按错过补跑了复盘。
# 可今天 01:00、06:30 到点时开关是关着的，本来就不该有报告。要的是：当天不报、不补；第二天开关一直开着却没报告，照常报。
root="$EVAL_WORK/projects"; fake="$EVAL_WORK/claude"; calls="$EVAL_WORK/claude-calls"
mkdir -p "$root" "$HOME/.agentq"
cat > "$fake" <<FAKE
#!/bin/sh
case "\$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
echo \$PPID >> "\$HOME/fake-shells"
echo "\$*" >> "$calls"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"看过了。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
# 任务外壳和门是后台进程：等它们都退出再结束，现场删掉后不会再有进程往里写
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
cfg() { # $1：夜班和复盘开没开
  printf '{"projects_root":"%s","claude_path":"%s","notify_enabled":false,"patrol_enabled":%s,"night_enabled":%s,"review_enabled":%s}\n' "$root" "$fake" "$1" "$1" "$1" > "$HOME/.agentq/config.json"
}
off=$(date +%z | sed 's/\(..\)$/:\1/')
day=2026-10-05; next=2026-10-06
at() { t=$1; shift; AGENTQ_TEST_NOW="${t}${off}" "$AGENTQ_BIN" "$@" 2>&1; }

cfg false
at "${day}T01:00:00" night --scheduled >/dev/null || { echo "夜班定点叫醒报错"; exit 2; }
at "${day}T06:30:00" review --scheduled >/dev/null || { echo "复盘定点叫醒报错"; exit 2; }
cfg true # 13:13 才打开
out=$(at "${day}T13:15:00" patrol)
case "$out" in
  *"$day.md 不存在"*|*"$day-night.md 不存在"*) echo "当天到点时开关关着，巡逻却报缺报告：$out" | tail -5; exit 1 ;;
esac
at "${day}T13:16:00" tick >/dev/null
if [ -s "$calls" ]; then
  echo "当天不该起模型（巡逻分析或补跑复盘）：$(head -c 300 "$calls")"
  exit 1
fi
out=$(at "${next}T13:15:00" patrol)
case "$out" in
  *"$next.md 不存在"*) ;;
  *) echo "第二天开关一直开着、没有早报，巡逻该报却没报：$out" | tail -5; exit 1 ;;
esac
