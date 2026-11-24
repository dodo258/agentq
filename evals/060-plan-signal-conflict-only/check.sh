#!/bin/sh
# 11-24「退回用完还没过反复出现」又进了 agentq 的待做，两份证据里一份是冲突不占轮数以前的版本写的：最后一轮只因和 main 有冲突，
# 一道检查都没跑，报告写成「退回 2 轮还没过，多半是计划（条目拆法）的问题」。规则后来改了，记录里的报告没变，照旧凑数。
# 要的是：门只走到合并那一步、没有一道检查看过改动的不算退回用完；检查真看过、没过的照旧记。
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

stuck() { # 任务号 项目 最后一次的原因 各门的结论
  mkdir -p "$data/jobs/$1"
  printf '{"id":"%s","project":"%s","title":"活 %s","fingerprint":"fp-%s","status":"done","gate":{"state":"stuck","branch":"agent/%s","round":2,"report":"退回 2 轮还没过，多半是计划（条目拆法）的问题：建议重新拆或改验收。最后一次：%s","tally":%s}}\n' "$1" "$2" "$1" "$1" "$1" "$3" "$4" > "$data/jobs/$1/meta.json"
}
stuck 1123-000756-web web '和最新 main 有冲突：AGENTS.md、src/core/vendor.rs；从最新 main 合入，解决后再写 ready.json' '{"merge":{"fail":1}}'
stuck 1124-051336-agentq agentq 'review：审查发现 3 个有证据的问题' '{"test":{"pass":1},"review":{"fail":1}}'

out=$("$AGENTQ_BIN" tick 2>&1) || { echo "tick 报错：$out" | tail -3; exit 2; }
test -f "$data/evolve/signals.json" || { echo "没收信号：$out" | tail -3; exit 2; }
todo=$(sed -n '/^## 待做/,/^## 已完成/p' "$p/docs/BACKLOG.md")
case "$todo" in *退回用完*) echo "只撞上合并冲突不是条目拆法的问题，不该凑成反复出现进待做："; cat "$p/docs/BACKLOG.md"; exit 1 ;; esac
python3 - "$data/evolve/signals.json" <<'PY'
import json, sys
ev = [e for s in json.load(open(sys.argv[1])).get("signals", []) if s.get("kind") == "plan" for e in s.get("evidence", [])]
sys.exit(0 if len(ev) == 1 and "1124-051336-agentq" in ev[0] else 1)
PY
[ $? -eq 0 ] || { echo "退回用完只该记审查真没过的那次："; cat "$data/evolve/signals.json"; exit 1; }
exit 0
