#!/bin/sh
# 11-15 23:44 任务 1115-234416-agentq 过门时，评测沙盒里 agentq setup 取锁定的第三方依赖失败（网络问题），
# 评测把 001-impl-go 的「这次判不了」记成变差，门退回了和它无关的分支；同一候选本机连跑两次都通过。
# 要的是：判不了的原因是取依赖、网络这类环境问题时不算变差（单列出来，门按判不了处理）；真变差（分数掉了）照旧算变差。
d="$HOME/.agentq/evals"
mkdir -p "$d/runs/1115-234416-env" "$d/runs/1115-234417-worse"
ok='"merged":true,"rounds":0,"human":0,"correct":true,"points":100,"tokens":0,"duration":0,"checks":[{"name":"合并","pass":true}]'
cat > "$d/baseline.json" <<JSON
{"updated":"2026-11-15T00:00:00Z","scores":[
 {"case":"001-impl-go","domain":"backend","kind":"task","mode":"fake",$ok},
 {"case":"002-fix-revert","domain":"backend","kind":"task","mode":"fake",$ok}]}
JSON
cat > "$d/runs/1115-234416-env/results.json" <<JSON
{"id":"1115-234416-env","mode":"fake","at":"2026-11-15T23:44:16Z","scores":[
 {"case":"001-impl-go","domain":"backend","kind":"task","mode":"fake","merged":false,"rounds":0,"human":0,"correct":false,"points":0,"tokens":0,"duration":0,
  "error":"agentq setup 退出码 1：…\"headless\": true, \"authenticated\": true } Support interpreter: Python 3.14.5 agentq: fetch locked third party: exit status 128"},
 {"case":"002-fix-revert","domain":"backend","kind":"task","mode":"fake",$ok}]}
JSON
cat > "$d/runs/1115-234417-worse/results.json" <<JSON
{"id":"1115-234417-worse","mode":"fake","at":"2026-11-15T23:44:17Z","scores":[
 {"case":"001-impl-go","domain":"backend","kind":"task","mode":"fake",$ok},
 {"case":"002-fix-revert","domain":"backend","kind":"task","mode":"fake","merged":false,"rounds":0,"human":0,"correct":false,"points":0,"tokens":0,"duration":0,"checks":[{"name":"合并","pass":false}]}]}
JSON

out=$("$AGENTQ_BIN" eval compare 1115-234416-env 2>&1)
code=$?
[ "$code" -eq 0 ] || { echo "取依赖失败判不了被算成了变差（退出码 $code）：$(echo "$out" | tail -3)"; exit 1; }
echo "$out" | grep -q '环境问题' || { echo "没写清是环境问题：$(echo "$out" | tail -3)"; exit 1; }

out=$("$AGENTQ_BIN" eval compare 1115-234417-worse 2>&1)
code=$?
[ "$code" -eq 1 ] || { echo "真变差没拦（退出码 $code）：$(echo "$out" | tail -3)"; exit 1; }
echo "$out" | grep -q '002-fix-revert：分数 100 → 0' || { echo "变差没写清：$(echo "$out" | tail -3)"; exit 1; }
