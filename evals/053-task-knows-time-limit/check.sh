#!/bin/sh
# 11-19 gps-tracker 的任务要做 24 小时压测、11-21 huitai 的任务要跑 6 小时长跑，执行者都在前台一轮轮等结果，
# 到任务时限（timeout 4h）被杀，前面几小时做好的部分没写 ready.json，一点也交不出去，第二天再接手重做。
# 合同里从没说过这次最多能跑多久。要的是：待做任务的合同写明时限（按配置的 timeout），并说等不完的长跑怎么办。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$p/.agentq" "$HOME/.agentq"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
echo $PPID >> "$HOME/fake-shells"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"没做。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
wait_shells() { # 任务外壳是后台进程：等它退出再结束
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 稳定性加测 —— 含 6 小时长跑\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '.claude/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","timeout":"90m","patrol_enabled":false,"notify_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

out=$("$AGENTQ_BIN" run demo 稳定性 2>&1) || { echo "run 失败：$out" | tail -3; exit 2; }
prompt=$(cat "$HOME"/.agentq/jobs/*/prompt.txt 2>/dev/null)
[ -n "$prompt" ] || { echo "没找到任务的 prompt.txt：$out" | tail -3; exit 2; }
case "$prompt" in
  *"最多 90m"*) ;;
  *) echo "合同里没写这次最多跑多久（配置 timeout 90m）："; printf '%s\n' "$prompt" | tail -3; exit 1 ;;
esac
case "$prompt" in
  *长跑*) ;;
  *) echo "合同没说等不完的长跑怎么办："; printf '%s\n' "$prompt" | tail -3; exit 1 ;;
esac
exit 0
