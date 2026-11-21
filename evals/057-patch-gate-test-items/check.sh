#!/bin/sh
# 11-21 huitai「托管层稳定性加测」过门：src/host 30 天里算进去的修复有一次是「合并 压力测试」，这次又是加测找出的修复，
# 改动影响面广，补丁门从 warn 升成 error，判成反复打补丁退回，续跑合同改成「重写 src/host」，用户手动停下才没推倒重写。
# 要的是：加测、压测条目修掉的问题不算打补丁（以前的不算进次数，这次的门不跑）；影响面广也不把补丁门升成 error，
# 没在 gates.json 里写 patch=error 就不会自动把合同改成重写。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"
mkdir -p "$p/.agentq" "$p/docs" "$p/core" "$HOME/.agentq"
printf '[user]\n\tname = t\n\temail = t%sexample.invalid\n[commit]\n\tgpgsign = false\n' @ > "$HOME/.gitconfig"
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
b=agent/core-soak; wt=$PWD/.claude/worktrees/core-soak
if [ ! -d "$wt" ]; then
  sed -i.bak "/^- \[ \] /s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md && rm -f docs/BACKLOG.md.bak
  git add docs/BACKLOG.md && git commit -q -m "待办：认领"
  git worktree add -q "$wt" -b "$b" main
  printf 'package core\n\n// A 长跑里查出的边界改好了。\nvar A = 9\n' > "$wt/core/a.go"
  printf 'module demo\n\ngo 1.23\n' > "$wt/go.mod"
  mkdir -p "$wt/.agentq"
  printf '{"branch":"%s","type":"fix","acceptance":"看 core/a.go","progress":"加测完，修掉长跑查出的边界"}\n' "$b" > "$wt/.agentq/ready.json"
  git -C "$wt" add -A && git -C "$wt" commit -q -m "core 长跑加测，修掉查出的边界" >/dev/null 2>&1
fi
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n分支可以合并了。","session_id":"s-eval"}'
FAKE
chmod 755 "$fake"
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap wait_shells EXIT
printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf 'module demo\n\ngo 1.22\n' > "$p/go.mod"
printf '# 待办\n\n## 待做\n\n- [ ] core 稳定性加测 —— 给 core 加长跑测试，修掉查出的问题\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '# 项目说明\n\n检查命令：`.agentq/verify`。\n' > "$p/AGENTS.md"
printf '.claude/worktrees/\n' > "$p/.gitignore"
printf 'package core\n\nvar A = 1\n' > "$p/core/a.go"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m "初始项目") || exit 2
# core/ 最近修过六次：两次是压测、加测条目合进来的，四次是修复条目。这次加测条目算的话就是第五次打补丁
n=2
for msg in "合并 压力测试" "合并 A 算错" "合并 接手中断的「core 加测」" "合并 A 边界不对" "合并 A 还是不对" "合并 A 溢出"; do
  printf 'package core\n\nvar A = %s\n' "$n" > "$p/core/a.go"
  git -C "$p" add -A && git -C "$p" commit -q -m "$msg" -m "Agentq-Type: fix" || exit 2
  n=$((n + 1))
done
printf '{"projects_root":"%s","claude_path":"%s","patrol_enabled":false,"notify_enabled":false,"pm_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

health=$("$AGENTQ_BIN" health demo 2>&1) || { echo "health 报错：$health" | tail -3; exit 2; }
case "$health" in *"已修过 4 次"*) ;; *) echo "压测、加测条目修掉的问题不该算进打补丁的次数（要 4 次）："; echo "$health"; exit 1 ;; esac

i=0
while [ $i -lt 60 ]; do
  "$AGENTQ_BIN" tick >/dev/null 2>&1
  last=$(ls "$HOME"/.agentq/gate/*/last.json 2>/dev/null | head -1)
  if [ -n "$last" ]; then
    r=$(tr -d '\n ' < "$last")
    case "$r" in *'"gate":"patch","outcome":"fail"'*)
      echo "加测条目被补丁门判成反复打补丁：$(head -c 600 "$last")"; exit 1 ;;
    esac
    # 合并提交先进 main，last.json 最后才写：两样都在了再判
    if git -C "$p" log --format=%B main | grep -q '由 agentq 过门后合并'; then
      exit 0
    fi
  fi
  sleep 1; i=$((i + 1))
done
echo "60 秒内没合并："
"$AGENTQ_BIN" status 2>&1 | tail -8
exit 1
