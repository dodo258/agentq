#!/bin/sh
# 11-23 huitai 任务 1123-144152：审查发现写的是 src/ui/balance.rs，证据里点名快照 tests/ui_snapshots/balance-reset.txt:17 渲染少了一段。
# 退回范围只给了 src/ui/balance.rs：修渲染必然要更新那份快照，只能标卡住，多一次新会话接手。
# 要的是：审查报告里带行号点到的仓库文件都进退回范围；说明（note）点到的不算。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
mkdir -p "$p/.agentq" "$p/docs" "$p/src" "$HOME/.agentq"
echo 别的 > "$p/src/other.txt"
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
  *审查门的独立审查进程*)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"审完了","session_id":"s-rv","structured_output":{"verdict":"block","summary":"快照里少了提示","findings":[{"file":"src/balance.txt","line":1,"kind":"regression","evidence":"tests/ui_snapshots/reset.txt:1 renders the row without the note","repro":"看快照第 1 行"},{"file":"src/balance.txt","line":1,"kind":"note","evidence":"src/other.txt:1 只是说明","repro":""}]}}'; exit 0 ;;
esac
b=agent/balance; wt=$PWD/.claude/worktrees/balance
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
mkdir -p "$wt/src" "$wt/tests/ui_snapshots"
echo 好了 > "$wt/src/balance.txt"
echo 少了提示 > "$wt/tests/ui_snapshots/reset.txt"
mkdir -p "$wt/.agentq"
printf '{"branch":"%s","type":"feature","acceptance":"看 balance.txt","progress":"写好 balance.txt"}\n' "$b" > "$wt/.agentq/ready.json"
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
printf '#!/bin/sh\ngrep -q 好了 src/balance.txt\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 写好 balance.txt —— 审查门会看\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

# 第一次 tick 接活，接上就暂停：门照常过，退回后不会自动续跑，现场不会在判完分后还有人往里写
"$AGENTQ_BIN" tick >/dev/null 2>&1
# 认领是后台任务写的，慢机器上（CI 的 macOS）要等：先暂停再等，等到「进行中」或已经合并
claimed() {
  i=0
  while [ $i -lt 150 ]; do
    grep -q '（进行中\|^- \[x\]' "$1/docs/BACKLOG.md" && return 0
    sleep 0.2; i=$((i + 1))
  done
  return 1
}
"$AGENTQ_BIN" pause >/dev/null 2>&1 || { echo "pause 失败"; exit 2; }
claimed "$p" || { echo "第一次 tick 没接上活：$("$AGENTQ_BIN" status 2>&1 | tail -20)"; exit 2; }
i=0
while [ $i -lt 40 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  st=$(tr -d '\n ' < "$HOME/.agentq/state.json" 2>/dev/null)
  case "$st" in
    *'"state":"merged"'*|*'"state":"stuck"'*) echo "审查有带证据的问题，门没有退回"; exit 2 ;;
    *'"state":"returned"'*)
      scope=${st#*'"scope":['}; scope=${scope%%]*}
      case "$scope" in *'"src/balance.txt"'*) ;; *) echo "退回范围里没有发现所在的文件：$scope"; exit 2 ;; esac
      case "$scope" in *'"tests/ui_snapshots/reset.txt"'*) ;; *) echo "审查报告点名的快照不在退回范围里：$scope"; exit 1 ;; esac
      case "$scope" in *'"src/other.txt"'*) echo "只是说明里点到的文件也进了范围：$scope"; exit 1 ;; esac
      exit 0 ;;
  esac
  sleep 1; i=$((i + 1))
done
echo "40 秒内审查门没有退回："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
