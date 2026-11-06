#!/bin/sh
# 发行构建：scripts/build-release.sh <版本> <输出目录> [<owner/repo>]
# 产出 macOS、Linux 的 arm64、amd64 四个 agentq_<os>_<arch>.tar.gz（包里只有 agentq）和 SHA256SUMS。
# 给了 owner/repo 时，把仓库名编进程序（agentq update 用），再填进 install.sh 的副本放进输出目录，也写进 SHA256SUMS；仓库里的原文件不写账号。
# 只构建，不打 tag、不上传。install.sh 按这几个文件名下载和校验。
set -eu

if [ "$#" -ne 2 ] && [ "$#" -ne 3 ]; then
	echo '用法：scripts/build-release.sh <版本> <输出目录> [<owner/repo>]' >&2
	exit 2
fi
version=$1
out=$2
repo=${3:-}
case $version in
	'' | -* | *[!A-Za-z0-9._+-]*)
		echo "版本号不对：$version" >&2
		exit 2
		;;
esac
if [ "$#" -eq 3 ]; then
	case $repo in
		-* | /* | */ | */*/* | *[!A-Za-z0-9._/-]*)
			echo "仓库名要写成 owner/repo：$repo" >&2
			exit 2
			;;
		*/*) ;;
		*)
			echo "仓库名要写成 owner/repo：$repo" >&2
			exit 2
			;;
	esac
fi

src=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$out"
out=$(CDPATH='' cd -- "$out" && pwd)
stage=$(mktemp -d "${TMPDIR:-/tmp}/agentq-release.XXXXXX")
trap 'rm -rf "$stage"' EXIT
trap 'exit 130' HUP INT
trap 'exit 143' TERM

if command -v sha256sum >/dev/null 2>&1; then
	sha() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
	sha() { shasum -a 256 "$@"; }
else
	echo '没有 sha256sum 或 shasum，算不了校验和' >&2
	exit 1
fi

files=
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
	os=${target%/*}
	arch=${target#*/}
	file=agentq_${os}_${arch}.tar.gz
	echo "构建 $target"
	rm -f "$stage/agentq"
	(cd "$src" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version${repo:+ -X main.repo=$repo}" -o "$stage/agentq" .)
	COPYFILE_DISABLE=1 tar -C "$stage" -czf "$out/$file" agentq
	files="$files $file"
done

rm -f "$out/install.sh"
if [ -n "$repo" ]; then
	sed "s|^\([[:space:]]*github_repo=\)__AGENTQ_REPO__\$|\1$repo|" "$src/install.sh" >"$stage/install.sh"
	if grep -q 'github_repo=__AGENTQ_REPO__' "$stage/install.sh" || ! grep -q "github_repo=$repo\$" "$stage/install.sh"; then
		echo 'install.sh 的仓库名没填上，检查 github_repo 那一行' >&2
		exit 1
	fi
	cp "$stage/install.sh" "$out/install.sh"
	chmod 644 "$out/install.sh"
	files="$files install.sh"
else
	echo '没给 owner/repo，这次不产出 install.sh'
fi

# shellcheck disable=SC2086 # 文件名是上面拼的固定字符，要按空格拆开
(cd "$out" && sha $files >SHA256SUMS)
echo "已产出 ${out}："
(cd "$out" && cat SHA256SUMS)
