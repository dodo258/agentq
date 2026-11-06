#!/bin/sh
# macOS 上 CLI 靠 USER 去钥匙串取订阅登录：服务检查环境少了 USER，登录好的用户会被判成没登录，开不了后台。
fake="$EVAL_WORK/claude"
cat > "$fake" <<'EOF'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)' ;;
  --help) echo '--print --output-format --session-id --effort' ;;
  auth) if [ -n "$USER" ]; then echo '{"loggedIn":true,"authMethod":"claude.ai"}'; else echo '{"loggedIn":false}'; fi ;;
  *) echo '{"is_error":false,"result":"ready"}' ;;
esac
EOF
chmod 755 "$fake"
out=$("$AGENTQ_BIN" setup --plan --services --claude "$fake" 2>&1)
code=$?
if [ "$code" -ne 0 ]; then
  echo "setup --plan --services 退出码 $code：$out" | tail -3
  exit 1
fi
