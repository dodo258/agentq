#!/bin/sh
# agentq 成品门的脚本示例。复制到项目的 .agentq/smoke，加可执行权限，按项目改开头几行，提交到默认分支。
#
# 引擎在一次性 worktree 的合并态上跑它，依赖要自己装。约定：
#   检查构建产物在、能启动。退出码 0 算过，过了才有交付单；非 0 算失败；125 算这次跑不了（工具没有、依赖装不上）。
set -u

# ---- 按项目改 ----
BUILD='make build'                # 构建
ARTIFACT='dist/app'               # 构建产物
START='./dist/app --version'      # 启动一下，要能自己结束；服务类的起起来查一次端口再停
# ------------------

sh -c "$BUILD" || exit 1
[ -e "$ARTIFACT" ] || { echo "没有构建产物 $ARTIFACT"; exit 1; }
sh -c "$START" || { echo "启动失败：$START"; exit 1; }
