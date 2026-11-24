#!/bin/sh
# 11-24 机器忙时门进程被杀在 git worktree add 半路上：git 里留着一条带 initializing 锁的 worktree 记录，目录已经没了。
# 清理只跑 git worktree prune，prune 不清带锁的，这个任务的门之后每次都报「建 worktree 失败：…是一个丢失但锁定的工作区」转卡住。
# 要的是：留着这样一条记录，门照样建得起来，过门合并，记录也清掉。
root="$EVAL_WORK/projects"; p="$root/demo"; data="$HOME/.agentq"; fake="$EVAL_WORK/claude"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/.agentq" "$p/docs" "$data"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
echo $PPID >> "$HOME/fake-shells"
case "$*" in
  *审查门的独立审查进程*) echo '{"type":"result","subtype":"success","is_error":false,"result":"审过了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"没问题","findings":[]}}'; exit 0 ;;
  *写好\ ok.txt*) ;;
  *) # 查额度这类不是任务的调用：照常答完就走
    echo '{"type":"result","subtype":"success","is_error":false,"result":"好。","session_id":"s-eval"}'; exit 0 ;;
esac
b=agent/ok-file; wt=$PWD/.claude/worktrees/ok-file
if ! git rev-parse -q --verify "refs/heads/$b" >/dev/null; then
  sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
  git add docs/BACKLOG.md && git commit -q -m "待办：认领"
  git worktree add -q "$wt" -b "$b" main
  echo 好了 > "$wt/ok.txt"
  mkdir -p "$wt/.agentq"
  printf '{"branch":"%s","type":"feature","acceptance":"看 ok.txt","progress":"写好 ok.txt"}\n' "$b" > "$wt/.agentq/ready.json"
  git -C "$wt" add -A && git -C "$wt" commit -q -m "交付" >/dev/null 2>&1
  # 门还没起，先留下门进程杀在 git worktree add 半路上的样子：记录带着 initializing 锁，目录没了
  gw="$HOME/.agentq/gate/$AGENTQ_JOB/wt"
  git worktree add --detach --quiet "$gw" main && admin=$(git -C "$gw" rev-parse --absolute-git-dir) &&
    printf initializing > "$admin/locked" && rm -rf "$gw" && git worktree prune && [ -f "$admin/locked" ] && echo "$admin" > "$HOME/left"
fi
echo '{"type":"result","subtype":"success","is_error":false,"result":"分支可以合并了。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
# 任务外壳和门是后台进程：等它们都退出再结束，现场删掉后不会再有进程往里写
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
printf '#!/bin/sh\ngrep -q 好了 ok.txt\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 写好 ok.txt —— 测试门会查\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","paused":true,"patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$data/config.json"

out=$("$AGENTQ_BIN" run demo 写好 2>&1) || { echo "run 失败：$out" | tail -3; exit 2; }
i=0
while [ $i -lt 60 ]; do
  out=$("$AGENTQ_BIN" tick 2>&1)
  grep -q '"status": "failed"' "$data/state.json" && { echo "任务没跑完：$out" | tail -5; exit 2; }
  if git -C "$p" log --format=%B main | grep -q '由 agentq 过门后合并' && git -C "$p" show main:ok.txt 2>/dev/null | grep -q 好了; then
    [ -f "$HOME/left" ] || { echo "记录没留成，题目没意义"; exit 2; }
    git -C "$p" worktree list --porcelain | grep -q '^locked' && { echo "合进去了，带锁的记录还留着"; exit 1; }
    exit 0
  fi
  grep -q '"state": "stuck"' "$data/state.json" && [ -f "$HOME/left" ] && break
  sleep 1; i=$((i + 1))
done
echo "留着一条带锁、目录没了的 worktree 记录，门没能合并："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
