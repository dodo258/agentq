#!/bin/sh
# agentq 安装脚本：把最新 Release 里的程序装到 ~/.local/bin/agentq，核对校验值。
#   curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
# 环境变量：AGENTQ_VERSION 指定版本（如 v0.1.0），默认最新；
#          AGENTQ_RELEASE_BASE 换下载地址（测试用，支持 file://）。
# 只放程序，不改 shell 配置，不装服务。整个脚本包在函数里、最后一行才调用：下到一半什么都不做。

tmp=
newbin=

die() {
	printf 'agentq 安装：%s\n' "$*" >&2
	exit 1
}

cleanup() {
	[ -n "$newbin" ] && rm -f "$newbin"
	[ -n "$tmp" ] && rm -rf "$tmp"
	return 0
}

refuse_old() {
	cat >&2 <<EOT
agentq 安装：这台机器上是旧版 agentq（v0.1–v0.3），没有改动任何东西。
换到新版：
  1. agentq pause            不再开新活；等在做的做完，或 agentq stop 停掉
  2. agentq remove --yes     卸掉旧版，数据留在 ~/.agentq
  3. 再跑一次这条安装命令
详细步骤见 README 的「从旧版怎么换」：https://github.com/dodo258/agentq#从旧版怎么换
EOT
	exit 1
}

refuse_other() {
	cat >&2 <<EOT
agentq 安装：$bin/agentq 已经有一个程序，不是这一版 agentq，没有改动任何东西。
先把它挪开（比如 mv $bin/agentq $bin/agentq.bak），再跑一次这条安装命令。
EOT
	exit 1
}

# 是不是这一版：看 --help 的第一行。
is_this() {
	[ -f "$1" ] && [ "$("$1" --help </dev/null 2>/dev/null | head -n 1)" = "agentq：说一句需求，做到合进主分支" ]
}

# 下载：fetch <地址> <目标文件>。正式地址只走 https。
fetch() {
	if [ -n "$strict" ]; then
		curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 -o "$2" "$1"
	else
		curl -fsSL --connect-timeout 15 --max-time 300 -o "$2" "$1"
	fi || die "下载失败：$1"
}

main() {
	trap cleanup EXIT
	trap 'exit 1' HUP INT TERM

	[ "$(id -u)" -ne 0 ] || die "不要用 root 跑，装到当前用户的 ~/.local/bin。"
	[ "$(uname -s)" = Darwin ] || die "只支持 macOS。"
	[ -n "${HOME:-}" ] && [ -d "$HOME" ] || die "找不到 HOME。"
	if [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then
		arch=arm64 # Rosetta 终端里 uname -m 是 x86_64，所以先看 sysctl
	else
		case "$(uname -m)" in
		arm64) arch=arm64 ;;
		x86_64) arch=amd64 ;;
		*) die "不认识的架构：$(uname -m)" ;;
		esac
	fi

	bin=$HOME/.local/bin
	# 先认旧版，再下载：旧版的软链、服务、数据一概不动。
	if [ -L "$bin/agentq" ] || [ -e "$HOME/Library/LaunchAgents/io.agentq.agentq.plist" ]; then
		refuse_old
	fi
	if [ -e "$bin/agentq" ]; then
		is_this "$bin/agentq" || refuse_other
	fi

	repo=https://github.com/dodo258/agentq/releases
	strict=1
	if [ -n "${AGENTQ_RELEASE_BASE:-}" ]; then
		base=${AGENTQ_RELEASE_BASE%/}
		strict=
	elif [ -n "${AGENTQ_VERSION:-}" ]; then
		case "$AGENTQ_VERSION" in
		v[0-9]*) ;;
		*) die "AGENTQ_VERSION 要写成 v1.2.3 这样。" ;;
		esac
		case "$AGENTQ_VERSION" in
		*[!0-9A-Za-z.+-]*) die "AGENTQ_VERSION 要写成 v1.2.3 这样。" ;;
		esac
		base=$repo/download/$AGENTQ_VERSION
	else
		base=$repo/latest/download
	fi

	name=agentq_darwin_$arch.tar.gz
	tmp=$(mktemp -d) || die "建不了临时目录。"
	fetch "$base/$name" "$tmp/$name"
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"

	want=$(awk -v f="$name" '{n=$2; sub(/^\*/, "", n)} n == f {print $1; exit}' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "SHA256SUMS 里没有 $name 的校验值，什么都没装。"
	got=$(shasum -a 256 "$tmp/$name" | awk '{print $1}')
	if [ "$got" != "$want" ]; then
		printf 'agentq 安装：校验值对不上，什么都没装。\n  应为 %s\n  实得 %s\n' "$want" "$got" >&2
		exit 1
	fi

	mkdir "$tmp/x"
	tar -xzf "$tmp/$name" -C "$tmp/x" || die "解包失败。"
	[ -f "$tmp/x/agentq" ] && [ ! -L "$tmp/x/agentq" ] || die "包里没有 agentq。"
	chmod 755 "$tmp/x/agentq"
	ver=$("$tmp/x/agentq" version </dev/null 2>/dev/null) || die "解出来的程序跑不起来。"
	[ -n "$ver" ] || die "解出来的程序没有版本号。"
	is_this "$tmp/x/agentq" || die "下载到的是 $ver，不是这一版 agentq（旧版 v0.1–v0.3 换不了），没有改动任何东西。"

	mkdir -p "$bin" || die "建不了 $bin。"
	newbin=$bin/.agentq.new.$$
	cp "$tmp/x/agentq" "$newbin" || die "装不进 $bin。"
	chmod 755 "$newbin"
	mv -f "$newbin" "$bin/agentq" || die "装不进 $bin。"
	newbin=

	echo "已装好 agentq $ver：$bin/agentq"
	case ":$PATH:" in
	*":$bin:"*) ;;
	*)
		echo "$bin 不在 PATH 里，把这一行加到你的 shell 配置里："
		# shellcheck disable=SC2016
		echo '  export PATH="$HOME/.local/bin:$PATH"'
		;;
	esac
	if [ -e "$HOME/Library/LaunchAgents/cc.agentq.resume.plist" ]; then
		echo "之前装过服务：再跑一次 agentq install，换上新程序。"
	else
		echo "下一步：agentq install"
	fi
}

main "$@"
