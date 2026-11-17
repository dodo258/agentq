#!/bin/sh
# 11-14、11-15 两天的定时备份都有一个仓库推不上（那个仓库的备份仓在 GitHub 上被归档成只读），巡逻每天报一次「今天的备份有 1 个仓库推送失败」。
# 自我进化把这两次当成 agentq 自己的问题反复出现，给 agentq 写了一条待做，可原因在外部备份脚本和仓库，改 agentq 修不好。
# 要的是：巡逻的备份报告失败和自动推送失败一样不进 agentq 的自身信号；同一批里 agentq 自己的问题（早报没生成）照记。
root="$EVAL_WORK/projects"
mkdir -p "$root" "$HOME/.agentq"
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
off=$(date +%z | sed 's/\(..\)$/:\1/')
cat > "$HOME/.agentq/patrol.json" <<PATROL
{"last_run":"2026-11-15T06:06:00$off","issues":[
 {"fingerprint":"backup:2026-11-14","check":"backup","anomaly":true,"title":"今天的备份有 1 个仓库推送失败","evidence":["备份报告第一行：**1 个失败**"],"action":"没自动处理","first_seen":"2026-11-14T06:05:00$off"},
 {"fingerprint":"backup:2026-11-15","check":"backup","anomaly":true,"title":"今天的备份有 1 个仓库推送失败","evidence":["备份报告第一行：**1 个失败**"],"action":"没自动处理","first_seen":"2026-11-15T06:06:00$off"},
 {"fingerprint":"report:review:2026-11-15","check":"report","anomaly":true,"title":"08:00 后还没有今天的早报","evidence":["没有早报文件"],"action":"没自动处理","first_seen":"2026-11-15T08:10:00$off"}
]}
PATROL
AGENTQ_TEST_NOW="2026-11-15T08:20:00$off" "$AGENTQ_BIN" tick >/dev/null 2>&1
out=$("$AGENTQ_BIN" evolve signals 2>&1)
case "$out" in
  *"早报"*) ;;
  *) echo "同一批里 agentq 自己的问题（早报没生成）没记成信号：$out" | tail -6; exit 2 ;;
esac
case "$out" in
  *"备份"*) echo "备份脚本的失败不该记成 agentq 的问题：$(printf '%s\n' "$out" | grep 备份 | head -3)"; exit 1 ;;
esac
