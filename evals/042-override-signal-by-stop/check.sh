#!/bin/sh
# 11-13 repo-tidy 是审查撞 Cloudflare 403 判不了、11-16 gps-tracker 是门都过了只因「难撤回」（把 Tailwind 类名 truncate 当成 TRUNCATE）
# 等人看，两次都点 Telegram 按钮放行。放行理由都是「Telegram 按钮放行」，放行信号按理由归类，凑成「反复出现」写进了待做。
# 要的是：放行信号按门当时停在哪归类（判不了的门、难撤回等人看），放行理由不进归类；已经按理由凑成的旧信号重判后去掉。
root="$EVAL_WORK/projects"; p="$root/agentq"; fake="$EVAL_WORK/claude"; data="$HOME/.agentq"
mkdir -p "$p/docs" "$data/evolve"
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
printf '# 待办\n\n## 待做\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
git -C "$p" init -q -b main && git -C "$p" add -A && git -C "$p" commit -q -m init || exit 1
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"pm_enabled":false,"self_project":"agentq","exclude_projects":["agentq"]}\n' "$root" "$fake" > "$data/config.json"

button() { # 任务号 项目 指纹 tally risk：Telegram 按钮放行后合并
  mkdir -p "$data/jobs/$1" "$data/gate/$1"
  printf '{"id":"%s","project":"%s","title":"活 %s","fingerprint":"%s","status":"done","gate":{"state":"merged","passes":1,"override":"Telegram 按钮放行","merge":"abcdef0123456789","tally":%s}}\n' "$1" "$2" "$3" "$3" "$4" > "$data/jobs/$1/meta.json"
  printf '{"outcome":"merged","merge":"abcdef0123456789","risk":%s,"results":[{"gate":"override","outcome":"pass","level":"error","summary":"用户放行：Telegram 按钮放行"}]}\n' "$5" > "$data/gate/$1/last.json"
}
# 审查判不了（上游 403）后放行
button 1113-180958-repo-tidy repo-tidy fp-a '{"test":{"pass":1},"review":{"unknown":1}}' '{"level":"broad","reasons":["改了 19 个文件（超过 15 个）"]}'
# 门都过了，难撤回等人看后放行
button 1116-142607-gps-tracker gps-tracker fp-b '{"test":{"pass":1},"review":{"pass":1},"ui":{"pass":1}}' '{"level":"irreversible","reasons":["新增行有删数据的语句：parent-web/src/components/PlaceList.tsx（TRUNCATE）"]}'
printf '{"results":[{"gate":"test","outcome":"pass","level":"error","summary":"过了"},{"gate":"review","outcome":"pass","level":"error","summary":"按方案实现"}]}\n' > "$data/gate/1116-142607-gps-tracker/passed-0123456789abcdef.json"

out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 1; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in *用户放行了*) echo "门停的原因不同的两次按钮放行不该凑成反复出现进待做："; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
if grep -q "Telegram 按钮放行" "$data/evolve/signals.json"; then
  echo "放行理由不该进放行信号的归类："; grep -n "Telegram" "$data/evolve/signals.json"; exit 1
fi
grep -q "用户放行了review 门拦下的任务：判不了" "$data/evolve/signals.json" || { echo "放行信号要写停在哪道门："; cat "$data/evolve/signals.json"; exit 1; }
grep -q "用户放行了门拦下的任务：难撤回" "$data/evolve/signals.json" || { echo "门都过了等人看的要写风险档位："; cat "$data/evolve/signals.json"; exit 1; }
exit 0
