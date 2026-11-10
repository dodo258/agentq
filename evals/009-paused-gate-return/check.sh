#!/bin/sh
# 11-09 官网改版试用：暂停时用 agentq run 亲手派的条目被测试门退回，tick 不续跑、run 也不接进行中的条目，只能先 resume。
# 要的是：一直暂停着，第一轮被门退回，tick 用同一会话接着做，第二轮过门合并。
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
prompt=""; resume=""; prev=""
for a in "$@"; do
  case "$prev" in -p) prompt="$a" ;; -r) resume="$a" ;; esac
  prev="$a"
done
case "$prompt" in
  /usage) echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; exit 0 ;;
  *审查门的独立审查进程*) echo '{"type":"result","subtype":"success","is_error":false,"result":"审过了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"没问题","findings":[]}}'; exit 0 ;;
esac
b=agent/ok-file; wt=$PWD/.claude/worktrees/ok-file
if [ -z "$resume" ]; then # 第一轮：认领，交一个过不了测试门的半成品
  sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
  git add docs/BACKLOG.md && git commit -q -m "待办：认领"
  git worktree add -q "$wt" -b "$b" main
  echo 半成品 > "$wt/ok.txt"
else # 门退回后用同一会话接着做：改好同一个文件
  echo 好了 > "$wt/ok.txt"
fi
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
printf '#!/bin/sh\ngrep -q 好了 ok.txt 2>/dev/null || { echo ok.txt 还没写好; exit 1; }\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 写好 ok.txt —— 测试门会查\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

"$AGENTQ_BIN" pause >/dev/null 2>&1 || { echo "pause 失败"; exit 2; }
out=$("$AGENTQ_BIN" run demo 写好 2>&1) || { echo "run 失败：$out" | tail -3; exit 1; }
i=0
while [ $i -lt 40 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  if git -C "$p" log --format=%B main | grep -q '由 agentq 过门后合并' && git -C "$p" show main:ok.txt 2>/dev/null | grep -q 好了; then
    exit 0
  fi
  sleep 1; i=$((i + 1))
done
echo "暂停着亲手派的条目被门退回后 40 秒没做完："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
