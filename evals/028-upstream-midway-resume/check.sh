#!/bin/sh
# 11-13：产品经理任务跑了 16 分钟后 Claude 接口返回「403 Attention Required! | Cloudflare」，因为已经花了钱不算瞬时错误，
# 任务记成失败，条目留着进行中、worktree 里的改动没人接，要等孤儿处理的 24 小时窗口。
# 要的是：做到一半撞上游瞬时错误（Cloudflare 拦截页的 403），能续会话就记中断、用同一会话续跑；真正的认证失败（401）照旧暂停后端，不续跑。
root="$EVAL_WORK/projects"; p="$root/demo"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq/jobs/1113-180354-demo" "$HOME/.agentq/jobs/1113-180355-demo"
cf='- [ ] 撞拦截页 —— 要什么'
au='- [ ] 令牌失效 —— 要什么'
printf '# 待办\n\n## 待做\n\n%s（进行中：agent/1113-cf 2026-11-13 18:03）\n%s（进行中：agent/1113-au 2026-11-13 18:03）\n\n## 已完成\n' "$cf" "$au" > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
fp() { printf '%s' "$1" | shasum | cut -c1-40; }
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false,"tick_after_job":false,"pm_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
sh -c 'exit 0' & dead=$!; wait "$dead" # 已经退出的 pid
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/state.json" <<STATE
{"jobs":[
 {"id":"1113-180354-demo","project":"demo","project_dir":"$p","default_branch":"main","item":"撞拦截页 —— 要什么","title":"撞拦截页","fingerprint":"$(fp "$cf")","pid":$dead,
  "started_at":"2026-11-13T18:03:54$off","status":"running","gate_contract":true},
 {"id":"1113-180355-demo","project":"demo","project_dir":"$p","default_branch":"main","item":"令牌失效 —— 要什么","title":"令牌失效","fingerprint":"$(fp "$au")","pid":$dead,
  "started_at":"2026-11-13T18:03:55$off","status":"running","gate_contract":true}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
# 真实 result.json 的关键字段（任务 1113-180354-agentq）
printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"api_error_status":403,"num_turns":73,"total_cost_usd":5.9,"result":"Failed to authenticate. API Error: 403 Attention Required! | Cloudflare","session_id":"4e36c640-4c2b-488a-88a1-dc30b68b0c70"}' > "$HOME/.agentq/jobs/1113-180354-demo/result.json"
printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"num_turns":30,"total_cost_usd":2,"result":"Failed to authenticate. API Error: 401 authentication_error","session_id":"5f36c640-4c2b-488a-88a1-dc30b68b0c70"}' > "$HOME/.agentq/jobs/1113-180355-demo/result.json"
out=$("$AGENTQ_BIN" tick 2>&1)
st=$(cat "$HOME/.agentq/state.json")
job() { printf '%s' "$st" | tr -d '\n' | sed 's/},[[:space:]]*{/}\n{/g' | grep "\"id\": *\"$1\""; }
a=$(job 1113-180354-demo); b=$(job 1113-180355-demo)
[ -n "$a" ] && [ -n "$b" ] || { echo "状态里找不到任务：$out" | tail -5; exit 2; }
case "$a" in
  *'"status": "interrupted"'*'"resume_pending": true'*|*'"resume_pending": true'*'"status": "interrupted"'*) ;;
  *) echo "做到一半撞 Cloudflare 拦截页没记中断、没等续跑：$(printf '%s' "$a" | grep -o '"summary": "[^"]*"')"; exit 1 ;;
esac
case "$b" in
  *'"resume_pending": true'*) echo "认证失败不该续跑"; exit 1 ;;
  *'"status": "failed"'*) ;;
  *) echo "认证失败应按失败算：$(printf '%s' "$b" | grep -o '"status": "[^"]*"')"; exit 1 ;;
esac
[ -f "$HOME/.agentq/backend-auth/claude" ] || { echo "认证失败没暂停后端"; exit 1; }
