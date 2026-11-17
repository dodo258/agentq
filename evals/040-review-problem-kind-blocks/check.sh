#!/bin/sh
# 11-11 到 11-15：门停住等人看的三次里，用户在卡片上退回了三次，两次是审查自己已经找到、写了复现的真问题：
# 任务 1115-192048-gps-tracker 的审查写出「上传期间裁剪删掉最老点，之后删的是没上传的下一条，静默丢点」和复现步骤，
# 却自己标成 severity=hint、verdict=pass，门就判过了；只因为改动里有 DELETE FROM 要人看，才被用户退回。
# 要的是：要不要阻塞由引擎按问题类别和证据定，带文件、行号、复现的丢数据问题不管模型标什么都退回。
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
prompt=""; prev=""
for a in "$@"; do
  case "$prev" in -p) prompt="$a" ;; esac
  prev="$a"
done
case "$prompt" in
  /usage) echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; exit 0 ;;
  *审查门的独立审查进程*) # 任务 1115-192048-gps-tracker 审查给的结论，换成这个现场的文件
    echo '{"type":"result","subtype":"success","is_error":false,"result":"审完了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"实现符合需求，无阻塞问题","findings":[{"file":"ok.txt","line":1,"severity":"hint","kind":"data-loss","evidence":"补传时先取最老点 A 再上传，上传期间裁剪把 A 删了，上传成功后按最小 id 删掉的是还没上传的 B，没有日志","repro":"离线攒满后恢复网络补传，在 A 上传期间来一个新定位点：B 从没上传就被删了"}]}}'; exit 0 ;;
esac
b=agent/ok-file; wt=$PWD/.claude/worktrees/ok-file
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
echo 好了 > "$wt/ok.txt"
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
printf '#!/bin/sh\ngrep -q 好了 ok.txt\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 写好 ok.txt —— 审查门会看\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

# 第一次 tick 接活，接上就暂停：门照常过，退回后不会自动续跑，现场不会在判完分后还有人往里写
"$AGENTQ_BIN" tick >/dev/null 2>&1
grep -q '（进行中' "$p/docs/BACKLOG.md" || { echo "第一次 tick 没接上活：$("$AGENTQ_BIN" status 2>&1 | tail -3)"; exit 2; }
"$AGENTQ_BIN" pause >/dev/null 2>&1 || { echo "pause 失败"; exit 2; }
i=0
while [ $i -lt 40 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  st=$(tr -d '\n ' < "$HOME/.agentq/state.json" 2>/dev/null)
  case "$st" in
    *'"state":"merged"'*) echo "审查写了带复现的丢数据问题，门还是合并了"; exit 1 ;;
    *'"state":"stuck"'*) echo "审查写了带复现的丢数据问题，门没有退回而是卡住了"; exit 1 ;;
    *'"state":"returned"'*)
      if ! grep -q 'ok.txt:1 补传时先取最老点' "$HOME/.agentq/state.json"; then echo "退回了，但退回报告里没有审查找到的问题"; exit 1; fi
      exit 0 ;;
  esac
  sleep 1; i=$((i + 1))
done
echo "40 秒内审查门没有退回："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
