#!/bin/sh
# 11-23 huitai：旧任务门 stuck（退回 2 轮），同一条目后来另起的任务做完合并了，agentq status 还在列「门没过，等你定」，
# notify.json 里还攒着同一张决定卡片，第二天早上会推给用户一个已经不用定的决定。
# 要的是：tick 后旧门标成已被取代，status 不再列，攒着的那张撤掉；同项目别的条目卡在门上照常列。
old=1123-000756-demo
other=1123-000900-demo
later=1123-002603-demo
dir="$EVAL_WORK/projects/demo"
mkdir -p "$dir/docs" "$HOME/.agentq"
printf '# 待办\n\n## 待做\n\n- [ ] 修东西 —— 要什么\n- [ ] 别的事 —— 要什么\n\n## 已完成\n' > "$dir/docs/BACKLOG.md"
git -C "$dir" init -q -b main && git -C "$dir" add -A && git -C "$dir" -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -q -m init ||
  { echo "建不了 git 项目，这次判不了"; exit 2; }
t1=$(date -u -v-3H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '-3 hours' +%Y-%m-%dT%H:%M:%SZ)
t2=$(date -u -v-2H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '-2 hours' +%Y-%m-%dT%H:%M:%SZ)
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$EVAL_WORK/projects" > "$HOME/.agentq/config.json"
stuck='{"id":"%s","project":"demo","project_dir":"%s","default_branch":"main","item":"%s","title":"%s","fingerprint":"%s","pid":0,"started_at":"%s","ended_at":"%s","status":"done","gate":{"state":"stuck","branch":"agent/1122-fix","head":"0123456789abcdef","round":2,"ready":{}}}'
j1=$(printf "$stuck" "$old" "$dir" "修东西" "修东西" fp-a "$t1" "$t1")
j2=$(printf "$stuck" "$other" "$dir" "别的事" "别的事" fp-b "$t1" "$t1")
j3=$(printf '{"id":"%s","project":"demo","project_dir":"%s","default_branch":"main","item":"修东西","title":"修东西","fingerprint":"fp-a","pid":0,"started_at":"%s","ended_at":"%s","status":"done","gate":{"state":"merged","branch":"agent/1122-fix","merge":"202013523e5c94c91bc2a14419c968b62e9578bf","ready":{}}}' "$later" "$dir" "$t2" "$t2")
printf '{"jobs":[%s,%s,%s]}\n' "$j1" "$j2" "$j3" > "$HOME/.agentq/state.json"
printf '{"pending":[{"cat":"decide","key":"gate:%s","job_id":"%s","text":"demo「修东西」门没过，等你定","at":"%s"},{"cat":"decide","key":"gate:%s","job_id":"%s","text":"demo「别的事」门没过，等你定","at":"%s"}]}\n' \
  "$old" "$old" "$t1" "$other" "$other" "$t1" > "$HOME/.agentq/notify.json"

"$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" status 2>&1) || { echo "agentq status 出错，这次判不了：$(printf '%s' "$out" | head -c 300)"; exit 2; }
case "$out" in
  *"等你定：demo「别的事」"*) ;;
  *) echo "没被取代的门不该撤：$(printf '%s' "$out" | head -c 400)"; exit 1 ;;
esac
case "$out" in
  *"等你定：demo「修东西」"*) echo "同一条目已经由任务 $later 合并了，status 还在列「等你定」"; exit 1 ;;
esac
grep -q "\"gate:$old\"" "$HOME/.agentq/notify.json" && { echo "notify.json 还攒着已经不用定的那张卡片"; exit 1; }
grep -q "\"gate:$other\"" "$HOME/.agentq/notify.json" || { echo "没被取代的那张卡片也被撤了"; exit 1; }
exit 0
