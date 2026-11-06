#!/bin/sh
# agentq 一键安装和升级，新老用户同一条：
#   curl -fsSL https://github.com/<owner>/agentq/releases/latest/download/install.sh | sh
#   带参数：... | sh -s -- --mirror https://镜像前缀/
# 这个脚本只做三件事：取程序并校验、交给程序自己的 setup 安装、把终端交给 agentq start --welcome。
# 备份、冲突检查、保留配置在 setup 里；要不要启动、算不算同意、官方 CLI 没装没登录怎么办都在 --welcome 里。不用 sudo，拒绝 root。
#
# 脚本自己的参数（不传给 setup）：
#   --yes                明确同意启动，不再问（AGENTQ_ASSUME_YES=1 同义）
#   --proxy <地址>       下载走代理：http://、https://、socks5://、socks5h://（AGENTQ_PROXY 同义）
#   --mirror <前缀>      GitHub 下载镜像，程序从 <前缀><github 地址> 取（AGENTQ_MIRROR 同义）。校验文件和源码仍从原地址取，镜像改过的文件会被拒绝
#   --version <版本>     要装的版本，默认 latest；main 走源码（AGENTQ_VERSION 同义）
#   --uninstall          转给 agentq remove --yes，数据保留
# 其余参数交给 setup，如 --backend <名字>（新安装用哪个后端，默认 claude；已有配置不改）、--extras、--plan、--scope skills、--backend-path <路径>。
# 环境变量 AGENTQ_RELEASE_BASE 换下载地址，优先于仓库名，测试用：latest 取 <地址>/<文件>，指定版本取 <地址>/<版本>/<文件>
# 全部逻辑在函数里，最后一行才调用：只下载到一半时语法不完整，sh 什么都不会做。

say() { printf '%s\n' "$*"; }

die() {
	spin_stop
	printf 'agentq 安装：%s\n' "$*" >&2
	exit 1
}

usage() {
	say 'agentq 安装脚本。用法：sh install.sh [参数]'
	say '  --yes              明确同意启动，不再问'
	say '  --proxy <地址>     下载走代理（http、https、socks5、socks5h）'
	say '  --mirror <前缀>    GitHub 下载镜像；校验文件和源码仍从原地址取，镜像改过的文件会被拒绝'
	say '  --version <版本>   要装的版本，默认 latest；main 走源码'
	say '  --uninstall        卸载，数据保留'
	say '交给 setup 的常用参数：--backend <名字>（新安装用哪个后端，默认 claude；已有配置不改）、--extras（加装可选 Waza 技能）、--plan（只看计划不安装）、--scope skills（只装技能）、--backend-path <路径>（--claude 同义）'
	say '环境变量：AGENTQ_ASSUME_YES、AGENTQ_PROXY、AGENTQ_MIRROR、AGENTQ_VERSION、AGENTQ_RELEASE_BASE（下载地址）'
}

# 只有终端才转圈、才上色。输出被捕获时不打动画：一屏过时的「下载中」只会添乱。
init_style() {
	spin_pid=
	animate=0
	green=
	reset=
	[ -t 1 ] || return 0
	[ "${TERM:-}" = dumb ] || animate=1
	if [ -z "${NO_COLOR:-}" ]; then
		green=$(printf '\033[32m')
		reset=$(printf '\033[0m')
	fi
}

ok() { printf '%s✓%s %s\n' "$green" "$reset" "$*"; }

spin_start() {
	[ "$animate" = 1 ] || return 0
	(
		trap 'exit 0' TERM
		while :; do
			for c in ⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏; do
				printf '\r%s %s' "$c" "$1"
				sleep 0.1 2>/dev/null || sleep 1
			done
		done
	) &
	spin_pid=$!
}

spin_stop() {
	[ -n "${spin_pid:-}" ] || return 0
	kill "$spin_pid" 2>/dev/null || true
	wait "$spin_pid" 2>/dev/null || true
	spin_pid=
	printf '\r\033[K'
}

# 服务装在用户级，root 装完跑不起来。在写任何文件之前拒绝。
refuse_root() {
	[ "$(id -u 2>/dev/null)" != 0 ] || die '不要用 root 或 sudo 运行。agentq 的后台服务是用户级的，装在 root 下跑不起来。换回你自己的用户重新运行，什么都没有改。'
}

parse_args() {
	skills_only=0
	plan_only=0
	while [ "$#" -gt 0 ]; do
		case $1 in
			-h | --help)
				usage
				exit 0
				;;
			--plan | -plan) plan_only=1 ;;
			--scope=skills | -scope=skills) skills_only=1 ;;
			--scope | -scope)
				if [ "$#" -gt 1 ]; then
					[ "$2" != skills ] || skills_only=1
					shift
				fi
				;;
		esac
		shift
	done
}

check_options() {
	case $proxy in
		'' | http://?* | https://?* | socks5://?* | socks5h://?*) ;;
		*) die "--proxy 要写成 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080 这样的地址，不是 $proxy" ;;
	esac
	case $mirror in
		'') ;;
		http://?* | https://?*)
			case $mirror in
				*/) ;;
				*) mirror=$mirror/ ;;
			esac
			;;
		*) die "--mirror 要写成放在 github.com 地址前面的 http(s) 前缀，如 https://mirror.example/，不是 $mirror" ;;
	esac
	# 代理放进环境：curl、wget、git（setup 取锁定依赖）、go 都认。
	if [ -n "$proxy" ]; then
		export http_proxy="$proxy" https_proxy="$proxy" HTTP_PROXY="$proxy" HTTPS_PROXY="$proxy" all_proxy="$proxy" ALL_PROXY="$proxy"
		export AGENTQ_PROXY="$proxy"
	fi
	[ -z "$mirror" ] || export AGENTQ_MIRROR="$mirror"
}

detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case $os in
		Darwin) os=darwin ;;
		Linux) os=linux ;;
		*) die "不支持 ${os}。支持 macOS 和 Linux 的 arm64、amd64；Windows 可以试 WSL2，但没有验证过。" ;;
	esac
	case $arch in
		arm64 | aarch64) arch=arm64 ;;
		x86_64 | amd64) arch=amd64 ;;
		*) die "不支持 $arch 架构。支持 macOS 和 Linux 的 arm64、amd64。" ;;
	esac
}

check_git() {
	git --version >/dev/null 2>&1 && return 0
	if [ "$os" = darwin ]; then
		die "需要 Git。先执行 xcode-select --install，装好后重新运行这条命令。"
	fi
	die "需要 Git。先用发行版的包管理器安装（如 apt install git），再重新运行这条命令。"
}

check_version() {
	case $want_version in
		'' | -* | *[!A-Za-z0-9._+-]*) die "版本（--version 或 AGENTQ_VERSION）不对：$want_version" ;;
	esac
}

# ① 在本仓 checkout 里运行：用本地源码。只认脚本自己所在的目录，curl | sh 时 $0 是 sh，不会误用当前目录。
try_checkout() {
	case $0 in
		install.sh | */install.sh) ;;
		*) return 1 ;;
	esac
	src=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd) || return 1
	[ -f "$src/main.go" ] && [ -f "$src/go.mod" ] || return 1
	grep -q '^module agentq$' "$src/go.mod" || return 1
	if ! command -v go >/dev/null 2>&1; then
		say "在源码目录里，但没有 Go，改用发布版。"
		return 1
	fi
	spin_start "从本地源码构建：$src"
	if ! (cd "$src" && go build -o "$tmp/agentq" .); then
		die "本地构建失败，见上面的报错。修好后重试；要用发布版，把脚本放到仓库外运行。"
	fi
	spin_stop
	ok "从本地源码构建：$src"
}

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 2 --connect-timeout 15 --max-time 600 -o "$2" "$1"
	else
		wget -q --timeout=30 -O "$2" "$1"
	fi
}

# 只有 Release 的程序包走镜像，它下载后要对校验文件。校验文件和源码不经过这里：源码克隆下来就构建运行，没有东西能核对它。
via_mirror() {
	case $1 in
		https://github.com/*) printf '%s%s' "$mirror" "$1" ;;
		*) printf '%s' "$1" ;;
	esac
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

repo_unfilled() {
	case $github_repo in
		__*__) return 0 ;;
	esac
	return 1
}

# ② 下载 GitHub Release。下载不到就让下一种办法试；下载到了就必须校验过，校验不过直接退出，绝不换来源或跳过。
try_release() {
	file=agentq_${os}_${arch}.tar.gz
	if [ -n "${AGENTQ_RELEASE_BASE:-}" ]; then
		base=${AGENTQ_RELEASE_BASE%/}
		[ "$want_version" = latest ] || base=$base/$want_version
	elif repo_unfilled; then
		release_note='这份脚本是仓库里的原文件，没填下载地址。改用 Release 附件里的 install.sh（https://github.com/<owner>/agentq/releases/latest/download/install.sh），或 git clone 仓库后在里面运行 sh install.sh'
		return 1
	elif [ "$want_version" = latest ]; then
		base=https://github.com/$github_repo/releases/latest/download
	else
		base=https://github.com/$github_repo/releases/download/$want_version
	fi
	if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
		release_note='没有 curl 或 wget，下载不了发布版'
		return 1
	fi
	from=$(via_mirror "$base/$file")
	spin_start "下载 $file"
	if ! fetch "$from" "$tmp/$file" </dev/null; then
		spin_stop
		release_note="下载不到发布版（${from}）"
		return 1
	fi
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" </dev/null || die "下载到了程序，但拿不到校验文件 $base/SHA256SUMS，没有安装。"
	spin_stop
	if [ "$from" = "$base/$file" ]; then
		ok "下载 $file"
	else
		ok "下载 ${file}（经镜像 ${mirror}）"
	fi
	if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
		die "没有 sha256sum 或 shasum，校验不了下载的程序，没有安装。装上其中一个再重新运行。"
	fi
	expected=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
	actual=$(sha256_of "$tmp/$file")
	if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
		die "校验不过，没有安装：$file 的 SHA256 是 ${actual:-未知}，校验文件里写的是 ${expected:-（没有这一条）}。可能下载被改过，先别继续用这个地址。"
	fi
	ok "校验通过"
	mkdir "$tmp/unpack"
	tar -xzf "$tmp/$file" -C "$tmp/unpack" agentq || die "解压 $file 失败，没有安装。"
	[ -f "$tmp/unpack/agentq" ] && [ ! -L "$tmp/unpack/agentq" ] || die "$file 里没有 agentq 程序，没有安装。"
	mv "$tmp/unpack/agentq" "$tmp/agentq"
	chmod 755 "$tmp/agentq"
}

# ③ 没有 Release 但有 Go：拉源码构建。--version main 直接走这里。
try_source() {
	if ! command -v go >/dev/null 2>&1; then
		source_note='没有 Go，不能从源码构建（https://go.dev/dl/）'
		return 1
	fi
	if repo_unfilled; then
		source_note='这份脚本没填仓库地址，拉不了源码'
		return 1
	fi
	if [ "$want_version" = main ]; then
		say "--version main：从源码构建。"
	else
		say "下载不到发布版，有 Go，从源码构建。"
	fi
	if [ "$want_version" = latest ]; then
		set --
	else
		set -- --branch "$want_version"
	fi
	if ! clone_out=$(GIT_TERMINAL_PROMPT=0 git clone -q --depth=1 "$@" "https://github.com/$github_repo.git" "$tmp/src" </dev/null 2>&1); then
		source_note="拉不到源码：$clone_out"
		return 1
	fi
	if ! (cd "$tmp/src" && go build -o "$tmp/agentq" .); then
		source_note='源码构建失败，见上面的报错'
		return 1
	fi
}

get_program() {
	release_note=
	source_note=
	try_checkout && return 0
	if [ "$want_version" != main ]; then
		try_release && return 0
	fi
	try_source && return 0
	printf 'agentq 安装：拿不到程序。\n' >&2
	[ -z "$release_note" ] || printf '  %s\n' "$release_note" >&2
	[ -z "$source_note" ] || printf '  %s\n' "$source_note" >&2
	die '解决其中一条再重新运行：网络能访问 Release（国内可以试 --proxy、--mirror）；或安装 Go 和 Git；或 git clone 仓库后在里面运行 sh install.sh。'
}

# setup 不覆盖不归它管的东西。旧版只装二进制的用户会在这里停下，告诉他们怎么腾地方。
explain_setup_failure() {
	if [ "$upgrading" = 0 ] && [ -e "$HOME/.local/bin/agentq" ]; then
		say '' >&2
		say '这台机器上有不是这个脚本装的旧版 agentq，安装程序不覆盖它。腾出位置后重新运行这条命令（配置和任务数据都保留）：' >&2
		say '  agentq uninstall' >&2
		say '  mv ~/.local/bin/agentq ~/.local/bin/agentq.legacy' >&2
		say '  [ ! -f ~/.agentq/tick-guard.sh ] || mv ~/.agentq/tick-guard.sh ~/.agentq/tick-guard.sh.legacy' >&2
	fi
	die '安装没有完成，见上面的报错。没有改动你的配置和任务数据。'
}

# 细节收起来，失败时才放出来。--plan 就是要看细节。
run_setup() {
	if [ "$plan_only" = 1 ]; then
		"$tmp/agentq" setup --yes --defer-engine "$@" </dev/null || explain_setup_failure
		return 0
	fi
	spin_start '安装程序和技能'
	if "$tmp/agentq" setup --yes --quiet --defer-engine "$@" </dev/null >"$tmp/setup.log" 2>&1; then
		spin_stop
		cat "$tmp/setup.log"
	else
		spin_stop
		cat "$tmp/setup.log" >&2
		explain_setup_failure
	fi
}

print_path_hint() {
	case :$PATH: in
		*:"$HOME/.local/bin":*) ;;
		*)
			say "$HOME/.local/bin 不在 PATH 里。把这一行加到 shell 配置文件（zsh 是 ~/.zshrc，bash 是 ~/.bashrc）后重开终端："
			# shellcheck disable=SC2016 # 就是要原样打印 $HOME 和 $PATH
			say '  export PATH="$HOME/.local/bin:$PATH"'
			;;
	esac
}

# 新安装的最后一步：要不要启动由 --welcome 决定，这里只找终端。
# CI 里可能开着终端，但输出要留在日志里，所以不重定向。
hand_over() {
	installed=$HOME/.local/bin/agentq
	if [ "$assume_yes" = 1 ]; then
		set -- --assume-yes
	else
		set --
	fi
	tty_ok=0
	if [ -z "${CI:-}${NONINTERACTIVE:-}" ] && (: </dev/tty >/dev/tty) 2>/dev/null; then
		tty_ok=1
	fi
	rc=0
	if [ "$tty_ok" = 0 ]; then
		"$installed" start --welcome "$@" </dev/null || rc=$?
	elif [ "$assume_yes" = 1 ]; then
		"$installed" start --welcome "$@" </dev/tty || rc=$?
	else
		"$installed" start --welcome "$@" </dev/tty >/dev/tty || rc=$?
	fi
	[ "$rc" = 0 ] && return 0
	say 'agentq 已经装好，后台没有开。原因见上面，解决后运行 agentq start。' >&2
	exit "$rc"
}

finish() {
	if [ "$plan_only" = 1 ]; then
		say ''
		say '只看了计划，没有安装。'
		return 0
	fi
	if [ "$skills_only" = 1 ]; then
		ok '技能装好了。这个范围不含后台程序。'
		return 0
	fi
	new_version=$("$HOME/.local/bin/agentq" version 2>/dev/null) || new_version=agentq
	if [ "$upgrading" = 1 ]; then
		ok "已升级：${old_version:-旧版本} -> ${new_version}。配置、任务和已选的服务都保留。"
		say '检查一遍：agentq doctor'
		[ -f "$HOME/.agentq/consent.json" ] || say '还没开后台的话：agentq start'
		# 已经加过 PATH 那一行的，新开的终端就有了。
		[ -f "$HOME/.local/share/agentq/shell-path.json" ] || print_path_hint
		return 0
	fi
	ok "安装程序和技能（${new_version}）"
	hand_over
}

# 上次卸载没能改回 shell 配置文件：程序入口已经没了，用保留的那一代接着清。
finish_path_cleanup() {
	out=
	for gen_bin in "$HOME"/.local/share/agentq/generations/*/bin/agentq; do
		[ -x "$gen_bin" ] || continue
		if out=$("$gen_bin" remove --yes --quiet </dev/null 2>&1); then
			ok '接着上次的卸载，处理了 shell 配置文件里 agentq 加的那一行。'
			[ -z "$out" ] || say "$out"
			return 0
		fi
	done
	[ -z "$out" ] || say "$out" >&2
	say '程序和后台服务已经卸载。shell 配置文件里还有带 agentq managed 标记的那一行，解决上面的问题后重跑 --uninstall，或者手动删掉它（备份是同名的 *.pre-agentq）。' >&2
	die '那一行没有清掉。配置和任务数据在 ~/.agentq。'
}

do_uninstall() {
	bin=$HOME/.local/bin/agentq
	receipt=$HOME/.local/share/agentq/receipt.json
	if [ -x "$bin" ]; then
		if out=$("$bin" remove --yes --quiet </dev/null 2>&1); then
			ok '卸载了程序、技能链接和后台服务。配置和任务数据还在 ~/.agentq。'
			[ -z "$out" ] || say "$out"
			return 0
		fi
		say "$out" >&2
		if [ -f "$receipt" ]; then
			die '卸载没有完成，安装记录还在。见上面的报错，解决后重跑 --uninstall。'
		fi
		if [ ! -e "$bin" ] && [ -f "$HOME/.local/share/agentq/shell-path.json" ]; then
			die '程序和后台服务已经卸载，配置和任务数据还在 ~/.agentq，但 shell 配置文件没改回去（原因见上面）。解决后重跑 --uninstall，会接着处理那一行。'
		fi
		say '这个程序不是这个脚本装的。旧版可以这样腾出位置：agentq uninstall，再 mv ~/.local/bin/agentq ~/.local/bin/agentq.legacy。' >&2
		die '卸载没有完成，什么都没有删。配置和任务数据在 ~/.agentq。'
	fi
	if [ ! -f "$receipt" ]; then
		if [ -f "$HOME/.local/share/agentq/shell-path.json" ]; then
			finish_path_cleanup
			return 0
		fi
		ok '这台机器上没有装 agentq，没有要卸的。'
		return 0
	fi
	say '有安装记录，但程序 ~/.local/bin/agentq 不在了。只装技能（--scope skills）的，到源码目录运行 sh scripts/bootstrap.sh remove --yes。' >&2
	say '手动删过程序的，这个脚本没法替你卸干净。可能还留着：' >&2
	say '  后台服务：launchctl list | grep agentq（Linux：systemctl --user list-units | grep agentq）' >&2
	say '  技能链接：~/.claude/skills、~/.agents/skills 下指向 ~/.local/share/agentq 的链接' >&2
	say '  shell 配置文件里带 agentq managed 标记的那一行' >&2
	die '没有卸载。配置和任务数据在 ~/.agentq。'
}

main() {
	set -eu
	# 占位：scripts/build-release.sh 在 Release 附件的副本里把这一行填成 <owner>/<repo>。仓库里的原文件不写账号，只能配合 checkout 或 AGENTQ_RELEASE_BASE 用。
	github_repo=__AGENTQ_REPO__
	init_style
	trap 'spin_stop' EXIT

	# 脚本自己的参数在这里吃掉，其余的留在 "$@" 里交给 setup。
	assume_yes=0
	uninstall=0
	proxy=${AGENTQ_PROXY:-}
	mirror=${AGENTQ_MIRROR:-}
	want_version=${AGENTQ_VERSION:-latest}
	n=$#
	while [ "$n" -gt 0 ]; do
		arg=$1
		shift
		n=$((n - 1))
		case $arg in
			--yes | -yes) assume_yes=1 ;;
			--uninstall) uninstall=1 ;;
			--proxy=*) proxy=${arg#--proxy=} ;;
			--mirror=*) mirror=${arg#--mirror=} ;;
			--version=*) want_version=${arg#--version=} ;;
			--proxy | --mirror | --version)
				[ "$n" -gt 0 ] || die "$arg 后面要写值"
				case $arg in
					--proxy) proxy=$1 ;;
					--mirror) mirror=$1 ;;
					--version) want_version=$1 ;;
				esac
				shift
				n=$((n - 1))
				;;
			*) set -- "$@" "$arg" ;;
		esac
	done
	[ "${AGENTQ_ASSUME_YES:-}" != 1 ] || assume_yes=1
	parse_args "$@"
	# 不用 ${HOME:?}：macOS 的 sh（bash 3.2）这种致命错误碰上 EXIT trap 退出码是 0。
	[ -n "${HOME:-}" ] || die '没有设置 HOME，不知道装到哪里。什么都没有改。'
	refuse_root
	if [ "$uninstall" = 1 ]; then
		do_uninstall
		return 0
	fi
	check_options
	detect_platform
	check_version
	check_git
	ok "环境检查：$os/${arch}，$(git --version)"

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentq-install.XXXXXX")
	trap 'spin_stop; rm -rf "$tmp"' EXIT
	trap 'exit 130' HUP INT
	trap 'exit 143' TERM

	get_program
	"$tmp/agentq" version >/dev/null 2>&1 || die "拿到的程序在这台机器上跑不起来（$os/${arch}）。临时目录不能执行程序时，用 TMPDIR 换一个目录再试。"

	upgrading=0
	old_version=
	if [ -f "$HOME/.local/share/agentq/receipt.json" ]; then
		upgrading=1
		old_version=$("$HOME/.local/bin/agentq" version 2>/dev/null) || old_version=
	fi
	run_setup "$@"
	finish
}

# 花括号让这一行要读完整才算一条命令：截断成 `main` 或 `{ main "$@"` 都不会带着残缺参数跑起来。
{ main "$@"; }
