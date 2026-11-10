#!/bin/sh
# 11-09 官网试用：首版设计合并后被用户否掉，dispatch 写了一条「验收 <标题> 的意见」要返工，evolve signals 一条都没收到。
# 要的是：tick 收信号（不调模型），agentq evolve signals 列出 rework，归到设计返工，带上是哪条意见。
root="$EVAL_WORK/projects"; p="$root/site"; fake="$EVAL_WORK/claude"; calls="$EVAL_WORK/claude-calls"
mkdir -p "$p/design" "$p/docs" "$HOME/.agentq"
cat > "$fake" <<FAKE
#!/bin/sh
case "\$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
echo "\$*" >> "$calls"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"好。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
printf '# 待办\n\n## 待做\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '.claude/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m init) || exit 2
printf '<svg xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10"/></svg>\n' > "$p/design/home.svg"
(cd "$p" && git add -A && git commit -q -m "首页首版设计") || exit 2
sha=$(git -C "$p" rev-parse HEAD)
cat > "$p/docs/BACKLOG.md" <<BL
# 待办

## 待做

- [ ] 验收 首页首版设计 的意见 —— 用户否了：配色和版式像模板，重做

## 已完成

- [x] 首页首版设计 —— 出首页设计稿；合并提交 $sha
BL
(cd "$p" && git add -A && git commit -q -m "待办：验收意见") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

"$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" evolve signals 2>&1)
for want in rework 设计返工 "验收 首页首版设计 的意见"; do
  case "$out" in
    *"$want"*) ;;
    *) echo "evolve signals 没有「$want」：$out" | tail -5; exit 1 ;;
  esac
done
if [ -s "$calls" ]; then
  echo "收信号不该调模型：$(head -c 300 "$calls")"
  exit 1
fi
