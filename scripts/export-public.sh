#!/bin/sh
# 导出公开候选：scripts/export-public.sh <输出目录> <owner/repo>
# 从 HEAD 取文件（工作区的改动不算），去掉 config/clean-source.json 的 private_paths，填仓库名，再扫隐私。
# 任何一步不过就退出非 0。扫描时只放过完整的 owner/repo。不建 git、不联网。输出目录必须不存在或是空的。
# 禁用词表读 ~/.agentq/private/privacy-patterns.json，缺了就报错，不跳过。报告只写路径和类别，不写命中的内容。
set -eu

if [ "$#" -ne 2 ]; then
	echo '用法：scripts/export-public.sh <输出目录> <owner/repo>' >&2
	exit 2
fi
out=$1
repo=$2

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
patterns=$HOME/.agentq/private/privacy-patterns.json
if [ ! -f "$patterns" ]; then
	echo "找不到禁用词表：${patterns}。先建好再导出，见 docs/privacy.md。" >&2
	exit 2
fi
command -v python3 >/dev/null 2>&1 || {
	echo '导出要 python3' >&2
	exit 2
}

mkdir -p "$out"
if [ -n "$(ls -A "$out")" ]; then
	echo "输出目录不是空的：${out}" >&2
	exit 2
fi
out=$(CDPATH='' cd -- "$out" && pwd)
case $out/ in
	"$root"/*)
		echo "输出目录不能在仓库里：${out}" >&2
		exit 2
		;;
esac

git -C "$root" archive --format=tar HEAD | tar -x -C "$out"

# 去掉私有路径
python3 -I - "$out" <<'EOF'
import json
import os
import shutil
import sys

out = sys.argv[1]
with open(os.path.join(out, "config", "clean-source.json"), encoding="utf-8") as f:
    private = json.load(f)["private_paths"]
if not private:
    sys.exit("private_paths 是空的")
for rel in private:
    path = os.path.join(out, rel)
    if os.path.isdir(path):
        shutil.rmtree(path)
    elif os.path.lexists(path):
        os.remove(path)
EOF

sh "$out/scripts/prepare-public.sh" "$repo"

# prepare-public 重新生成配图时 hash 会变：内容来自同一棵树里的 render-readme-art.py，换成新 hash，再由下面的扫描查。
python3 -I - "$out" <<'EOF'
import hashlib
import json
import os
import sys

out = sys.argv[1]
path = os.path.join(out, "config", "clean-source.json")
with open(path, encoding="utf-8") as f:
    manifest = json.load(f)
changed = False
for rel, want in manifest.get("approved_files", {}).items():
    if not rel.startswith("docs/brand/readme/"):
        continue
    with open(os.path.join(out, rel), "rb") as f:
        got = hashlib.sha256(f.read()).hexdigest()
    if got != want:
        manifest["approved_files"][rel] = got
        changed = True
if changed:
    with open(path, "w", encoding="utf-8") as f:
        json.dump(manifest, f, indent=2, ensure_ascii=False)
        f.write("\n")
EOF

# 目标仓库名是你指定要公开的，可能本身就在词表里（账号名）。在副本里只遮掉完整的 owner/repo 再扫，别处单独出现账号名照样算命中。
scan_dir=$(mktemp -d)
trap 'rm -rf "$scan_dir"' EXIT
cp -R "$out/." "$scan_dir/"
python3 -I - "$scan_dir" "$repo" <<'EOF'
import os
import sys

scan_dir, repo = sys.argv[1], sys.argv[2].encode()
for base, _, files in os.walk(scan_dir):
    for name in files:
        path = os.path.join(base, name)
        if os.path.islink(path):
            continue
        with open(path, "rb") as f:
            data = f.read()
        if repo in data:
            with open(path, "wb") as f:
                f.write(data.replace(repo, b"TARGET-REPO"))
EOF

if ! python3 -I "$scan_dir/scripts/privacy_scan.py" --root "$scan_dir" --manifest "$scan_dir/config/clean-source.json"; then
	echo "隐私扫描没过：${out}" >&2
	exit 1
fi

# 再按禁用词表逐字节扫一遍所有文件和文件名，包括扫描器按 hash 放行的图片。
python3 -I - "$scan_dir" "$patterns" <<'EOF'
import json
import os
import sys

out, patterns_path = sys.argv[1], sys.argv[2]
try:
    with open(patterns_path, encoding="utf-8") as f:
        patterns = json.load(f)["patterns"]
    rules = [(p["type"], p["literal"].encode(), bool(p.get("case_sensitive", True))) for p in patterns]
except (OSError, ValueError, KeyError, TypeError, AttributeError) as e:
    sys.exit(f"禁用词表读不了：{type(e).__name__}")
if not rules:
    sys.exit("禁用词表是空的")


def hits(data: bytes) -> set[str]:
    low = data.lower()
    return {kind for kind, literal, cs in rules if (literal in data if cs else literal.lower() in low)}


found = 0
for base, dirs, files in os.walk(out):
    for name in dirs + files:
        path = os.path.join(base, name)
        rel = os.path.relpath(path, out)
        kinds = hits(rel.encode())
        if name in files and not os.path.islink(path):
            with open(path, "rb") as f:
                kinds |= hits(f.read())
        elif os.path.islink(path):
            kinds |= hits(os.readlink(path).encode())
        for kind in sorted(kinds):
            print(f"{rel}: {kind}", file=sys.stderr)
            found += 1
if found:
    sys.exit(f"禁用词表命中 {found} 处")
EOF

echo "公开候选：${out}"
