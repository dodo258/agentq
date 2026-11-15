#!/bin/sh
# 11-13 18:17：审查门的审查进程两次都返回「403 Attention Required! | Cloudflare」，门判「审查没给出合格的结论」直接标卡住，
# 用户只能按放行按钮跳过审查合并。
# 要的是：审查撞上上游瞬时错误（Cloudflare 拦截页的 403）时排回去按退避重审，不标卡住、不占退回轮数。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
mkdir -p "$p/.agentq" "$p/docs" "$HOME/.agentq"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
echo $PPID >> "$HOME/fake-shells"
prompt=""; prev=""
for a in "$@"; do
  case "$prev" in -p) prompt="$a" ;; esac
  prev="$a"
done
case "$prompt" in
  /usage) echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; exit 0 ;;
  *审查门的独立审查进程*) # 真实 result.json 的关键字段（任务 1113-180354-agentq 撞上的同一种拦截页）
    echo '{"type":"result","subtype":"success","is_error":true,"api_error_status":403,"num_turns":1,"result":"Failed to authenticate. API Error: 403 Attention Required! | Cloudflare","session_id":"s-rv"}'; exit 1 ;;
esac
b=agent/ok-file; wt=$PWD/.claude/worktrees/ok-file
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
echo 好了 > "$wt/ok.txt"
mkdir -p "$wt/.agentq"
printf '{"branch":"%s","type":"feature","acceptance":"看 ok.txt","progress":"写好 ok.txt"}\n' "$b" > "$wt/.agentq/ready.json"
git -C "$wt" add -A && git -C "$wt" commit -q -m "交付" >/dev/null 2>&1
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n分支可以合并了。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
# 任务外壳和门是后台进程：等它们都退出再结束，现场删掉后不会再有进程往里写
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
printf '#!/bin/sh\ngrep -q 好了 ok.txt\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 写好 ok.txt —— 审查门会看\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

i=0
while [ $i -lt 40 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  st=$(tr -d '\n ' < "$HOME/.agentq/state.json" 2>/dev/null)
  case "$st" in
    *'"state":"stuck"'*) echo "审查撞上 Cloudflare 拦截页就卡住了：$(grep -o '"report": "[^"]*"' "$HOME/.agentq/state.json" | head -1)"; exit 1 ;;
    *'"state":"queued"'*'"upstream":1'*|*'"upstream":1'*'"state":"queued"'*)
      if grep -q '（卡住' "$p/docs/BACKLOG.md"; then echo "条目标了卡住"; exit 1; fi
      exit 0 ;;
  esac
  sleep 1; i=$((i + 1))
done
echo "40 秒内审查门没有排回去退避："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
