#!/bin/sh
# 11-11 两次放行都是门全过、只因为改了尺子（internal/setup/setup.go）等人看；信号是在「这类放行不记」之前按旧规则记的，
# 还把上一轮已经改好的 test 门拦截（没写门说明）当成被放行的门。11-15 配上 self_project 后复审旧提案，
# 把这条旧信号原样转进了待做「用户放行了test 门拦下的任务」。
# 要的是：记过的放行信号按现在的规则重判，尺子放行的去掉、旧提案标「已过时」不进待做；放行信号写这个任务自己被拦的门。
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

note='改了测试或门脚本，提交说明里没写原因（单写一行「门说明：原因」）'
returned() { # 任务号 指纹：test 门没过、退回了
  mkdir -p "$data/jobs/$1" "$data/gate/$1"
  printf '{"id":"%s","project":"agentq","title":"活 %s","fingerprint":"%s","status":"interrupted","gate":{"state":"returned","branch":"agent/%s"}}\n' "$1" "$2" "$2" "$1" > "$data/jobs/$1/meta.json"
  printf '{"outcome":"fail","head":"1234567890abcdef","results":[{"gate":"test","outcome":"fail","level":"error","summary":"%s"}]}\n' "$note" > "$data/gate/$1/last.json"
}
override() { # 任务号 指纹 放行理由 [hold]：用户放行后合并
  mkdir -p "$data/jobs/$1" "$data/gate/$1"
  printf '{"id":"%s","project":"agentq","title":"活 %s","fingerprint":"%s","status":"done","gate":{"state":"merged","passes":1,"override":"%s","merge":"abcdef0123456789"}}\n' "$1" "$2" "$2" "$3" > "$data/jobs/$1/meta.json"
  printf '{"outcome":"merged","merge":"abcdef0123456789","results":[{"gate":"override","outcome":"pass","level":"error","summary":"用户放行"}]}\n' > "$data/gate/$1/last.json"
  [ "$4" = hold ] && printf '{"results":[{"gate":"test","outcome":"pass","level":"error","summary":"过了"},{"gate":"eval","outcome":"pass","level":"error","summary":"没变差","hold":"改了评测题、判分或门的判定（internal/setup/setup.go）：评判的尺子不让候选自己改"}]}\n' > "$data/gate/$1/passed-0123456789abcdef.json"
  return 0
}

# 旧规则记下的：两次尺子放行凑成一条 test 门放行信号，提案只写进了 proposals.md
override 1111-202432-agentq fp-a "setup.go 只加提示，不动判定" hold
override 1111-211452-agentq fp-b "setup.go 只改帮助文字，不动判定" hold
day=$(date -u -v-1d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)
key="gate-override|test|$note"
cat > "$data/evolve/signals.json" <<JSON
{"checked":"2020-01-01T00:00:00Z","signals":[{"kind":"gate-override","key":"$key","summary":"用户放行了test 门拦下的任务：$note","evidence":["任务 1111-202432-agentq（agentq）","任务 1111-211452-agentq（agentq）"],"seen":2,"first":"$day","last":"$day","proposed":true}],"proposed_keys":["$key"]}
JSON
printf '# agentq 自我进化提案\n\n- 2026-11-11 22:09　agentq 自身：用户放行了门拦下的任务反复出现：用户放行了test 门拦下的任务：改了测试或门脚本，提交说明 —— 同类信号 2 次，11-11 到 11-11：用户放行了test 门拦下的任务：%s；证据：任务 1111-202432-agentq（agentq）、任务 1111-211452-agentq（agentq）；自我进化 L0 提案，只提案不改代码，做不做你定\n' "$note" > "$data/evolve/proposals.md"
# 新的：上一轮被 test 门退回、改好后这一轮门判不了，用户各自放行，理由不同
returned 1116-100000-agentq fp-c
returned 1116-110000-agentq fp-d
override 1116-100100-agentq fp-c "审查判不了，看过可以合并"
override 1116-110100-agentq fp-d "上游 403，看过没问题"

out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 1; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in *用户放行了*) echo "按旧规则记的尺子放行、各不相同的放行都不该进待做："; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
grep "用户放行了门拦下的任务反复出现" "$data/evolve/proposals.md" | grep -q "（已过时" || { echo "旧提案要标「已过时」："; cat "$data/evolve/proposals.md"; exit 1; }
if grep -q "任务 1111-202432-agentq" "$data/evolve/signals.json"; then
  echo "尺子放行的证据要从信号里去掉："; grep -n "1111-202432" "$data/evolve/signals.json"; exit 1
fi
if grep -q "用户放行了test 门" "$data/evolve/signals.json"; then
  echo "上一轮已经改好的 test 门拦截不是这次放行的门："; grep -n "用户放行了test 门" "$data/evolve/signals.json"; exit 1
fi
exit 0
