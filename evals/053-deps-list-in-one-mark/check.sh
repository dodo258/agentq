#!/bin/sh
# 11-21 使用反馈：派活照 dispatch 技能「可写多个」，在一个依赖括号里用顿号列了四条（依赖：索引与搜索、额度 quota、开标签页 opener、透传托管 host），
# 解析把整串当成一段字去找，status 报「依赖找不到」，前置都合并了这条还一直不开工。要的是：一个括号里按逗号、顿号拆开，每条单独找；真写错的照报。
root="$EVAL_WORK/projects"; p="$root/demo"
mkdir -p "$p/docs" "$HOME/.agentq"
cat > "$p/docs/BACKLOG.md" <<'BL'
# 待办

## 待做

- [ ] 汇总页 —— 要什么（依赖：索引与搜索、额度 quota，开标签页 opener）
- [ ] 写错了 —— 要什么（依赖：没有这一条）

## 已完成

- [x] 索引与搜索 —— 合并提交 abc1234
- [x] 额度 quota 读取 —— 合并提交 abc1235
- [x] 开标签页 opener —— 合并提交 abc1236
BL
atsign=@
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m init) || exit 2
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
out=$("$AGENTQ_BIN" status 2>&1)
case "$out" in
  *"「写错了」依赖『没有这一条』找不到"*) ;;
  *) echo "真写错的依赖要照报找不到：$out" | tail -8; exit 2 ;;
esac
case "$out" in
  *"「汇总页」依赖"*) echo "一个括号里用顿号列的依赖都做完了，不该报找不到：$out" | tail -8; exit 1 ;;
esac
exit 0
