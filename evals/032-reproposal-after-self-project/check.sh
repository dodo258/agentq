#!/bin/sh
# 11-15 self_project 一直没配，自我进化提的 7 条只写进 ~/.agentq/evolve/proposals.md，Key 记成提过后永远不再提：配上 self_project 也进不了「待做」。
# 另外 12 条「用户放行了门拦下的任务」大多是门都过了、只因为改了尺子等人看的放行，那是设计好的人工把关，不是 agentq 的问题。
# 要的是：配上后第一次收信号，近 14 天还出现过的旧提案进待做一次，对不上的标「已过时」；改了尺子等人放行的不记信号，门判不了后放行的照记。
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
printf '# 待办\n\n## 待做\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
git -C "$p" init -q -b main && git -C "$p" add -A && git -C "$p" commit -q -m init || exit 1
conf() {
  printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"pm_enabled":false,"self_project":"%s","exclude_projects":["agentq"]}\n' "$root" "$fake" "$1" > "$data/config.json"
}
job() { # 任务号 状态 门记录
  mkdir -p "$data/jobs/$1"
  printf '{"id":"%s","project":"web","title":"活 %s","fingerprint":"fp-%s","status":"%s","summary":"跑了四个小时没结束"%s}\n' "$1" "$1" "$1" "$2" "$3" > "$data/jobs/$1/meta.json"
}
override() { # 任务号 有没有「门都过了、改了尺子要人看」的记录
  job "$1" done ',"gate":{"state":"merged","passes":1,"override":"看过，判定没变","merge":"abcdef0123456789"}'
  mkdir -p "$data/gate/$1"
  printf '{"outcome":"merged","merge":"abcdef0123456789","results":[{"gate":"override","outcome":"pass","level":"error","summary":"用户放行"}]}\n' > "$data/gate/$1/last.json"
  [ "$2" = hold ] && printf '{"results":[{"gate":"test","outcome":"pass","level":"error","summary":"过了"},{"gate":"eval","outcome":"pass","level":"error","summary":"没变差","hold":"改了评测题、判分或门的判定（internal/evolve/judge.go）：评判的尺子不让候选自己改"}]}\n' > "$data/gate/$1/passed-0123456789abcdef.json"
}

conf ""
job 1101-100000-web timeout ""
job 1102-100000-web timeout ""
out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 1; }
grep -q "agentq 自身：任务超时反复出现" "$data/evolve/proposals.md" 2>/dev/null || { echo "没配 self_project 时提案要写进 proposals.md"; cat "$data/evolve/proposals.md" 2>&1; exit 1; }
printf -- '- 2026-10-20 09:00　agentq 自身：巡逻发现问题反复出现：早就不报了 —— 同类信号 2 次；证据：巡逻 gone:1 10-19 08:00\n' >> "$data/evolve/proposals.md"

conf agentq
override 1103-100000-web hold
override 1104-100000-web hold
override 1105-100000-web ""
override 1106-100000-web ""
sed 's/"checked": "[^"]*"/"checked": "2020-01-01T00:00:00Z"/' "$data/evolve/signals.json" > "$data/evolve/s.tmp" && mv "$data/evolve/s.tmp" "$data/evolve/signals.json"
out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 1; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in
  *"- [ ] agentq 自身：任务超时反复出现"*) ;;
  *) echo "配上 self_project 后，只写进 proposals.md、还在出现的旧提案要进待做："; cat "$p/docs/BACKLOG.md"; exit 1 ;;
esac
[ "$(printf '%s\n' "$todo" | grep -c '任务超时反复出现')" = 1 ] || { echo "旧提案只进一次："; cat "$p/docs/BACKLOG.md"; exit 1; }
case "$todo" in *早就不报了*) echo "对不上信号的旧提案不进待做"; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
grep "早就不报了" "$data/evolve/proposals.md" | grep -q "（已过时" || { echo "过时的旧提案要标「已过时」："; cat "$data/evolve/proposals.md"; exit 1; }
case "$todo" in *"任务 1105-100000-web"*) ;; *) echo "门判不了后用户放行的要照记（够两次进待做）："; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
if grep -q "任务 1103-100000-web" "$data/evolve/signals.json"; then
  echo "门都过了、只因为改了尺子等人看的放行不记信号："; grep -n "任务 1103-100000-web" "$data/evolve/signals.json"; exit 1
fi
git -C "$p" status --porcelain | grep -q . && { echo "写进待做要提交"; exit 1; }

before=$(cat "$p/docs/BACKLOG.md")
job 1107-100000-web done ""
sed 's/"checked": "[^"]*"/"checked": "2020-01-01T00:00:00Z"/' "$data/evolve/signals.json" > "$data/evolve/s.tmp" && mv "$data/evolve/s.tmp" "$data/evolve/signals.json"
"$AGENTQ_BIN" tick >/dev/null 2>&1
[ "$(cat "$p/docs/BACKLOG.md")" = "$before" ] || { echo "复审只做一次："; cat "$p/docs/BACKLOG.md"; exit 1; }
exit 0
