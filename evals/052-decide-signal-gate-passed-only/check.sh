#!/bin/sh
# 11-16 agentq 的任务门都过了，只因改了评测的尺子等人看，用户看出合进最新 main 后题数要改，在门卡片上退回；
# 11-21 huitai 的任务审查门退回用完还没过，用户在卡片上补了做法再退回。门卡片退回的信号只按动作归类，
# 两次凑成「用户在门卡片上退回或放弃反复出现」写进了 agentq 的待做，没有可修的共同原因。
# 要的是：只有门都过了、等人拍板时用户不认的才算，按停下等人看的原因归；门自己挡下后照门的结论退回不算（已记成退回用完）。
root="$EVAL_WORK/projects"; p="$root/agentq"; fake="$EVAL_WORK/claude"; data="$HOME/.agentq"
mkdir -p "$p/docs" "$data/evolve" "$data/gate"
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
git -C "$p" init -q -b main && git -C "$p" add -A && git -C "$p" commit -q -m init || exit 2
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"pm_enabled":false,"self_project":"agentq","exclude_projects":["agentq"]}\n' "$root" "$fake" > "$data/config.json"

parked() { # 任务号 项目 标题 门的报告 last.json 退回轮数：停在门上等用户定
  mkdir -p "$data/jobs/$1" "$data/gate/$1"
  printf '{"id":"%s","project":"%s","title":"%s","fingerprint":"fp-%s","status":"interrupted","gate":{"state":"stuck","branch":"agent/%s","round":%s,"report":"%s"}}\n' "$1" "$2" "$3" "$1" "$1" "$6" "$4" > "$data/jobs/$1/meta.json"
  printf '%s\n' "$5" > "$data/gate/$1/last.json"
}
parked 1116-122432-agentq agentq '升级后任务变差' '改了评测题、判分或门的判定（internal/evolve/upgrade.go）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行' \
  '{"outcome":"hold","reason":"改了评测题、判分或门的判定（internal/evolve/upgrade.go）","risk":{"level":"broad","reasons":["跨 4 个顶层目录：docs、evals、internal、tests"]},"results":[{"gate":"test","outcome":"pass","level":"error"},{"gate":"eval","outcome":"pass","level":"error","hold":"改了评测题、判分或门的判定（internal/evolve/upgrade.go）"},{"gate":"review","outcome":"pass","level":"error"}]}' 0
parked 1121-034521-huitai huitai '守护进程接上通知与全局呼出键' '退回 2 轮还没过，多半是计划（条目拆法）的问题：建议重新拆或改验收。最后一次：review：审查发现 1 个有证据的问题' \
  '{"outcome":"fail","reason":"review：审查发现 1 个有证据的问题","results":[{"gate":"test","outcome":"pass","level":"error"},{"gate":"review","outcome":"fail","level":"error","summary":"审查发现 1 个有证据的问题"}]}' 2
at=$(date -v-1H +%Y-%m-%dT%H:%M:%S%z 2>/dev/null || date -d '-1 hour' +%Y-%m-%dT%H:%M:%S%z) || exit 2
at=$(printf '%s' "$at" | sed 's/\([0-9][0-9]\)$/:\1/')
{
  printf '%s 退回 1116-122432-agentq agentq「升级后任务变差」分支 agent/1116-upgrade-carried-rounds：从最新 main 合入，改题数\n' "$at"
  printf '%s 退回 1121-034521-huitai huitai「守护进程接上通知与全局呼出键」分支 agent/1121-daemon-notify-hotkey：像 tmux 的 focus-events 那样做\n' "$at"
} > "$data/gate/audit.log"

out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 2; }
test -f "$data/evolve/signals.json" || { echo "没收信号：$out" | tail -3; exit 2; }
grep -q '1116-122432-agentq' "$data/evolve/signals.json" || { echo "门都过了用户还退回的要记下（没读到审计日志就判不了）："; cat "$data/evolve/signals.json"; exit 2; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in *门卡片上退回或放弃*) echo "门挡下后照门的结论退回，不该和门都过了的那次凑成反复出现进待做："; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
if python3 -c "import json,sys;d=json.load(open(sys.argv[1]));sys.exit(0 if any(s.get('kind')=='user-decide' and any('1121-034521-huitai' in e for e in s.get('evidence',[])) for s in d.get('signals',[])) else 1)" "$data/evolve/signals.json"; then
  echo "门自己挡下的退回不该记成门卡片退回："; cat "$data/evolve/signals.json"; exit 1
fi
grep -q '门都过了，改了评测的尺子' "$data/evolve/signals.json" || { echo "要按停下等人看的原因归："; cat "$data/evolve/signals.json"; exit 1; }
exit 0
