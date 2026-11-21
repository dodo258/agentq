#!/bin/sh
# 11-16 到 11-21「退回用完还没过反复出现」进了 agentq 的待做，三份证据里只有一次是改动真没过：一次是评测题布置不起来
# （agentq setup 退出码 1，检查自己判不了），一次是门都过了只因路径命中 *.sql 难撤回等人拍板。门停下时轮数用完了，
# 自我进化就当成条目拆法的问题，凑够两次写了待做，摘要还把「退回 N 轮还没过」包了两层。
# 要的是：只有门写了「退回 N 轮还没过，多半是计划……」的才算退回用完；等拍板、判不了的照旧是卡在门上，不进 agentq 的待做。
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
git -C "$p" init -q -b main && git -C "$p" add -A && git -C "$p" commit -q -m init || exit 2
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"notify_enabled":false,"patrol_enabled":false,"pm_enabled":false,"self_project":"agentq","exclude_projects":["agentq"]}\n' "$root" "$fake" > "$data/config.json"

stuck() { # 任务号 项目 门的报告：门停下等用户定，轮数已经用完
  mkdir -p "$data/jobs/$1"
  printf '{"id":"%s","project":"%s","title":"活 %s","fingerprint":"fp-%s","status":"interrupted","gate":{"state":"stuck","branch":"agent/%s","round":2,"report":"%s"}}\n' "$1" "$2" "$1" "$1" "$1" "$3" > "$data/jobs/$1/meta.json"
}
stuck 1115-234416-agentq agentq '退回 2 轮还没过，多半是计划（条目拆法）的问题：建议重新拆或改验收。最后一次：review：审查发现 1 个有证据的问题'
stuck 1121-060257-huitai huitai '难撤回，门过了也不自动合并，等你看过再放行：路径命中 *.sql：tests/fixtures/codex/state.sql'
stuck 1121-070000-web web 'eval：评测比基线变差 1 处：001-impl-go：这次判不了：agentq setup 退出码 1'

out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 2; }
test -f "$data/evolve/signals.json" || { echo "没收信号：$out" | tail -3; exit 2; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in *退回用完*) echo "等拍板、判不了不是条目拆法的问题，不该凑成反复出现进待做："; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
for id in 1121-060257-huitai 1121-070000-web; do
  if python3 -c "import json,sys;d=json.load(open(sys.argv[1]));sys.exit(0 if any(s.get('kind')=='plan' and any(sys.argv[2] in e for e in s.get('evidence',[])) for s in d.get('signals',[])) else 1)" "$data/evolve/signals.json" "$id"; then
    echo "$id 不是改动没过，不该记成退回用完："; cat "$data/evolve/signals.json"; exit 1
  fi
done
grep -q "多半是条目拆法或验收的问题：退回" "$data/evolve/signals.json" && { echo "摘要把门的报告又包了一层："; cat "$data/evolve/signals.json"; exit 1; }
grep -q '1115-234416-agentq' "$data/evolve/signals.json" || { echo "改动真没过的要记下："; cat "$data/evolve/signals.json"; exit 1; }
exit 0
