#!/bin/sh
# 11-10 审查发现：agentq add 在 ~/Documents 这类目录里会直接 git init，把私人文件做初始提交、登记进 extra_projects。
# 要的是：桌面、文档、下载本身不建仓库、不登记，私人文件原样；它们的子目录照常接入。
fake="$EVAL_WORK/claude"
mkdir -p "$HOME/.agentq" "$HOME/projects"
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
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$HOME/projects" "$fake" > "$HOME/.agentq/config.json"
for sub in Desktop Documents Downloads; do
  junk="$HOME/$sub"
  mkdir -p "$junk"
  printf '私人文件\n' > "$junk/tax.pdf"
  out=$(cd "$junk" && "$AGENTQ_BIN" add "整理一下这里的文件" 2>&1)
  code=$?
  if [ "$code" -eq 0 ] || [ -e "$junk/.git" ]; then
    echo "~/$sub 本身不该建仓库（退出码 $code）：$out" | tail -3
    exit 1
  fi
  if grep -q "$sub" "$HOME/.agentq/config.json"; then
    echo "~/$sub 不该登记进配置：$(cat "$HOME/.agentq/config.json")" | head -c 400
    exit 1
  fi
  [ "$(cat "$junk/tax.pdf")" = 私人文件 ] || { echo "~/$sub 里的私人文件被改了"; exit 1; }
done
app="$HOME/Documents/notes-app"
mkdir -p "$app"
printf 'notes\n' > "$app/README.md"
out=$(cd "$app" && "$AGENTQ_BIN" add "加一个按日期排序" 2>&1) || { echo "~/Documents 的子目录应能接入：$out" | tail -3; exit 1; }
[ -d "$app/.git" ] || { echo "~/Documents 的子目录接入后没有仓库：$out" | tail -3; exit 1; }
