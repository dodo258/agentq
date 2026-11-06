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
"$tmp/agentq" "$cmd" "$@"
