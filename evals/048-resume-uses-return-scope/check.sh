#!/bin/sh
# 11-18 gps-tracker：任务被审查退回，退回范围只有 parent-web/ 下几个文件；退回那一轮中断后等续跑。
# 同项目在跑的另一条范围是 app-v2/、tools/simulator/，只和条目原文范围里的 tools/simulator/ 重叠，和退回范围不重叠，
# 调度却按条目原文判重叠，一个半小时没续跑。要的是：续跑判重叠用退回时定的范围；没定范围的照旧按条目原文判。
root="$EVAL_WORK/projects"; p="$root/demo"
mkdir -p "$p/docs" "$HOME/.agentq"
printf '# 待办\n\n## 待做\n\n- [ ] 网页截图 —— 要什么（范围：parent-web/、tools/simulator/）（进行中：agent/1118-web 2026-11-18 16:34）\n- [ ] 模拟器 —— 要什么（范围：app-v2/、tools/simulator/）（进行中：agent/1118-sim 2026-11-18 17:35）\n- [ ] 模拟器脚本 —— 要什么（范围：tools/simulator/run.sh）（进行中：agent/1118-run 2026-11-18 16:00）\n' > "$p/docs/BACKLOG.md"
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1118-173533-demo","project":"demo","project_dir":"$p","item":"模拟器 —— 要什么（范围：app-v2/、tools/simulator/）","title":"模拟器","fingerprint":"fp-sim","pid":0,
  "started_at":"2026-11-18T17:35:33$off","status":"running"},
 {"id":"1118-170000-demo","project":"demo","project_dir":"$p","item":"网页截图 —— 要什么（范围：parent-web/、tools/simulator/）","title":"网页截图","fingerprint":"fp-web","pid":0,
  "started_at":"2026-11-18T17:00:00$off","ended_at":"2026-11-18T17:35:00$off","status":"interrupted","resume_pending":true,
  "gate_round":1,"gate_scope":["parent-web/src/App.tsx","parent-web/src/map.ts"],"gate_scope_from":"0000000"},
 {"id":"1118-160000-demo","project":"demo","project_dir":"$p","item":"模拟器脚本 —— 要什么（范围：tools/simulator/run.sh）","title":"模拟器脚本","fingerprint":"fp-run","pid":0,
  "started_at":"2026-11-18T16:00:00$off","ended_at":"2026-11-18T16:30:00$off","status":"interrupted","resume_pending":true}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
out=$(AGENTQ_TEST_NOW="2026-11-18T19:00:00$off" "$AGENTQ_BIN" status 2>&1)
case "$out" in
  *"等续跑：demo「模拟器脚本」"*) ;;
  *) echo "没退回范围、和在跑的范围重叠的要说在等：$out" | tail -8; exit 2 ;;
esac
case "$out" in
  *"等续跑：demo「网页截图」"*) echo "退回范围只有 parent-web/ 下的文件，和在跑的不重叠，不该挡：$out" | tail -8; exit 1 ;;
esac
exit 0
