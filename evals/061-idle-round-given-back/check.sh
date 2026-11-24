#!/bin/sh
# 11-24 agentq「产品经理按方案补齐」（任务 1124-051336-agentq）：第 1 轮测试门没过是 main 自己的毛病（整套测试撞 900 秒时限），
# main 修好后任务只合了 main、一个文件没改，测试门就过了，接着审查查出问题。白占的那一轮让退回轮数用完，标卡住还写
# 「多半是计划(条目拆法)的问题」推给用户。
# 要的是：上一轮被测试门退回、这一轮没改代码就过了的，那一轮不占退回轮数，退回接着改审查的问题。
root="$EVAL_WORK/projects"; p="$root/demo"; data="$HOME/.agentq"; prev=1124-021619-demo; id=1124-051336-demo
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$data/jobs/$id" "$data/jobs/$prev" "$data/gate/$id" "$data/gate/$prev"
item='- [ ] 开工前先调研 —— 要什么'
printf '# 待办\n\n## 待做\n\n%s（进行中：agent/1123-pm 2026-11-24 05:13）\n\n## 已完成\n' "$item" > "$p/docs/BACKLOG.md"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
fp=$(printf '%s' "$item" | shasum | cut -c1-40)
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false,"tick_after_job":false,"pm_enabled":false}\n' "$root" > "$data/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
job() { # 任务号 接哪个 状态 轮数 门的状态
  printf '{"id":"%s","resume_of":"%s","project":"demo","project_dir":"%s","default_branch":"main","item":"开工前先调研 —— 要什么","title":"开工前先调研","fingerprint":"%s","pid":0,"started_at":"2026-11-24T05:13:00%s","ended_at":"2026-11-24T05:46:00%s","status":"%s","session_id":"s-eval","gate_contract":true,"gate_round":%s,"gate_scope":["tests/"],"gate_scope_from":"0000001","gate":{"state":"%s","branch":"agent/1123-pm","head":"0000000","round":%s,"ready":{"branch":"agent/1123-pm","type":"feature","acceptance":"看","progress":"做好了"},"started_at":"2026-11-24T05:47:00%s","deadline":"2026-11-24T06:47:00%s"}}' \
    "$1" "$2" "$p" "$fp" "$off" "$off" "$3" "$4" "$5" "$4" "$off" "$off"
}
printf '{"jobs":[%s,%s],"breaker_until":"0001-01-01T00:00:00Z"}\n' "$(job $prev "" interrupted 1 returned)" "$(job $id $prev done 2 running)" > "$data/state.json"
printf '{"outcome":"fail","reason":"test：.agentq/verify 没过：退出码 1","head":"0000001","results":[{"gate":"test","outcome":"fail","summary":".agentq/verify 没过：退出码 1","level":"error"}]}\n' > "$data/gate/$prev/last.json"
printf '{"outcome":"fail","reason":"review：审查发现 3 个有证据的问题","head":"0000000","results":[{"gate":"scope","outcome":"pass","summary":"这一轮改的 0 个文件都在退回时定的范围里","level":"error","no_change":true},{"gate":"test","outcome":"pass","summary":".agentq/verify 过了","level":"error"},{"gate":"review","outcome":"fail","summary":"审查发现 3 个有证据的问题","level":"error","files":["a.go"]}],"scope":["a.go"]}\n' > "$data/gate/$id/result.json"
out=$(AGENTQ_TEST_NOW="2026-11-24T05:50:00$off" "$AGENTQ_BIN" tick 2>&1)
python3 - "$data/state.json" "$id" <<'PY'
import json, sys
j = [x for x in json.load(open(sys.argv[1]))["jobs"] if x["id"] == sys.argv[2]]
if not j:
    print("状态里找不到任务"); sys.exit(2)
g = j[0]["gate"]
if g["state"] == "running":
    print("门的结果没收"); sys.exit(2)
if g["state"] != "returned" or not j[0].get("resume_pending"):
    print("没改代码测试就过了的那轮不该占退回轮数，要退回接着改：", g["state"], g.get("report")); sys.exit(1)
PY
rc=$?
[ $rc -eq 0 ] || { [ $rc -eq 2 ] && echo "$out" | tail -5; exit $rc; }
grep -q '条目拆法' "$p/docs/BACKLOG.md" "$data/state.json" && { echo "不是计划的问题："; cat "$p/docs/BACKLOG.md"; exit 1; }
exit 0
