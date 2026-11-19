#!/bin/sh
# 11-18 gps-tracker：任务 20:15 跑完，把条目标成「卡住：等用户：验收取舍」并写了选项 A/B，交给用户定；
# 那天已经推了 10/10 条，这条「卡住了」按普通「要你知道」被每天上限攒住，用户第二天早上才在早报里看到。
# 要的是：交给用户的卡住算「要你定」，不受每天条数上限挡，当时就推，并列出选项。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq/jobs/1118-201427-demo"
item='- [ ] 判断层 —— 要什么'
printf '# 待办\n\n## 待做\n\n%s（卡住：等用户：验收取舍，分支 agent/1118-v2）（选项：A 改家长页｜B 先合判断层；推荐 B）\n\n## 已完成\n' "$item" > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
fp=$(printf '%s' "$item" | shasum | cut -c1-40)
mac="$EVAL_WORK/macnotify.log"
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":true,"patrol_enabled":false,"tick_after_job":false,"pm_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
printf '{"day":"2026-11-18","count":10,"decide_init":true}\n' > "$HOME/.agentq/notify.json"
sh -c 'exit 0' & dead=$!; wait "$dead" # 已经退出的 pid
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1118-201427-demo","project":"demo","project_dir":"$p","default_branch":"main","item":"判断层 —— 要什么","title":"判断层","fingerprint":"$fp","pid":$dead,
  "started_at":"2026-11-18T20:14:27$off","status":"running"}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"total_cost_usd":3,"result":"分支还不能合并，分支名 agent/1118-v2。没好的原因是：等你在 A、B 里选。","session_id":"530cc332-52d5-4566-92d9-6717df4d958c"}' > "$HOME/.agentq/jobs/1118-201427-demo/result.json"
out=$(AGENTQ_MACNOTIFY_LOG="$mac" AGENTQ_TEST_NOW="2026-11-18T20:15:34$off" "$AGENTQ_BIN" tick 2>&1)
grep -q '"status": "done"' "$HOME/.agentq/state.json" || { echo "任务没收尾：$out" | tail -5; exit 2; }
got=$(cat "$mac" 2>/dev/null)
case "$got" in
  *"要你定"*"「判断层」卡住了"*) ;;
  *) echo "今天推满 10 条后，交给用户的卡住没当时推出去：$got"; exit 1 ;;
esac
case "$got" in
  *"B 先合判断层（推荐）"*) ;;
  *) echo "推送没列出条目写的选项：$got"; exit 1 ;;
esac
exit 0
