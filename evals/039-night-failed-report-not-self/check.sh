#!/bin/sh
# 10-12 01:00 夜班任务因为后端登录失败（组织关了订阅登录）没跑成，巡逻 01:32 报了「任务失败」；08:32 又报「08:00 后还没有当天的夜班报告」，
# 自我进化把它算成 agentq 自己的问题，和 11-10 那次（当天才打开开关，已另修）合在一起写成「巡逻发现问题反复出现」的待做。
# 要的是：当天的夜班任务失败了，缺报告是那次失败的后果，巡逻照报，自我进化不另算；夜班说做完了却没写报告的照算 agentq 的问题。
root="$EVAL_WORK/projects"
mkdir -p "$root" "$HOME/.agentq"
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false,"night_enabled":true,"review_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1012-010008-夜班","kind":"night","project":"夜班","project_dir":"$root","item":"night","title":"agentq night","pid":0,
  "started_at":"2026-10-12T01:00:08$off","ended_at":"2026-10-12T01:00:20$off","status":"failed",
  "summary":"Your organization has disabled Claude subscription access for Claude Code"},
 {"id":"1013-010008-夜班","kind":"night","project":"夜班","project_dir":"$root","item":"night","title":"agentq night","pid":0,
  "started_at":"2026-10-13T01:00:08$off","ended_at":"2026-10-13T03:00:00$off","status":"done",
  "summary":"夜班做完了"}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
at() { t=$1; shift; AGENTQ_TEST_NOW="${t}${off}" "$AGENTQ_BIN" "$@" 2>&1; }
pj="$HOME/.agentq/patrol.json"; sj="$HOME/.agentq/evolve/signals.json"

out=$(at 2026-10-12T08:32:00 patrol)
grep -q 'report:night:2026-10-12' "$pj" 2>/dev/null || { echo "缺夜班报告巡逻该照报却没报：$out" | tail -5; exit 2; }
at 2026-10-12T08:33:00 tick >/dev/null
if grep -q '夜班报告' "$sj" 2>/dev/null; then
  echo "夜班任务失败引起的缺报告被算成了 agentq 自己的问题：$(grep -o '"summary": *"[^"]*夜班报告[^"]*"' "$sj" | head -2)"
  exit 1
fi

out=$(at 2026-10-13T08:32:00 patrol)
grep -q 'report:night:2026-10-13' "$pj" 2>/dev/null || { echo "缺夜班报告巡逻该照报却没报：$out" | tail -5; exit 2; }
at 2026-10-13T08:33:00 tick >/dev/null
grep -q '夜班报告' "$sj" 2>/dev/null || { echo "夜班做完了却没写报告，该算 agentq 的问题却没算：$(cat "$sj" 2>/dev/null | head -c 300)"; exit 1; }
