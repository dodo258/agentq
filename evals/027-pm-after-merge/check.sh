#!/bin/sh
# 11-13 用户要 agentq 像产品经理一样想下一步：项目做完一版（有任务过门合并、待做清空）后，起一个只读联网的调研任务，
# 写 1–3 条带出处的下一步进「## 建议」（不进待做）；再 tick 不重复起；exclude_projects 里的项目不起。
root="$EVAL_WORK/projects"; fake="$EVAL_WORK/claude"; calls="$EVAL_WORK/claude-calls"; data="$HOME/.agentq"
mkdir -p "$data"
printf '[user]\n\tname = t\n\temail = t%sexample.invalid\n[commit]\n\tgpgsign = false\n' @ > "$HOME/.gitconfig"
cat > "$fake" <<FAKE
#!/bin/sh
case "\$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
[ "\$2" = "/usage" ] && exit 0
echo \$PPID >> "\$HOME/fake-shells"
echo "PWD=\$PWD \$*" | tr '\n' ' ' >> "$calls"; echo >> "$calls"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-eval","structured_output":{"suggestions":[{"title":"加导出 CSV","why":"同类都有，用户在 issue 里要","source":"https://github.com/example/clipper/issues/12"}]}}'
FAKE
chmod 755 "$fake"
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
off=$(date +%z | sed 's/\(..\)$/:\1/')
jobs=""
for name in demo ex; do
  p="$root/$name"; mkdir -p "$p/docs"
  printf '# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 第一条 —— 合并提交 abc1234\n' > "$p/docs/BACKLOG.md"
  printf '.claude/\n' > "$p/.gitignore"
  (cd "$p" && git init -q -b main && git add -A && git commit -q -m init) || exit 2
  jobs="$jobs{\"id\":\"1112-100000-$name\",\"project\":\"$name\",\"project_dir\":\"$p\",\"item\":\"第一条\",\"title\":\"第一条\",\"fingerprint\":\"fp-$name\",\"pid\":0,
   \"started_at\":\"2026-11-12T10:00:00$off\",\"ended_at\":\"2026-11-12T10:30:00$off\",\"status\":\"done\",\"summary\":\"合并提交 abc1234\",
   \"gate\":{\"state\":\"merged\",\"branch\":\"agent/1112-first\",\"merge\":\"abc1234\"}},"
done
printf '{"jobs":[%s],"breaker_until":"0001-01-01T00:00:00Z"}\n' "${jobs%,}" > "$data/state.json"
printf '{"projects_root":"%s","claude_path":"%s","notify_enabled":false,"patrol_enabled":false,"exclude_projects":["ex"],"tick_after_job":false}\n' "$root" "$fake" > "$data/config.json"
tick() { AGENTQ_TEST_NOW="2026-11-12T12:00:00$off" "$AGENTQ_BIN" tick 2>&1; }

out=$(tick) || { echo "tick 报错：$out" | tail -3; exit 1; }
wait_shells
grep -q 'WebSearch' "$calls" 2>/dev/null || { echo "做完一版、待做清空后没起调研：$out" | tail -3; exit 1; }
line=$(grep 'WebSearch' "$calls" | head -1)
case "$line" in
  *"PWD=$root/demo "*|*"/projects/demo "*) ;;
  *) echo "调研要在 demo 的目录里跑：$line"; exit 1 ;;
esac
case "$line" in
  *"--tools Read,Grep,Glob,WebSearch,WebFetch"*) ;;
  *) echo "调研只给读文件和联网两类工具：$line"; exit 1 ;;
esac
case "$line" in *Bash*|*Write*|*Edit*|*"--permission-mode"*) echo "调研不能写文件、跑命令：$line"; exit 1 ;; esac
tick >/dev/null
b=$(cat "$root/demo/docs/BACKLOG.md")
case "$b" in
  *"## 建议"*"- [ ] 加导出 CSV —— 同类都有，用户在 issue 里要；出处：https://github.com/example/clipper/issues/12"*"（建议 "*) ;;
  *) echo "建议要带出处写进「## 建议」："; echo "$b"; exit 1 ;;
esac
sed -n '/^## 待做/,/^## /p' "$root/demo/docs/BACKLOG.md" | grep -q '加导出 CSV' && { echo "不能直接写进待做"; exit 1; }
git -C "$root/demo" status --porcelain | grep -q . && { echo "写进建议要提交"; exit 1; }
tick >/dev/null; wait_shells
n=$(grep -c 'WebSearch' "$calls")
[ "$n" = 1 ] || { echo "同一版只调研一次，起了 $n 次"; exit 1; }
grep -q "/ex " "$calls" && { echo "exclude_projects 里的项目不调研"; exit 1; }
exit 0
