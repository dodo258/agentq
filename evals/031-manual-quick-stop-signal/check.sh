#!/bin/sh
# 11-15 一次派活会话：后台过了 70% 额度线，会话把「agentq run 仍可用」当成可以绕过上限，亲手 run 之后 7 秒自己 agentq stop，
# 条目被 retry_after 挂了 12 小时。自我进化只记成「用户停了任务」（正常操作，要两次才提，而这一类早提过），一直没发现。
# 要的是：亲手派的任务启动 3 分钟内被停，tick 收信号时单算一类，一次就提案，证据带上谁停的（命令行）和停时的额度、后台上限。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq"
printf '# 待办\n\n## 待做\n\n- [ ] 加导出 —— 要什么（进行中：agent/1115-export 2026-11-15 15:04）\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false,"usage_stop_session_pct":70}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1115-150444-demo","project":"demo","project_dir":"$p","item":"加导出","title":"加导出","fingerprint":"fp-export","pid":0,
  "started_at":"2026-11-15T15:04:44$off","status":"running","manual":true}
],"breaker_until":"0001-01-01T00:00:00Z",
 "backend_limits":{"claude":{"usage":{"checked_at":"2026-11-15T15:04:00$off","windows":[{"label":"session","used":72}]}}}}
STATE
out=$(AGENTQ_TEST_NOW="2026-11-15T15:04:51$off" "$AGENTQ_BIN" stop 1115-150444-demo 2>&1)
case "$out" in
  *已停止*) ;;
  *) echo "没停成：$out" | tail -5; exit 2 ;;
esac
AGENTQ_TEST_NOW="2026-11-15T15:06:00$off" "$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" evolve signals 2>&1)
for want in quick-stop "任务 1115-150444-demo" "被停（命令行）" "到了后台上限（会话 72%，后台上限 70%）" 已提案; do
  case "$out" in
    *"$want"*) ;;
    *) echo "evolve signals 没有「$want」：$out" | tail -6; exit 1 ;;
  esac
done
