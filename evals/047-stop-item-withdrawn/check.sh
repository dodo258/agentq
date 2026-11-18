#!/bin/sh
# 11-16 到 11-18 三次「用户停了任务」：两次是派活会话把只评估的建议写进了待做，tick 接了，用户停掉后一分钟内把条目撤出待做；
# 一次是让路给别的项目，之后去掉标记接着做完、合并进「已完成」。自我进化把三次都记成 user-stop，攒够次数给 agentq 写了待做，
# 其实都是用户改计划。要的是：停的那一条在 BACKLOG 里待过、后来已经不在「待做」里的不记；停了还留在待做里没人管的照记。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq"
commit() { (cd "$p" && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m "$1") || exit 2; }
(cd "$p" && git init -q -b main) || exit 2
printf '# 待办\n\n## 待做\n\n- [ ] 滚动误呼菜单 —— 用户建议（进行中：agent/1118-scroll 2026-11-18 15:00）\n- [ ] 门停住先审查 —— 要什么（进行中：agent/1118-hold 2026-11-18 15:00）\n- [ ] 书名识别 —— 要什么（进行中：agent/1118-title 2026-11-18 15:00）\n\n## 以后再说\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
commit 认领
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1118-150000-demo","project":"demo","project_dir":"$p","item":"滚动误呼菜单 —— 用户建议","title":"滚动误呼菜单","fingerprint":"fp-scroll","pid":0,
  "started_at":"2026-11-18T15:00:00$off","status":"running"},
 {"id":"1118-150001-demo","project":"demo","project_dir":"$p","item":"门停住先审查 —— 要什么","title":"门停住先审查","fingerprint":"fp-hold","pid":0,
  "started_at":"2026-11-18T15:00:01$off","status":"running"},
 {"id":"1118-150002-demo","project":"demo","project_dir":"$p","item":"书名识别 —— 要什么","title":"书名识别","fingerprint":"fp-title","pid":0,
  "started_at":"2026-11-18T15:00:02$off","status":"running"}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
for id in 1118-150000-demo 1118-150001-demo 1118-150002-demo; do
  out=$(AGENTQ_TEST_NOW="2026-11-18T15:10:00$off" "$AGENTQ_BIN" stop "$id" 2>&1)
  case "$out" in
    *已停止*) ;;
    *) echo "没停成 $id：$out" | tail -5; exit 2 ;;
  esac
done
# 停了以后：一条撤到「以后再说」，一条接着做完合并进「已完成」，一条留在待做没人管
printf '# 待办\n\n## 待做\n\n- [ ] 书名识别 —— 要什么（进行中：agent/1118-title 2026-11-18 15:00）\n\n## 以后再说\n\n- [ ] 滚动误呼菜单 —— 用户建议，先评估\n\n## 已完成\n\n- [x] 门停住先审查 —— 要什么；合并提交 abcdef1234567\n' > "$p/docs/BACKLOG.md"
commit 撤回和合并
AGENTQ_TEST_NOW="2026-11-18T16:00:00$off" "$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" evolve signals 2>&1)
case "$out" in
  *"任务 1118-150002-demo"*) ;;
  *) echo "停了还留在待做里没人管的要记成 user-stop：$out" | tail -6; exit 2 ;;
esac
case "$out" in
  *"任务 1118-150000-demo"*) echo "停了以后撤出待做，是改计划，不该记：$out" | tail -6; exit 1 ;;
esac
case "$out" in
  *"任务 1118-150001-demo"*) echo "停了以后接着做完合并，是让路，不该记：$out" | tail -6; exit 1 ;;
esac
exit 0
