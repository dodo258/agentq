#!/bin/sh
# 假 CLI：--fake 用它走完引擎的全流程，不调模型。@@TASK@@ 由程序换成这道题的 fake.sh。
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
prompt=""; prev=""
for a in "$@"; do
  [ "$prev" = "-p" ] && prompt="$a"
  prev="$a"
done
result() { # $1 是结果文字，已经是 JSON 字符串里的写法
  printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","session_id":"s-eval","usage":{"input_tokens":1000,"output_tokens":200}}\n' "$1"
}
case "$prompt" in
  /usage)
    echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'
    exit 0 ;;
  *界面门的独立审查进程*)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"看过了","session_id":"s-ui","structured_output":{"verdict":"pass","skill":"loaded","summary":"没问题","findings":[]}}'
    exit 0 ;;
  *审查门的独立审查进程*)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"审过了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"没问题","findings":[]}}'
    exit 0 ;;
esac
# claim 像 agent-task 那样认领第一条待做，开分支和 worktree，之后改动都在 $wt 里。
claim() {
  b=$1
  sed -i.fakebak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md
  rm -f docs/BACKLOG.md.fakebak
  git add docs/BACKLOG.md
  git commit -q -m "待办：认领"
  wt=$PWD/.claude/worktrees/$(basename "$b")
  git worktree add -q "$wt" -b "$b" main
}
# ready 写交付单并提交。$1 类型，$2 提交说明，可以多行。
ready() {
  mkdir -p "$wt/.agentq"
  printf '{"branch":"%s","type":"%s","acceptance":"跑 .agentq/verify","progress":"做好了"}\n' "$b" "$1" > "$wt/.agentq/ready.json"
  git -C "$wt" add -A
  git -C "$wt" commit -q -m "$2"
}
. @@TASK@@
