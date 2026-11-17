#!/bin/sh
# 试装发布附件：scripts/try-release.sh <附件目录> <版本>
# 附件目录是 scripts/build-release.sh 的产出（或从 GitHub Release 下载的同一批文件）：对 SHA256SUMS、核对 VERSION，
# 再用附件里的 install.sh 在全新临时 HOME 里加 --paused 装一遍（下载地址指向这个目录，不装服务、不开后台），装出来的 agentq version 要是这个版本。
# CLI 用一个只回版本号的假 claude，不登录、不调模型。只读附件目录，临时 HOME 用完就删。
set -eu

if [ "$#" -ne 2 ]; then
	echo '用法：scripts/try-release.sh <附件目录> <版本>' >&2
	exit 2
fi
dl=$(CDPATH='' cd -- "$1" && pwd)
v=$2

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$dl" && sha256sum -c SHA256SUMS) >/dev/null
else
	(cd "$dl" && shasum -a 256 -c SHA256SUMS) >/dev/null
fi
echo "校验值对上了：$(wc -l <"$dl/SHA256SUMS" | tr -d ' ') 个文件"
got=$(cat "$dl/VERSION")
if [ "$got" != "$v" ]; then
	echo "VERSION 写的是 ${got}，不是 ${v}" >&2
	exit 1
fi
[ -f "$dl/install.sh" ] || {
	echo "附件里没有 install.sh" >&2
	exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentq-try.XXXXXX")
tmp=$(CDPATH='' cd -- "$tmp" && pwd -P) # 安装拒绝走软链接（macOS 的 /var 就是）
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home" "$tmp/bin"
cat >"$tmp/bin/claude" <<'FAKE'
#!/bin/sh
case "$1" in
	--version) echo '2.1.316 (Claude Code)' ;;
	--help) echo '--print --output-format --session-id --effort' ;;
esac
FAKE
chmod 755 "$tmp/bin/claude"
if ! HOME=$tmp/home PATH=$tmp/bin:$PATH CI=1 AGENTQ_RELEASE_BASE=file://$dl sh "$dl/install.sh" --paused >"$tmp/install.log" 2>&1; then
	tail -30 "$tmp/install.log" >&2
	echo "临时 HOME 里装不上" >&2
	exit 1
fi
got=$(HOME=$tmp/home "$tmp/home/.local/bin/agentq" version)
if [ "$got" != "agentq $v" ]; then
	echo "装出来的是「${got}」，不是 agentq ${v}" >&2
	exit 1
fi
echo "临时 HOME 试装通过：$got"
