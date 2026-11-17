#!/bin/sh
# 11-10 到 11-15 三个项目各有一次：派活会话 agentq run 之后发现条目要改（改写条目、换一条替它），agentq stop 掉，
# 几分钟内亲手再派。自我进化把这三次都记成「用户亲手停了任务」，攒够次数给 agentq 自己写了一条待做，其实是用户改需求。
# 要的是：停了以后 30 分钟内同一项目亲手再派的不记 user-stop；停了没人重派的照样记。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq/jobs/1110-150740-demo"
printf '# 待办\n\n## 待做\n\n- [ ] 加导出 —— 要什么（进行中：agent/1110-export 2026-11-10 15:00）\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1110-135000-demo","project":"demo","project_dir":"$p","item":"加日志","title":"加日志","fingerprint":"fp-log","pid":0,
  "started_at":"2026-11-10T13:50:00$off","status":"running","manual":true},
 {"id":"1110-150000-demo","project":"demo","project_dir":"$p","item":"加导出","title":"加导出","fingerprint":"fp-export","pid":0,
  "started_at":"2026-11-10T15:00:00$off","status":"running","manual":true}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
for s in "1110-135000-demo 2026-11-10T14:00:00" "1110-150000-demo 2026-11-10T15:06:39"; do
  set -- $s
  out=$(AGENTQ_TEST_NOW="$2$off" "$AGENTQ_BIN" stop "$1" 2>&1)
  case "$out" in
    *已停止*) ;;
    *) echo "没停成 $1：$out" | tail -5; exit 2 ;;
  esac
done
# 停了一分钟后亲手重派改过的条目
cat > "$HOME/.agentq/jobs/1110-150740-demo/meta.json" <<META
{"id":"1110-150740-demo","project":"demo","project_dir":"$p","item":"加导出成 CSV","title":"加导出成 CSV","fingerprint":"fp-export2","pid":0,
 "started_at":"2026-11-10T15:07:40$off","ended_at":"2026-11-10T15:30:00$off","status":"done","manual":true,"summary":"跑完"}
META
AGENTQ_TEST_NOW="2026-11-10T16:00:00$off" "$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" evolve signals 2>&1)
case "$out" in
  *"任务 1110-135000-demo"*) ;;
  *) echo "没人重派的停止要记成 user-stop：$out" | tail -6; exit 2 ;;
esac
case "$out" in
  *"任务 1110-150000-demo"*) echo "停了一分钟后亲手重派，是改需求，不该记：$out" | tail -6; exit 1 ;;
esac
