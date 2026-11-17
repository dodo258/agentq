#!/bin/sh
# 11-16 09:20 升到 dev-40466ea 后，观察期的三个任务都是升级前被门退回、升级后接着做的（续跑带过来 3、1、1 轮）。
# 观察期把升级前那几轮算成这一代退回的：平均 1.7 轮，比升级前多一轮以上，回滚了，其实这一代一轮都没退回。
# 要的是：观察期的过门退回轮数只算升级后退回的；升级后开始、升级后才退回的照算。
mkdir -p "$HOME/.agentq/evolve"
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$EVAL_WORK/projects" > "$HOME/.agentq/config.json"
ver=$("$AGENTQ_BIN" version | awk '{print $2}')
[ -n "$ver" ] || { echo "读不出版本"; exit 2; }
off=$(date +%z | sed 's/\(..\)$/:\1/')
d=2026-11-16T
printf '{"at":"%s09:20:00%s","kind":"upgrade","from":"dev-1111111","to":"%s","commit":"2222222222222222222222222222222222222222","job":"x"}\n' "$d" "$off" "$ver" > "$HOME/.agentq/evolve/upgrades.jsonl"
# job id 开始 结束 状态 指纹 轮数 门
job() {
  printf ' {"id":"%s","project":"demo","title":"%s","item":"%s","fingerprint":"%s","pid":0,"started_at":"%s%s%s","ended_at":"%s%s%s","status":"%s","gate_round":%s,"gate":{"state":"%s","round":%s}},\n' \
    "$1" "$5" "$5" "$5" "$d" "$2" "$off" "$d" "$3" "$off" "$4" "$6" "$7" "$6"
}
{
  echo '{"jobs":['
  # 升级前做完的 5 个，没退回
  for i in 1 2 3 4 5; do job "pre$i" "0$i:00:00" "0$i:30:00" done "fp-pre$i" 0 merged; done
  # 甲：升级前退回 3 轮，升级后接着做、合并
  job a0 "05:00:00" "05:10:00" interrupted fp-a 0 returned
  job a1 "06:00:00" "06:10:00" interrupted fp-a 1 returned
  job a2 "07:00:00" "07:10:00" interrupted fp-a 2 returned
  job a3 "09:25:00" "09:40:00" done fp-a 3 merged
  # 乙：升级前退回 1 轮，升级后接着做、卡在门上
  job b0 "07:30:00" "07:40:00" interrupted fp-b 0 returned
  job b1 "09:41:00" "10:18:00" done fp-b 1 stuck
  # 丙：升级前退回 1 轮，升级后接着做、合并
  job c0 "08:00:00" "08:10:00" interrupted fp-c 0 returned
  job c1 "10:18:00" "10:32:00" done fp-c 1 merged
  echo ' {"id":"z","project":"demo","title":"z","item":"z","fingerprint":"fp-z","pid":0,"started_at":"'"$d"'00:00:00'"$off"'","ended_at":"'"$d"'00:10:00'"$off"'","status":"done"}'
  echo '],"breaker_until":"0001-01-01T00:00:00Z"}'
} > "$HOME/.agentq/state.json"
out=$("$AGENTQ_BIN" evolve upgrades 2>&1)
case "$out" in
  *"观察期：升级后 3/3 个任务"*) ;;
  *) echo "没看到观察期：$out" | tail -6; exit 2 ;;
esac
case "$out" in
  *"升级后 3/3 个任务（3 个任务，失败、超时或卡在门上 1 个，平均过门退回 0.0 轮）"*) ;;
  *) echo "升级前退回的轮数不该算进观察期：$(printf '%s\n' "$out" | grep 观察期)"; exit 1 ;;
esac
