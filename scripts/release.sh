#!/bin/sh
# 打 macOS 发布包：两个 tar.gz、install.sh、SHA256SUMS。不打 tag、不上传。
# 用法：scripts/release.sh <版本，如 v1.2.3> <输出目录>
set -e

usage() {
	echo "用法：scripts/release.sh <版本，如 v1.2.3> <输出目录>" >&2
	exit 2
}

[ $# -eq 2 ] || usage
version=$1
out=$2
case "$version" in
*[!0-9A-Za-z.+-]*) usage ;;
esac
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$' || usage

root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)

# 工具链照 .agentq/verify，本地和 CI 用同一个。
unset GOROOT
tc=$(sed -n 's/^export GOTOOLCHAIN=//p' "$root/.agentq/verify" 2>/dev/null | head -n 1)
if [ -n "$tc" ]; then
	GOTOOLCHAIN=$tc
	export GOTOOLCHAIN
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

rm -f "$out/agentq_darwin_arm64.tar.gz" "$out/agentq_darwin_amd64.tar.gz" "$out/install.sh" "$out/SHA256SUMS" "$out/VERSION"
for arch in arm64 amd64; do
	mkdir "$stage/$arch"
	(cd "$root" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/$arch/agentq" .)
	COPYFILE_DISABLE=1 tar -czf "$out/agentq_darwin_$arch.tar.gz" -C "$stage/$arch" agentq
done
cp "$root/install.sh" "$out/install.sh"
(cd "$out" && shasum -a 256 agentq_darwin_arm64.tar.gz agentq_darwin_amd64.tar.gz install.sh >SHA256SUMS)
echo "已生成 $version 的发布文件在 $out"
