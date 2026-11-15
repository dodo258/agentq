#!/bin/sh
# 后端能力缺口（11-11 到 11-12 三次门没过）：其中一次是删数据的判定写错了——按路径任一级命中可再生目录就判能删，
# 可再生目录里的 .env 会被一起删掉，靠审查才拦下。
# 要的是：Go 项目的任务新加了删目录的代码（os.RemoveAll）、同一个包没改测试，能力门要报出来（warn 档照样合并），
# 并在门的结果里记下用上了哪项能力。没有这项能力时能力门不适用，这题判没过。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
mkdir -p "$p/.agentq" "$p/docs" "$p/prune" "$HOME/.agentq"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
echo $PPID >> "$HOME/fake-shells"
prompt=""; prev=""
for a in "$@"; do
  case "$prev" in -p) prompt="$a" ;; esac
  prev="$a"
done
case "$prompt" in
  /usage) echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; exit 0 ;;
  *审查门的独立审查进程*) echo '{"type":"result","subtype":"success","is_error":false,"result":"审过了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"没问题","findings":[]}}'; exit 0 ;;
esac
b=agent/prune-cache; wt=$PWD/.claude/worktrees/prune-cache
sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
git add docs/BACKLOG.md && git commit -q -m "待办：认领"
git worktree add -q "$wt" -b "$b" main
cat > "$wt/prune/prune.go" <<'GO'
package prune

import (
	"os"
	"path/filepath"
	"strings"
)

// Prune 删掉 root 下可以重新生成的目录。
func Prune(root string, names []string) error {
	for _, n := range names {
		for _, seg := range strings.Split(n, "/") {
			if seg == "build" || seg == "dist" {
				if err := os.RemoveAll(filepath.Join(root, n)); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}
GO
mkdir -p "$wt/.agentq"
printf '{"branch":"%s","type":"feature","acceptance":"看 prune/prune.go","progress":"加了清理可再生目录"}\n' "$b" > "$wt/.agentq/ready.json"
git -C "$wt" add -A && git -C "$wt" commit -q -m "加清理可再生目录" >/dev/null 2>&1
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n分支可以合并了。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
# 任务外壳和门是后台进程：等它们都退出再结束，现场删掉后不会再有进程往里写
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf 'module demo\n\ngo 1.22\n' > "$p/go.mod"
printf 'package prune\n' > "$p/prune/doc.go"
printf '# 待办\n\n## 待做\n\n- [ ] 加清理可再生目录 —— prune 包里删 build、dist\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

i=0
while [ $i -lt 60 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  # 合并提交先进 main，门的结果 last.json 最后才写：两样都在了再判
  last=$(ls "$HOME"/.agentq/gate/*/last.json 2>/dev/null | head -1)
  if [ -n "$last" ] && git -C "$p" log --format=%B main | grep -q '由 agentq 过门后合并'; then
    r=$(tr -d '\n ' < "$last")
    case "$r" in
      *'"gate":"capability","outcome":"fail"'*) ;;
      *) echo "能力门没报出新加的删目录代码没测试：$(grep -o 'capability[^"]*' "$last" | head -3)"; exit 1 ;;
    esac
    case "$r" in
      *'"caps":[{"name":"'*'","outcome":"fail"}]'*) exit 0 ;;
      *) echo "门的结果里没记下用上的能力：$(head -c 600 "$last")"; exit 1 ;;
    esac
  fi
  sleep 1; i=$((i + 1))
done
echo "60 秒内没合并："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
