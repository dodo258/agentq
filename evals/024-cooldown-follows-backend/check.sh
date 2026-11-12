#!/bin/sh
# 11-12：默认后端的用量冷却存在 state.json 顶层、没记属于哪个后端，按当前配置解释。Claude 撞限后把默认换成 Codex：
# Codex 被 Claude 的冷却挡到重置，还指定 Claude 的项目读到空冷却再撞一次限。
# 要的是：冷却按后端名存，换默认后端前后跟着原后端走。
mkdir -p "$HOME/.agentq" "$EVAL_WORK/projects"
cfg="$HOME/.agentq/config.json"
st="$HOME/.agentq/state.json"
base='"projects_root":"'"$EVAL_WORK/projects"'","paused":true,"tick_after_job":false,"patrol_enabled":false,"notify_enabled":false'
printf '{%s}\n' "$base" > "$cfg"
touch -t 202601010000 "$cfg"
until=$(date -u -d '+3 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+3H +%Y-%m-%dT%H:%M:%SZ)
# 默认 Claude 时撞了限（老版本写的样子），tick 存一次
printf '{"jobs":[],"cooldown_until":"%s","breaker_until":"0001-01-01T00:00:00Z"}\n' "$until" > "$st"
"$AGENTQ_BIN" tick > /dev/null 2>&1
# 换默认后端成 Codex，cl 项目还指定 Claude
# status 只看不接活：打开暂停，调度那一行才说冷却
printf '{%s,"backend":"codex","permission_mode":"danger-full-access","project_backends":{"cl":"claude"},"backends":{"claude":{"permission":"bypassPermissions"}}}\n' "$base" | sed 's/"paused":true/"paused":false/' > "$cfg"
out=$("$AGENTQ_BIN" status 2>&1)
line=$(printf '%s\n' "$out" | grep '^调度：')
case "$line" in
  *"codex 用量冷却中"*|"调度：用量冷却中"*) echo "Claude 的冷却挡住了换上来的 Codex：$line"; exit 1 ;;
esac
case "$line" in
  *"后端 claude 用量冷却中"*) ;;
  *) echo "还指定 Claude 的项目读不到 Claude 的冷却：$line"; exit 1 ;;
esac
