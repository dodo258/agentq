#!/bin/sh
# 后端能力缺口又出现（11-15 到 11-16 四次问题）：三次是和最新 main 冲突退回一轮，其中两次撞在 tests/eval_test.go——
# 测试里写死了评测题数，每加一道题都改同一行，两条并行的分支各加一道就撞上；30 天里 93 次合并有 20 次在改这几行。
# 要的是：Go 项目的任务给清单加了一项、把测试里写死的「有几项」跟着改了个数，能力门要报出来（warn 档照样合并），
# 并在门的结果里记下用上的是 backend-count-hotspot。没有这项能力时能力门不报这一项，这题判没过。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
mkdir -p "$p/.agentq" "$p/docs" "$p/cmds" "$HOME/.agentq"
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
  *审查门的独立审查进程*) echo '{"type":"result","subtype":"success","is_error":false,"result":"审过了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"没问题","findings":[]}}'; exit 0 ;;
esac
b=agent/add-stop; wt=$PWD/.claude/worktrees/add-stop
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
sed -i.bak 's/"start", "status"}/"start", "status", "stop"}/' "$wt/cmds/cmds.go" && rm -f "$wt/cmds/cmds.go.bak"
sed -i.bak 's/len(Names) != 2/len(Names) != 3/' "$wt/cmds/cmds_test.go" && rm -f "$wt/cmds/cmds_test.go.bak"
mkdir -p "$wt/.agentq"
printf '{"branch":"%s","type":"feature","acceptance":"看 cmds/cmds.go","progress":"加了 stop 命令"}\n' "$b" > "$wt/.agentq/ready.json"
git -C "$wt" add -A && git -C "$wt" commit -q -m "加 stop 命令" >/dev/null 2>&1
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
printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf 'module demo\n\ngo 1.22\n' > "$p/go.mod"
printf 'package cmds\n\n// Names 是支持的命令。\nvar Names = []string{"start", "status"}\n' > "$p/cmds/cmds.go"
printf 'package cmds\n\nimport "testing"\n\nfunc TestNames(t *testing.T) {\n\tif len(Names) != 2 {\n\t\tt.Fatal(Names)\n\t}\n}\n' > "$p/cmds/cmds_test.go"
printf '# 待办\n\n## 待做\n\n- [ ] 加 stop 命令 —— cmds 包的清单里加 stop\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

i=0
while [ $i -lt 60 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  # 合并提交先进 main，门的结果 last.json 最后才写：两样都在了再判
  last=$(ls "$HOME"/.agentq/gate/*/last.json 2>/dev/null | head -1)
  if [ -n "$last" ] && git -C "$p" log --format=%B main | grep -q '由 agentq 过门后合并'; then
    r=$(tr -d '\n ' < "$last")
    case "$r" in
      *'"name":"backend-count-hotspot","outcome":"fail"'*) exit 0 ;;
      *) echo "能力门没报出测试里跟着改了写死的数量：$(grep -o '"caps":[^]]*]' "$last" | head -1)"; exit 1 ;;
    esac
  fi
  sleep 1; i=$((i + 1))
done
echo "60 秒内没合并："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
