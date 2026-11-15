#!/bin/sh
# 能力 backend-delete-guard 的检查（知识在 rules/backend-delete.md）：这次新加了删文件、删分支、删工作树的代码，
# 同一个目录（或它上面的 test/tests 目录）就要有改到的测试，否则报出来。只看新增行，测试和文档里的不算。
# 能力门在候选 worktree 里跑它，AGENTQ_BASE 是默认分支基点；退出码 0 过、1 报出问题、125 判不了。
# 做法同 Danger 的「改了代码没改测试就提醒」，只盯删数据的代码：这类代码判错一次就丢用户的东西。
base=${AGENTQ_BASE:-}
[ -n "$base" ] || { echo "没有 AGENTQ_BASE，不知道和哪里比"; exit 125; }
git rev-parse -q --verify "$base^{commit}" >/dev/null 2>&1 || { echo "基点 $base 不是提交"; exit 125; }
tmp=$(mktemp) || exit 125
trap 'rm -f "$tmp"' EXIT
git diff --unified=0 --no-color --no-renames --no-ext-diff "$base" HEAD >"$tmp" 2>&1 || { echo "git diff 失败：$(head -c 300 "$tmp")"; exit 125; }

awk '
function istest(f,   n, seg, i) {
	if (f ~ /(^|\/)[^\/]*_test\.(go|py)$/ || f ~ /(^|\/)test_[^\/]*\.py$/ || f ~ /\.(test|spec)\.[^\/]*$/) return 1
	n = split(f, seg, "/")
	for (i = 1; i < n; i++) if (seg[i] ~ /^(test|tests|testdata|__tests__|fixtures)$/) return 1
	return 0
}
function source(f) { return f ~ /\.(go|rs|py|js|mjs|cjs|ts|rb|java|kt|swift|sh|bash)$/ }
function dirof(f) { if (f !~ /\//) return "."; sub(/\/[^\/]*$/, "", f); return f }
# 删数据的调用：递归删目录、删分支和工作树、硬重置、镜像同步。删单个文件不算，多是清自己刚写的文件；
# defer、trap、注释，和参数一看就是临时目录的也不算。代码里调 git 认参数数组的写法；命令行写法只在 shell 脚本里认，
# 别的语言里多是写给用户看的恢复命令。
function deletes(s, f) {
	if (s ~ /^[ \t]*(\/\/|#|--|\*|\/\*)/ || s ~ /^[ \t]*defer[ \t]/ || s ~ /(^|[^a-zA-Z_])trap[ \t]/) return 0
	if (s ~ /[Tt][Ee]?[Mm][Pp]/) return 0
	if (s ~ /os\.RemoveAll\(|shutil\.rmtree\(|fs\.(promises\.)?(rm|rmSync)\(.*recursive|remove_dir_all\(|FileUtils\.rm_r|deleteRecursively\(/) return 1
	if (s ~ /"branch",[ \t]*"-[dD]"|"worktree",[ \t]*"remove"|"clean",[ \t]*"-[a-zA-Z]*f|"reset",[ \t]*"--hard"|"update-ref",[ \t]*"-d"/) return 1
	return f ~ /\.(sh|bash)$/ && s ~ /branch -[dD] |worktree remove|clean -[a-zA-Z]*f|reset --hard|update-ref -d|(^|[^a-zA-Z_-])rm -[a-zA-Z]*r|rsync .*--delete/
}
/^\+\+\+ / { f = substr($0, 7); if ($0 == "+++ /dev/null") f = ""; next }
/^@@ / { split($0, h, " "); ln = h[3]; sub(/^\+/, "", ln); sub(/,.*/, "", ln); ln = ln + 0; next }
/^\+/ {
	if (f == "") next
	s = substr($0, 2)
	if (istest(f)) { tested[f] = 1 }
	else if (source(f)) {
		if (s ~ /#\[test\]|#\[cfg\(test\)\]/) inline[f] = 1
		if (deletes(s, f)) { gsub(/^[ \t]+/, "", s); hit[++nh] = f; hl[nh] = ln; ht[nh] = substr(s, 1, 120) }
	}
	ln++
	next
}
END {
	if (nh == 0) { print "这次没有新加删数据的代码"; exit 0 }
	bad = 0
	for (i = 1; i <= nh; i++) {
		f = hit[i]; d = dirof(f); ok = (f in inline)
		for (t in tested) {
			td = dirof(t)
			if (td == d) ok = 1
			# 上面某一层的 test/tests 目录（Rust、Python 常这样放）
			for (a = d; !ok; ) {
				if (td == a "/test" || td == a "/tests" || index(td, a "/tests/") == 1 || index(td, a "/test/") == 1) ok = 1
				if (a == ".") { if (td ~ /^tests?(\/|$)/) ok = 1; break }
				if (a !~ /\//) a = "."; else sub(/\/[^\/]*$/, "", a)
			}
		}
		if (!ok) { bad++; print f ":" hl[i] "：" ht[i] }
	}
	if (bad == 0) { print "新加了 " nh " 处删数据的代码，同目录都改了测试"; exit 0 }
	print ""
	print "上面 " bad " 处新加了删文件、删分支或删工作树的代码，同一个目录没改到测试。"
	print "删数据的判定错一次就丢用户的东西：补测试，至少一例「该留的留着」（不该删的照样在），见 rules/backend-delete.md。"
	exit 1
}' "$tmp"
