#!/bin/sh
# 11-11 审查发现：做减法把 retention_days 写死成 90，配过 0（不删）的老用户升级后第一次 tick 就永久删掉超过 90 天的任务记录。
# 要的是：retention_days 配 0 时 tick 不删任何记录；改成 30 后才清掉过期的，新的留着。
root="$EVAL_WORK/projects"; fake="$EVAL_WORK/claude"; data="$HOME/.agentq"
mkdir -p "$root" "$data"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"好。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
cfg() {
  printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"retention_days":%s}\n' "$root" "$fake" "$1" > "$data/config.json"
}
old="$data/jobs/0101-120000-demo"; oldgate="$data/gate/0101-120000-demo"; new="$data/jobs/1101-120000-demo"
mkdir -p "$old" "$oldgate" "$new"
printf '{"id":"0101-120000-demo","status":"done"}\n' > "$old/meta.json"
printf '{"results":[]}\n' > "$oldgate/last.json"
printf '{"id":"1101-120000-demo","status":"done"}\n' > "$new/meta.json"
touch -t 202501011200 "$old/meta.json" "$old" "$oldgate/last.json" "$oldgate"

cfg 0
out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 1; }
if [ ! -f "$old/meta.json" ] || [ ! -f "$oldgate/last.json" ]; then
  echo "retention_days 配 0（不删），tick 却删了旧记录"
  exit 1
fi
cfg 30
rm -f "$data/retention-day" # 清理一天一次：换个配置再清一次
"$AGENTQ_BIN" tick >/dev/null 2>&1
if [ -e "$old" ] || [ ! -f "$new/meta.json" ]; then
  echo "retention_days 30：过期的该清掉、新的该留着（旧的还在：$([ -e "$old" ] && echo 是 || echo 否)，新的还在：$([ -f "$new/meta.json" ] && echo 是 || echo 否)）"
  exit 1
fi
