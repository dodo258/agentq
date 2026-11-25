#!/bin/sh
# 11-18 到 11-21：任务只加了文档和测试目录里的样例 sql（docs/ 下模拟器造数据的 plant.sql、tests/fixtures/ 下的脱敏样例），
# 测试、审查都过了，门按「路径命中 *.sql」难撤回停下等用户拍板，三次都是用户手动放行。
# 要的是：文档和测试文件里的 sql 不会在用户数据上执行，照常合并；真的 db/ 下的 sql 照旧停下等用户。
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
  *审查门的独立审查进程*) out '{"verdict":"pass","summary":"没问题","findings":[]}' ;;
  *独立审查代*) out '{"verdict":"unknown","summary":"不该走到这里","findings":[]}' ;;
esac
b=agent/sql; wt=$PWD/.claude/worktrees/sql
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
case "$prompt" in
  *场景甲*)
    mkdir -p "$wt/tests/fixtures/codex" "$wt/app/docs/emulator"
    printf 'CREATE TABLE threads (id TEXT);\nINSERT INTO threads VALUES (%s);\n' "'t1'" > "$wt/tests/fixtures/codex/state.sql"
    printf 'INSERT INTO points VALUES (1, 2);\n' > "$wt/app/docs/emulator/plant.sql" ;;
  *)
    mkdir -p "$wt/db"
    printf 'CREATE TABLE threads (id TEXT);\n' > "$wt/db/schema.sql" ;;
esac
mkdir -p "$wt/.agentq"
printf '{"branch":"%s","type":"feature","acceptance":"看 sql 文件","progress":"加了 sql"}\n' "$b" > "$wt/.agentq/ready.json"
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
for s in 甲 乙; do
  p="$root/p$s"
  mkdir -p "$p/.agentq" "$p/docs"
  printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
  chmod 755 "$p/.agentq/verify"
  printf '# 待办\n\n## 待做\n\n- [ ] 加一份 sql —— 场景%s\n\n## 已完成\n' "$s" > "$p/docs/BACKLOG.md"
  printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
  printf '.claude/worktrees/\n' > "$p/.gitignore"
  (cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
done
printf '{"projects_root":"%s","claude_path":"%s","max_parallel":2,"per_project":1,"patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

# 第一次 tick 两个项目都接上活，接上就暂停：门照常过，现场不会在判完分后还有人往里写
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
for s in 甲 乙; do
  claimed "$root/p$s" || { echo "第一次 tick 没接上 p$s 的活：$("$AGENTQ_BIN" status 2>&1 | tail -20)"; exit 2; }
done

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
  a=$(outcome p甲); b=$(outcome p乙)
  if [ -n "$a" ] && [ -n "$b" ]; then break; fi
  sleep 1; i=$((i + 1))
done
[ -n "$a" ] && [ -n "$b" ] || { echo "60 秒内两个项目没都过完门："; "$AGENTQ_BIN" status 2>&1 | tail -8; exit 1; }

case "$a" in
  *'"outcome":"merged"'*) ;;
  *'"outcome":"hold"'*) echo "只加了文档和测试目录里的样例 sql，门还是按难撤回停下等用户：$a" | cut -c1-400; exit 1 ;;
  *) echo "样例 sql 那一条没合并：$a" | cut -c1-400; exit 1 ;;
esac
[ -f "$root/p甲/tests/fixtures/codex/state.sql" ] || { echo "判了合并，样例 sql 没进主分支"; exit 1; }
case "$b" in
  *'"outcome":"hold"'*'*.sql'*) ;;
  *'"outcome":"merged"'*) echo "db/ 下的 sql 要照旧停下等用户，门却合并了"; exit 1 ;;
  *) echo "db/ 下的 sql 要按路径命中 *.sql 停下等用户：$b" | cut -c1-400; exit 1 ;;
esac
[ -f "$root/p乙/db/schema.sql" ] && { echo "没放行，db/schema.sql 却进了主分支"; exit 1; }
exit 0
