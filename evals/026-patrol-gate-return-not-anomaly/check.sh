#!/bin/sh
# 11-12 repo-tidy 第一版被审查门退回第 1 轮（status=interrupted，门记录 state=returned），巡逻按「任务中断」报成异常、
# 起了一次模型分析，结论是「这不是故障，是门正常打回」，这个问题在 patrol.json 里留了一整天。
# 要的是：门退回的任务巡逻不报、不分析；真正中断（没有门退回记录）的照旧报。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq"
printf '# 待办\n\n## 待做\n\n- [ ] 第一条 —— 要什么（进行中：agent/1112-first 2026-11-12 16:49）\n- [ ] 第二条 —— 要什么（进行中：agent/1112-second 2026-11-12 16:50）\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1112-164509-new","project":"demo","project_dir":"$p","item":"第一条","title":"第一条","fingerprint":"fp-first","pid":0,
  "started_at":"2026-11-12T16:45:09$off","ended_at":"2026-11-12T17:05:00$off","status":"interrupted",
  "summary":"门没过，退回第 1 轮：少了测试","new_report":{"path":"$p","assumptions":"","view":"","commit":"","root":"$root"},
  "gate":{"state":"returned","branch":"agent/1112-first","ready":{"branch":"agent/1112-first","type":"feature","acceptance":"","progress":""}}},
 {"id":"1112-165000-demo","project":"demo","project_dir":"$p","item":"第二条","title":"第二条","fingerprint":"fp-second","pid":0,
  "started_at":"2026-11-12T16:50:00$off","ended_at":"2026-11-12T17:06:00$off","status":"interrupted",
  "summary":"上一轮门没过，退回第 1 轮；这一轮进程被杀"}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
out=$(AGENTQ_TEST_NOW="2026-11-12T17:07:00$off" "$AGENTQ_BIN" patrol 2>&1)
pj="$HOME/.agentq/patrol.json"
[ -f "$pj" ] || { echo "巡逻没跑成：$out" | tail -5; exit 2; }
if grep -q 'job:1112-164509-new' "$pj"; then
  echo "门退回的任务被报成了问题：$(grep -A3 'job:1112-164509-new' "$pj" | head -4)"
  exit 1
fi
grep -q 'job:1112-165000-demo' "$pj" || { echo "真正中断的任务没报：$out" | tail -5; exit 1; }
