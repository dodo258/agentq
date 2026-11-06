#!/bin/sh
# agentq 界面门的脚本示例（网站，Playwright）。复制到项目的 .agentq/ui，加可执行权限，按项目改开头几行，提交到默认分支。
#
# 引擎在一次性 worktree 的合并态上跑它，依赖要自己装。约定：
#   截图（PNG）写进 $AGENTQ_ARTIFACTS，退出码 0 算过；非 0 算失败；125 算这次跑不了（工具没有、依赖装不上），不算提交有问题。
# 引擎查截图不是空白，再交给审查进程。
set -u

# ---- 按项目改 ----
INSTALL='npm ci'                   # 装依赖
BUILD='npm run build'              # 构建
SERVE='npm run preview -- --port 4173 --strictPort' # 起服务，要一直跑着
URL='http://localhost:4173'        # 服务地址
PAGES='/'                          # 要截图的页面，空格隔开，受影响的状态都放进来
# ------------------

command -v node >/dev/null 2>&1 || { echo "没有 node"; exit 125; }
command -v curl >/dev/null 2>&1 || { echo "没有 curl"; exit 125; }
sh -c "$INSTALL" || { echo "依赖装不上"; exit 125; }
node -e 'require("playwright")' 2>/dev/null || { echo "项目依赖里没有 playwright"; exit 125; }
sh -c "$BUILD" || exit 1

sh -c "$SERVE" &
server=$!
trap 'kill "$server" 2>/dev/null' EXIT

i=0
until curl -fsS "$URL/" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "服务 60 秒没起来"
		exit 1
	fi
	sleep 1
done

# 宽屏和手机宽度各一张。浏览器没装（npx playwright install）时 launch 报错，算跑不了。
URL="$URL" PAGES="$PAGES" node -e '
const { chromium } = require("playwright");
(async () => {
  let browser;
  try {
    browser = await chromium.launch();
  } catch (e) {
    console.error(e.message);
    process.exit(125);
  }
  const sizes = { desktop: { width: 1280, height: 800 }, phone: { width: 390, height: 844 } };
  for (const path of process.env.PAGES.split(/\s+/).filter(Boolean)) {
    for (const [name, viewport] of Object.entries(sizes)) {
      const page = await browser.newPage({ viewport });
      await page.goto(process.env.URL + path, { waitUntil: "networkidle" });
      const file = name + path.replace(/[^A-Za-z0-9]+/g, "_") + ".png";
      await page.screenshot({ path: process.env.AGENTQ_ARTIFACTS + "/" + file, fullPage: true });
      await page.close();
    }
  }
  await browser.close();
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
'
