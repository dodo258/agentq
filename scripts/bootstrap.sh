#!/bin/sh
# 唯一源码安装入口；不下载/执行第三方安装脚本，不需要 Node。
set -eu
src=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
case $(uname -s)/$(uname -m) in Darwin/arm64|Darwin/x86_64|Linux/x86_64|Linux/aarch64|Linux/arm64) ;; *) echo 'Supported: macOS arm64 / amd64, Linux amd64 / arm64' >&2; exit 1;; esac
if [ "$(uname -s)" = Linux ]; then
 git_hint='Git required. Install it with your distribution package manager first.'
 python_hint='Python 3 not found. The scheduler and required skills work without it; optional Waza helper scripts need it. Install it with your distribution package manager.'
else
 git_hint='Git required. Install Apple Command Line Tools first: xcode-select --install'
 python_hint='Python 3 not found. The scheduler and required skills work without it; optional Waza helper scripts need it: https://www.python.org/downloads/macos/'
fi
command -v git >/dev/null || { echo "$git_hint" >&2; exit 1; }
command -v go >/dev/null || { echo 'Source install requires Go (see go.mod). Install from https://go.dev/dl/ then repeat this same command.' >&2; exit 1; }
command -v python3 >/dev/null || echo "$python_hint" >&2
tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentq-bootstrap.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
(cd "$src" && go build -o "$tmp/agentq" .)
cmd=setup
case ${1:-} in setup|doctor|rollback|remove|restore) cmd=$1; shift;; esac
# 新的完整安装装完即运行，和 install.sh 一样交给 start --welcome；--paused 只装不跑。升级（已有安装记录）保留原来的开关。
fresh=0; paused=0; welcome=1
[ -f "$HOME/.local/share/agentq/receipt.json" ] || fresh=1
n=$#
while [ "$n" -gt 0 ]; do
 a=$1; shift; n=$((n-1))
 case $a in
  --paused) paused=1; continue;;
  --plan|-plan|--scope=skills|-scope=skills) welcome=0;;
  --scope|-scope) [ "${1:-}" != skills ] || welcome=0;;
 esac
 set -- "$@" "$a"
done
"$tmp/agentq" "$cmd" "$@"
[ "$cmd" = setup ] && [ "$fresh" = 1 ] && [ "$welcome" = 1 ] || exit 0
set --
[ "$paused" = 0 ] || set -- --paused
if [ -z "${CI:-}${NONINTERACTIVE:-}" ] && (: </dev/tty) 2>/dev/null; then
 exec "$HOME/.local/bin/agentq" start --welcome "$@" </dev/tty
fi
exec "$HOME/.local/bin/agentq" start --welcome "$@" </dev/null
