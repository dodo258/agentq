#!/bin/sh
# 用户装过别的版本的同名技能（比如 Waza 的 check）：不算冲突，不替换，照样装得上。
fake="$EVAL_WORK/claude"
cat > "$fake" <<'EOF'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)' ;;
  --help) echo '--print --output-format --session-id --effort' ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}' ;;
  *) echo '{"is_error":false,"result":"ready"}' ;;
esac
EOF
chmod 755 "$fake"
mkdir -p "$HOME/.claude/skills/check"
printf '# 用户自己的 check 技能\n' > "$HOME/.claude/skills/check/SKILL.md"
before=$(cat "$HOME/.claude/skills/check/SKILL.md")
out=$("$AGENTQ_BIN" setup --plan --claude "$fake" 2>&1)
code=$?
if [ "$code" -ne 0 ]; then
  echo "setup --plan 退出码 $code：$out" | tail -3
  exit 1
fi
case "$out" in
  *conflict*) echo "setup --plan 报了冲突：$out" | tail -3; exit 1 ;;
esac
out=$("$AGENTQ_BIN" setup --yes --claude "$fake" 2>&1)
code=$?
if [ "$code" -ne 0 ]; then
  echo "setup --yes 退出码 $code：$out" | tail -3
  exit 1
fi
if [ "$(cat "$HOME/.claude/skills/check/SKILL.md")" != "$before" ]; then
  echo "用户自己的技能被改了"
  exit 1
fi
