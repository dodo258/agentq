#!/bin/sh
# 11-16：改到尺子、新增删数据语句这两类门停住、其余门都过了的，先让独立审查代替用户看，不再直接推卡片。
# 三种情况：删数据的语句只在注释里（误报），它放行就自动合并，审计行和报告写「独立审查代放行」；
# 它看出改动让门变松（有疑问）、或者判不了，都不合并，停下等用户。
root="$EVAL_WORK/projects"; fake="$EVAL_WORK/claude"
mkdir -p "$HOME/.agentq"
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
out() { printf '{"type":"result","subtype":"success","is_error":false,"result":"看完了","session_id":"s-rv","structured_output":%s}\n' "$1"; exit 0; }
case "$prompt" in
  /usage) echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; exit 0 ;;
  *独立审查代*场景甲*) out '{"verdict":"pass","summary":"删数据的那句只在注释里，不会执行","findings":[]}' ;;
  *独立审查代*场景乙*) out '{"verdict":"doubt","summary":"这次把没跑完的检查也算通过，门变松了","findings":[{"file":"clean.sh","line":2,"kind":"loosen","evidence":"超时当通过"}]}' ;;
  *独立审查代*场景丙*) out '{"verdict":"unknown","summary":"条目没说清这张表该不该删","findings":[]}' ;;
  *审查门的独立审查进程*) out '{"verdict":"pass","summary":"没问题","findings":[]}' ;;
esac
b=agent/clean; wt=$PWD/.claude/worktrees/clean
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
printf '#!/bin/sh\n# 以前这里跑过 DELETE FROM jobs，现在不删了，只打印\necho ok\n' > "$wt/clean.sh"
mkdir -p "$wt/.agentq"
printf '{"branch":"%s","type":"feature","acceptance":"看 clean.sh","progress":"清理脚本只打印"}\n' "$b" > "$wt/.agentq/ready.json"
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
for s in 甲 乙 丙; do
  p="$root/p$s"
  mkdir -p "$p/.agentq" "$p/docs"
  printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
  chmod 755 "$p/.agentq/verify"
  printf '# 待办\n\n## 待做\n\n- [ ] 清理脚本只打印 —— 场景%s\n\n## 已完成\n' "$s" > "$p/docs/BACKLOG.md"
  printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
  printf '.claude/worktrees/\n' > "$p/.gitignore"
  (cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
done
printf '{"projects_root":"%s","claude_path":"%s","max_parallel":3,"per_project":1,"patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

# 第一次 tick 三个项目都接上活，接上就暂停：门照常过，现场不会在判完分后还有人往里写
"$AGENTQ_BIN" tick >/dev/null 2>&1
for s in 甲 乙 丙; do
  grep -q '（进行中' "$root/p$s/docs/BACKLOG.md" || { echo "第一次 tick 没接上 p$s 的活：$("$AGENTQ_BIN" status 2>&1 | tail -3)"; exit 2; }
done
"$AGENTQ_BIN" pause >/dev/null 2>&1 || { echo "pause 失败"; exit 2; }

# outcome <项目>：这个项目那次过门的结论（last.json），还没有是空
outcome() {
  for d in "$HOME/.agentq/gate"/*/; do
    grep -q "\"project\": *\"$1\"" "$d/plan.json" 2>/dev/null || continue
    [ -f "$d/last.json" ] && tr -d '\n ' < "$d/last.json" && return
  done
}
i=0
while [ $i -lt 60 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  a=$(outcome p甲); b=$(outcome p乙); c=$(outcome p丙)
  if [ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ]; then break; fi
  sleep 1; i=$((i + 1))
done
[ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ] || { echo "60 秒内三个项目没都过完门："; "$AGENTQ_BIN" status 2>&1 | tail -8; exit 1; }

case "$a" in
  *'"outcome":"merged"'*'"gate":"holdreview","outcome":"pass"'*|*'"gate":"holdreview","outcome":"pass"'*'"outcome":"merged"'*) ;;
  *'"outcome":"hold"'*) echo "误报（删数据的语句只在注释里）：独立审查代放行了，还是停下等用户"; exit 1 ;;
  *) echo "误报那一条没有经独立审查代放行合并：$a" | cut -c1-400; exit 1 ;;
esac
[ -f "$root/p甲/clean.sh" ] || { echo "放行了但改动没进主分支"; exit 1; }
grep -q "独立审查代放行 .* p甲「" "$HOME/.agentq/gate/audit.log" 2>/dev/null || { echo "审计日志看不出是独立审查代放行：$(cat "$HOME/.agentq/gate/audit.log" 2>/dev/null)"; exit 1; }
id=$(basename "$(dirname "$(grep -l '"project": *"p甲"' "$HOME/.agentq/gate"/*/plan.json | head -1)")")
"$AGENTQ_BIN" gate report "$id" 2>&1 | grep -q '独立审查代放行' || { echo "gate report 看不出是独立审查代放行：$("$AGENTQ_BIN" gate report "$id" 2>&1 | tail -5)"; exit 1; }

for x in "乙:$b:有疑问" "丙:$c:判不了"; do
  s=${x%%:*}; rest=${x#*:}; res=${rest%:*}; word=${rest##*:}
  case "$res" in
    *'"outcome":"merged"'*) echo "p$s：独立审查代$word，门还是合并了"; exit 1 ;;
    *'"outcome":"hold"'*"独立审查代$word"*) ;;
    *) echo "p$s：独立审查代${word}时要停下等用户，结论里写它的话：$res" | cut -c1-400; exit 1 ;;
  esac
  [ -f "$root/p$s/clean.sh" ] && { echo "p$s：没放行，改动却进了主分支"; exit 1; }
done
grep -q "独立审查代放行 .* p[乙丙]「" "$HOME/.agentq/gate/audit.log" && { echo "没放行的也写了独立审查代放行"; exit 1; }
exit 0
