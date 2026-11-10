#!/bin/sh
# 11-10 官网试用：agentq stop 停掉的条目要等 retry_after（默认 12 小时）才再接，stop 的回复和 status 都不说，看着像没人接。
# 要的是：stop 和 status 都写明几点后再接、现在就做用哪条命令，照着敲那条命令能马上接上。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
mkdir -p "$p/.agentq" "$p/docs" "$HOME/.agentq"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
  -p) [ "$2" = /usage ] && { echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; exit 0; } ;;
esac
echo $PPID >> "$HOME/fake-shells"
if [ ! -f "$HOME/fake-first" ]; then # 第一次：慢慢做，等着被停
  : > "$HOME/fake-first"
  sleep 20
fi
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"还没做完。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
# 任务外壳和门是后台进程：等它们都退出再结束，现场删掉后不会再有进程往里写
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 整理日志格式 —— 时间放最前面\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"
aq() { "$AGENTQ_BIN" "$@" 2>&1; }
cleanup() {
  for id in $(grep -o '"id": *"[^"]*"' "$HOME/.agentq/state.json" 2>/dev/null | sed 's/.*"\([^"]*\)"$/\1/'); do aq stop "$id" >/dev/null; done
}
trap 'cleanup; wait_shells' EXIT

out=$(aq run demo 整理日志) || { echo "run 失败：$out" | tail -3; exit 1; }
id=$(grep -o '"id": *"[^"]*"' "$HOME/.agentq/state.json" | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
[ -n "$id" ] || { echo "没起任务：$out" | tail -3; exit 1; }
out=$(aq stop "$id")
case "$out" in
  *后才会再自动接*) ;;
  *) echo "stop 没说停掉的条目几点后再接：$out" | tail -3; exit 1 ;;
esac
st=$(aq status)
case "$st" in
  *后才会再自动接*) ;;
  *) echo "status 没说停掉的条目几点后再接：$st" | tail -5; exit 1 ;;
esac
kw=$(printf '%s\n' "$out" | sed -n 's/.*现在就做：agentq run demo "\([^"]*\)".*/\1/p' | head -1)
[ -n "$kw" ] || { echo "stop 没给现在就做的命令：$out" | tail -3; exit 1; }
again=$(aq run demo "$kw")
case "$again" in
  *已启动任务*) exit 0 ;;
esac
echo "照提示敲 agentq run demo \"$kw\" 没接上：$again" | tail -3
exit 1
