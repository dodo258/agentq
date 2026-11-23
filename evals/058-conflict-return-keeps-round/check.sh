#!/bin/sh
# 11-23 huitai「OpenCode 适配器」（任务 1123-000756-huitai）：第 1 轮是真审查问题，第 2 轮只因并行任务先合了 main，
# 门报「和最新 main 有冲突」，退回轮数用完，标卡住还写「多半是计划(条目拆法)的问题」推给用户；去掉卡住标记后什么都没改就合上了。
# 要的是：只因和最新默认分支冲突退回，不占退回轮数、不说是计划的问题，带原会话退回接着合；连续冲突到上限才停，原因写 main 一直在动。
root="$EVAL_WORK/projects"; p="$root/demo"; data="$HOME/.agentq"; id=1123-000756-demo
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$data/jobs/$id" "$data/gate/$id"
item='- [ ] OpenCode 适配器 —— 要什么'
printf '# 待办\n\n## 待做\n\n%s（进行中：agent/1122-opencode 2026-11-23 00:07）\n\n## 已完成\n' "$item" > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
fp=$(printf '%s' "$item" | shasum | cut -c1-40)
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false,"tick_after_job":false,"pm_enabled":false}\n' "$root" > "$data/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$data/state.json" <<STATE
{"jobs":[
 {"id":"$id","project":"demo","project_dir":"$p","default_branch":"main","item":"OpenCode 适配器 —— 要什么","title":"OpenCode 适配器","fingerprint":"$fp","pid":0,
  "started_at":"2026-11-23T00:07:56$off","ended_at":"2026-11-23T00:12:00$off","status":"done","session_id":"s-eval","gate_contract":true,"gate_round":2,
  "gate":{"state":"running","branch":"agent/1122-opencode","head":"0000000","round":2,"ready":{"branch":"agent/1122-opencode","type":"feature","acceptance":"看","progress":"做好了"},
   "started_at":"2026-11-23T00:12:00$off","deadline":"2026-11-23T01:12:00$off"}}
],"breaker_until":"0001-01-01T00:00:00Z"}
STATE
why='和最新 main 有冲突：AGENTS.md、src/core/vendor.rs；从最新 main 合入，解决后再写 ready.json'
printf '{"outcome":"fail","reason":"%s","head":"0000000","conflict":true,"results":[{"gate":"merge","outcome":"fail","summary":"%s","level":"error","files":["AGENTS.md","src/core/vendor.rs"]}],"scope":["AGENTS.md","src/core/vendor.rs"]}\n' "$why" "$why" > "$data/gate/$id/result.json"
out=$(AGENTQ_TEST_NOW="2026-11-23T00:14:00$off" "$AGENTQ_BIN" tick 2>&1)
st=$(tr -d '\n' < "$data/state.json")
case "$st" in *"\"id\": \"$id\""*) ;; *) echo "状态里找不到任务：$out" | tail -5; exit 2 ;; esac
case "$st" in *'"state": "running"'*) echo "门的结果没收：$out" | tail -5; exit 2 ;; esac
case "$st" in
  *'"state": "returned"'*) ;;
  *) echo "最后一轮只因和 main 冲突，要退回接着合，不该停下：$(printf '%s' "$st" | grep -o '"report": "[^"]*"')"; exit 1 ;;
esac
grep -q '条目拆法' "$p/docs/BACKLOG.md" "$data/state.json" && { echo "冲突不是计划的问题："; cat "$p/docs/BACKLOG.md"; exit 1; }
case "$st" in *'"resume_pending": true'*) ;; *) echo "要用原会话续跑：$st" | tail -c 600; exit 1 ;; esac
exit 0
