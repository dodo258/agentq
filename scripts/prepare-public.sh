#!/bin/sh
# 公开发布前，在导出的候选目录里运行：scripts/prepare-public.sh <owner/repo>
# 把主页和文档里的 <owner>/agentq 换成真实仓库，打开公开后才有意义的徽章，删掉「公开之前」的说明。
# 源码仓库不写账号，所以只在候选目录做，不要提交回源码仓库。
# 还剩 <owner> 时退出码 1，并列出位置，要人看一眼再发布。
set -eu

if [ "$#" -ne 1 ]; then
	echo '用法：scripts/prepare-public.sh <owner/repo>' >&2
	exit 2
fi
repo=$1
case $repo in
	-* | /* | */ | */*/* | *[!A-Za-z0-9._/-]*)
		echo "仓库名要写成 owner/repo：${repo}" >&2
		exit 2
		;;
	*/*) ;;
	*)
		echo "仓库名要写成 owner/repo：${repo}" >&2
		exit 2
		;;
esac

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
files=$(find README.md README.en.md llms.txt SECURITY.md CONTRIBUTING.md docs/guide docs/reference docs/install.zh-CN.md docs/install.en.md -type f -name '*' 2>/dev/null | grep -E '\.(md|txt)$')

for f in $files; do
	# 换仓库；配图换成绝对地址（有的客户端解析不了相对路径）；打开 public-badges 那行注释；删掉 pre-release 两个标记之间的内容。
	sed -e "s#<owner>/agentq#$repo#g" \
		-e "s#src=\"docs/brand/#src=\"https://raw.githubusercontent.com/$repo/main/docs/brand/#g" \
		-e "s#srcset=\"docs/brand/#srcset=\"https://raw.githubusercontent.com/$repo/main/docs/brand/#g" \
		-e 's#^<!-- public-badges: \(.*\) -->$#\1#' \
		-e '/<!-- pre-release:start -->/,/<!-- pre-release:end -->/d' "$f" >"$f.tmp"
	mv "$f.tmp" "$f"
done

if command -v python3 >/dev/null 2>&1 && [ -f scripts/render-readme-art.py ]; then
	if AGENTQ_REPO=$repo python3 -I scripts/render-readme-art.py >/dev/null 2>&1; then
		echo '配图已按真实仓库名重新生成，记得把 sha256 更新进 config/clean-source.json。'
	else
		echo '配图没有重新生成（要本机装 Chrome），终端图里还是 <owner>。' >&2
	fi
fi

# shellcheck disable=SC2086 # 文件名都是仓库内路径，没有空格
left=$(grep -Hn '<owner>' $files || true)
if [ -n "$left" ]; then
	echo '还有没填的 <owner>，改掉再发布：' >&2
	echo "$left" >&2
	exit 1
fi
echo "主页和文档已填成 ${repo}。"
# 公开仓库是新建的，About 不会跟过来，发布时要再设一次。
echo "新仓库的 About：gh repo edit ${repo} --description '交代一句，做到合并。把活排进 BACKLOG，agentq 开独立的 Claude Code 进程做到合并，中断了续上，做完要对得上。' --add-topic claude-code --add-topic coding-agent --add-topic automation --add-topic developer-tools --add-topic golang"
