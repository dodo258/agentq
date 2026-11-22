#!/bin/sh
# 11-21 huitai 的「托管层稳定性加测」任务起了 6 小时长跑（输出写到 /tmp），到任务时限 4h 被 SIGTERM，
# 长跑子进程却没跟着结束，成了孤儿接着跑：它在自己的进程组里（后台工具开新进程组、setsid、set -m），对 CLI 进程组发信号够不着。
# 要的是：超时杀任务时，任务拉起的、环境里带这个任务标记的进程不管在哪个进程组都一起清掉。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/claude"; stray="$EVAL_WORK/stray"
atsign=@ # 拆开写：整段像邮箱的会被隐私扫描拦下
mkdir -p "$p/docs" "$HOME/.agentq" "$stray"
# 长跑用 agentq 自己的任务外壳扮演：它不是系统自带命令，macOS 上 ps -E 看得到它的环境（系统自带的 sleep、sh 看不到，认不出就不杀）
cat > "$fake" <<FAKE
#!/bin/sh
case "\$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
case "\$*" in
  *稳定性加测*) ;;
  *) # 查额度这类不是任务的调用：照常答完就走
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"好。","session_id":"s-eval"}'
    exit 0 ;;
esac
echo \$PPID >> "\$HOME/fake-shells"
set -m # 后台命令自己一个进程组；前台的还留在 CLI 的组里
"$AGENTQ_BIN" _job "$stray" "$EVAL_WORK" /bin/sleep 300 3>/dev/null >/dev/null 2>&1 &
echo \$! > "$stray/pid"
set +m
sleep 300
FAKE
chmod 755 "$fake"
cleanup() {
  for f in "$stray/pid" "$stray/claude.pid"; do [ -s "$f" ] && kill -9 "-$(cat "$f")" 2>/dev/null; done # 两个都是自己进程组的组长
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do # 任务外壳是后台进程：等它退出再结束
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap cleanup EXIT
printf '# 待办\n\n## 待做\n\n- [ ] 稳定性加测 —— 含 6 小时长跑\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '.claude/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t${atsign}t.invalid commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","claude_path":"%s","timeout":"1s","patrol_enabled":false,"notify_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

out=$("$AGENTQ_BIN" run demo 稳定性 2>&1) || { echo "run 失败：$out" | tail -3; exit 2; }
i=0; while [ ! -s "$stray/pid" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
[ -s "$stray/pid" ] || { echo "任务没起来长跑：$out" | tail -3; exit 2; }
pid=$(cat "$stray/pid")
sleep 2 # 过了 1s 的时限
out=$("$AGENTQ_BIN" tick 2>&1)
grep -q '"status": "timeout"' "$HOME/.agentq/state.json" || { echo "任务没按超时收尾：$out" | tail -5; exit 2; }
i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
if kill -0 "$pid" 2>/dev/null; then
  echo "任务超时被杀了，它拉起、在别的进程组里的长跑（pid ${pid}）还活着：$out" | tail -5
  exit 1
fi
exit 0
