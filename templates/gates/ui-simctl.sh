#!/bin/sh
# agentq 界面门的脚本示例（iOS，simctl）。复制到项目的 .agentq/ui，加可执行权限，按项目改开头几行，提交到默认分支。
#
# 引擎在一次性 worktree 的合并态上跑它。约定：
#   截图（PNG）写进 $AGENTQ_ARTIFACTS，退出码 0 算过；非 0 算失败；125 算这次跑不了（工具没有、模拟器起不来），不算提交有问题。
# 引擎查截图不是空白，再交给审查进程。这里新建一台模拟器，用完删掉，不碰你在用的那台。
set -u

# ---- 按项目改 ----
SCHEME='MyApp'                    # xcodebuild 的 scheme
BUNDLE_ID='com.example.myapp'     # App 的 bundle id
DEVICE='iPhone 17'                # 机型，xcrun simctl list devicetypes 里有的
# ------------------

command -v xcrun >/dev/null 2>&1 || { echo "没有 xcrun"; exit 125; }
udid=$(xcrun simctl create "agentq-ui-$$" "$DEVICE") || { echo "模拟器建不了：$DEVICE"; exit 125; }
trap 'xcrun simctl shutdown "$udid" >/dev/null 2>&1; xcrun simctl delete "$udid" >/dev/null 2>&1' EXIT
xcrun simctl bootstatus "$udid" -b >/dev/null || { echo "模拟器起不来"; exit 125; }

derived=$(mktemp -d)
xcodebuild -scheme "$SCHEME" -destination "id=$udid" -derivedDataPath "$derived" build >/dev/null || exit 1
app=$(find "$derived/Build/Products" -maxdepth 2 -name '*.app' | head -n 1)
[ -n "$app" ] || { echo "构建产物里没有 .app"; exit 1; }

xcrun simctl install "$udid" "$app" || exit 1
xcrun simctl launch "$udid" "$BUNDLE_ID" || exit 1
sleep 3 # 等首屏出来；要截别的页面用项目的 UI 测试或深链接跳过去，再截一张
xcrun simctl io "$udid" screenshot "$AGENTQ_ARTIFACTS/launch.png" || exit 1
