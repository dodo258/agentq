#!/bin/sh
# 能力 backend-count-hotspot 的检查（知识在 rules/backend-merge-hotspots.md）：这次在测试里把写死的「有几项」改了个数
# （34 题 → 35 题、len(x) != 7 → 8），报出来。这种数每加一项都要跟着改同一行，两条并行的分支各加一项就在这一行撞冲突，
# 后合的那条退回重来一轮。只看测试代码里一对删改行：去掉数字后一模一样、数变了、行里有「几项」的说法。
# 能力门在候选 worktree 里跑它，AGENTQ_BASE 是默认分支基点；退出码 0 过、1 报出问题、125 判不了。
base=${AGENTQ_BASE:-}
[ -n "$base" ] || { echo "没有 AGENTQ_BASE，不知道和哪里比"; exit 125; }
git rev-parse -q --verify "$base^{commit}" >/dev/null 2>&1 || { echo "基点 $base 不是提交"; exit 125; }
tmp=$(mktemp) || exit 125
trap 'rm -f "$tmp"' EXIT
git diff --unified=0 --no-color --no-renames --no-ext-diff "$base" HEAD >"$tmp" 2>&1 || { echo "git diff 失败：$(head -c 300 "$tmp")"; exit 125; }

awk '
# 测试代码：Go、Python、JS/TS、Rust、Swift 等的测试文件，和 test/tests 目录下的源码。testdata、fixtures 里的黄金文件不算：它们是生成的输出。
function istest(f,   n, seg, i) {
	if (f !~ /\.(go|rs|py|js|mjs|cjs|ts|tsx|jsx|rb|java|kt|swift|sh|bash)$/) return 0
	if (f ~ /(^|\/)(testdata|fixtures|__snapshots__)\//) return 0
	if (f ~ /(^|\/)[^\/]*_test\.(go|py)$/ || f ~ /(^|\/)test_[^\/]*\.py$/ || f ~ /\.(test|spec)\.[^\/]*$/ || f ~ /Tests?\.(swift|kt|java)$/) return 1
	n = split(f, seg, "/")
	for (i = 1; i < n; i++) if (seg[i] ~ /^(test|tests|__tests__)$/) return 1
	return 0
}
function norm(s) { gsub(/[0-9]+/, "#", s); gsub(/[ \t]+/, " ", s); return s }
# 行里在说「有几项」：取长度、计数，或「N 题、N 条、N 项、N 个」。
function counting(s) {
	return s ~ /len\(|Len\(|HaveLen|[Ll]ength|[Cc]ount|[Ss]ize\(|\.size|[0-9]+ ?(题|条|项|个|道|份|行|处)/
}
function nums(s,   out) {
	out = ""
	while (match(s, /[0-9]+/)) { out = out (out == "" ? "" : ",") substr(s, RSTART, RLENGTH); s = substr(s, RSTART + RLENGTH) }
	return out
}
function flush(   i, k) {
	k = (nr < na) ? nr : na
	for (i = 1; i <= k; i++) {
		if (rm[i] == ad[i] || norm(rm[i]) != norm(ad[i]) || !counting(ad[i])) continue
		s = ad[i]; gsub(/^[ \t]+/, "", s)
		hit[++nh] = f ":" al[i] "：" substr(s, 1, 120) "（" nums(rm[i]) " → " nums(ad[i]) "）"
	}
	nr = 0; na = 0
}
/^\+\+\+ / { flush(); f = substr($0, 7); if ($0 == "+++ /dev/null") f = ""; t = (f != "" && istest(f)); next }
/^@@ / { flush(); split($0, h, " "); ln = h[3]; sub(/^\+/, "", ln); sub(/,.*/, "", ln); ln = ln + 0; next }
/^-/ && !/^--- / { if (t) rm[++nr] = substr($0, 2); next }
/^\+/ { if (t) { ad[++na] = substr($0, 2); al[na] = ln }; ln++; next }
END {
	flush()
	if (nh == 0) { print "测试里没有跟着改写死的数量"; exit 0 }
	for (i = 1; i <= nh; i++) print hit[i]
	print ""
	print "上面 " nh " 处测试把写死的数量跟着改了：并行的分支各加一项都要改这一行，后合的那条必撞冲突、退回一轮。"
	print "改成从数据源数出来再比（数目录、数清单），或只断言新加的那项在，见 rules/backend-merge-hotspots.md。"
	exit 1
}' "$tmp"
