#!/bin/sh
# 11-11 自己长大：agentq 自己反复出的问题要直接进 self_project 的「待做」（被 exclude_projects 排除也写），不等人回「做 N」；
# 关掉 self_grow_enabled 和以前一样，只写「建议」。
root="$EVAL_WORK/projects"; p="$root/agentq"; fake="$EVAL_WORK/claude"; data="$HOME/.agentq"
mkdir -p "$p/docs" "$data"
printf '[user]\n\tname = t\n\temail = t%sexample.invalid\n[commit]\n\tgpgsign = false\n' @ > "$HOME/.gitconfig"
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
fresh() {
  rm -rf "$p" "$data/evolve" "$data/jobs"; mkdir -p "$p/docs"
  printf '# 待办\n\n## 待做\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
  git -C "$p" init -q -b main && git -C "$p" add -A && git -C "$p" commit -q -m init || exit 1
  for id in 1101-100000-web 1102-100000-web; do
    mkdir -p "$data/jobs/$id"
    printf '{"id":"%s","project":"web","status":"timeout","summary":"跑了四个小时没结束"}\n' "$id" > "$data/jobs/$id/meta.json"
  done
  printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"self_project":"agentq","exclude_projects":["agentq"],"self_grow_enabled":%s}\n' "$root" "$fake" "$1" > "$data/config.json"
}

fresh true
out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 1; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in
  *"- [ ] agentq 自身：任务超时反复出现"*) ;;
  *) echo "自己长大开着：agentq 自己的问题要写进 self_project 的待做（项目被排除也写）："; cat "$p/docs/BACKLOG.md"; exit 1 ;;
esac
git -C "$p" status --porcelain | grep -q . && { echo "写进待做要提交"; exit 1; }

fresh false
"$AGENTQ_BIN" tick >/dev/null 2>&1
if sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md" | grep -q "agentq 自身"; then
  echo "关掉 self_grow_enabled 不能进待做："; cat "$p/docs/BACKLOG.md"; exit 1
fi
exit 0
