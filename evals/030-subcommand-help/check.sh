#!/bin/sh
# 11-13 Codex 派活前跑 agentq add --help，回的是「不认识 --help」、退出码非 0；run --help 把 --help 当项目名，logs --help 报「没有任务 --help」。
# 要的是：每个子命令（含二级命令）见到 -h、--help 只打印这条命令的用法，退出码 0，不写 BACKLOG、不起任务、不改状态。
fake="$EVAL_WORK/claude"
called="$EVAL_WORK/claude-called"
mkdir -p "$HOME/.agentq" "$HOME/projects/demo/docs"
cat > "$fake" <<FAKE
#!/bin/sh
case "\$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
touch "$called"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"好。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"pm_enabled":false}\n' "$HOME/projects" "$fake" > "$HOME/.agentq/config.json"
proj="$HOME/projects/demo"
printf '# 待办\n\n## 待做\n\n- [ ] 做一件事\n' > "$proj/docs/BACKLOG.md"
git -C "$proj" init -q -b main && git -C "$proj" add -A && git -C "$proj" -c user.name=t -c user.email=t commit -q -m init || { echo "建项目失败"; exit 1; }
snap() { find "$HOME/.agentq" "$proj" -path "$proj/.git" -prune -o -type f -print | sort | xargs cksum; git -C "$proj" rev-parse HEAD; }
before=$(snap)
for args in "add --help" "add 做点别的 -h" "run --help" "run demo --help" "logs --help" "stop -h" "new --help" \
  "gate report --help" "backlog archive demo --help" "eval run --help" "evolve apply --help" "notify send --help" \
  "start --help" "tick --help" "setup --help" "remove -h" "status --help"; do
  name=${args%% *}
  # shellcheck disable=SC2086
  out=$(cd "$proj" && "$AGENTQ_BIN" $args 2>&1)
  code=$?
  if [ "$code" -ne 0 ]; then
    echo "agentq $args 退出码 $code：$out" | head -3
    exit 1
  fi
  case "$out" in
    *"agentq $name"*) ;;
    *) echo "agentq $args 没打印这条命令的用法：$out" | head -3; exit 1 ;;
  esac
done
[ "$(snap)" = "$before" ] || { echo "--help 改了文件或提交"; exit 1; }
[ ! -e "$called" ] || { echo "--help 调了模型 CLI"; exit 1; }
