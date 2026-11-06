mkdir -p wordcount/docs
printf '# 待办\n\n## 待做\n\n- [ ] 第一版（进行中：agent/first 2026-01-01 10:00）\n\n## 已完成\n' > wordcount/docs/BACKLOG.md
printf '.claude/\n' > wordcount/.gitignore
git -C wordcount init -q -b main
git -C wordcount add -A
git -C wordcount commit -q -m 初始项目
git -C wordcount worktree add -q .claude/worktrees/first -b agent/first
wt=wordcount/.claude/worktrees/first
mkdir -p "$wt/.agentq"
printf '#!/bin/sh\nexit 0\n' > "$wt/.agentq/verify"
printf '#!/bin/sh\nexit 0\n' > "$wt/.agentq/smoke"
chmod 755 "$wt/.agentq/verify" "$wt/.agentq/smoke"
printf '#!/bin/sh\nwc -lw\n' > "$wt/wordcount.sh"
printf '{"branch":"agent/first","type":"feature","acceptance":"跑 wordcount.sh","progress":"做出第一版"}\n' > "$wt/.agentq/ready.json"
git -C "$wt" add -A
git -C "$wt" commit -q -m 第一版
result "都做完了。\n\n项目路径：$PWD/wordcount\n补的假设：只给自己用\n第一版去哪看：sh wordcount.sh\n任务分支：agent/first"
