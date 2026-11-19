#!/bin/sh
# 11-20 一次派活会话：亲手 agentq run 了一条，用户同一秒说「这个不做了」，会话 5 秒后 agentq stop，十几秒后把那一条撤出待做。
# 自我进化把它记成「亲手派的任务刚启动就被停」，一次就给 agentq 自己写了一条「查哪句说明带偏了派活的人」的待做，其实是用户改了主意。
# 要的是：刚启动就停、停的那一条在 BACKLOG 里待过、后来已经不在「待做」里的不记 quick-stop；停了还留在待做里的照记。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq"
commit() { (cd "$p" && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m "$1") || exit 2; }
(cd "$p" && git init -q -b main) || exit 2
printf '# 待办\n\n## 待做\n\n- [ ] 移植专用版 —— 要什么（进行中：agent/1120-port 2026-11-20 01:34）\n- [ ] 加导出 —— 要什么（进行中：agent/1120-export 2026-11-20 01:34）\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
commit 认领
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1120-013452-demo","project":"demo","project_dir":"$p","item":"移植专用版 —— 要什么","title":"移植专用版","fingerprint":"fp-port","pid":0,
  "started_at":"2026-11-20T01:34:52$off","status":"running","manual":true},
 {"id":"1120-013453-demo","project":"demo","project_dir":"$p","item":"加导出 —— 要什么","title":"加导出","fingerprint":"fp-export","pid":0,
  "started_at":"2026-11-20T01:34:53$off","status":"running","manual":true}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
for id in 1120-013452-demo 1120-013453-demo; do
  out=$(AGENTQ_TEST_NOW="2026-11-20T01:34:58$off" "$AGENTQ_BIN" stop "$id" 2>&1)
  case "$out" in
    *已停止*) ;;
    *) echo "没停成 $id：$out" | tail -5; exit 2 ;;
  esac
done
# 停了以后：「移植专用版」撤出待做（用户不做了），「加导出」留在待做
printf '# 待办\n\n## 待做\n\n- [ ] 加导出 —— 要什么（进行中：agent/1120-export 2026-11-20 01:34）\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
commit 撤下移植专用版
AGENTQ_TEST_NOW="2026-11-20T02:01:37$off" "$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" evolve signals 2>&1)
case "$out" in
  *"任务 1120-013453-demo"*) ;;
  *) echo "停了还留在待做里的要记成 quick-stop：$out" | tail -6; exit 2 ;;
esac
case "$out" in
  *"任务 1120-013452-demo"*) echo "刚启动就停、随后撤出待做，是用户改了主意，不该记：$out" | tail -6; exit 1 ;;
esac
exit 0
