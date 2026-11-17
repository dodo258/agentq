#!/bin/sh
# 11-10、11-14 巡逻两次报「launchd 服务 <前缀>backup 上次退出码 1」，被自己长大当成反复出现的问题写进待做。
# 那个服务是每天 05:30 的备份脚本：有仓库没推上时退出码 1，同一次失败当天的备份报告已经报了（backup:<日期>）；
# 手动重跑修好后报告更新，launchd 的退出码要等第二天定点运行才变，巡逻一直挂着一个已经修好的问题。
# 要的是：日志末尾是备份总结、退出码 1 的服务不按 launchd 报，备份报告照报；别的服务退出码 1 照报。
if [ "$(uname)" != "Darwin" ]; then
  echo "不是 macOS：launchd 服务和从 plist 取日志路径的 plutil 只有 macOS 有，这题跳过"
  exit 0
fi
root="$EVAL_WORK/projects"
mkdir -p "$root" "$HOME/.agentq" "$HOME/.claude/nightly" "$HOME/Library/LaunchAgents"
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":false,"patrol_enabled":false}\n' "$root" > "$HOME/.agentq/config.json"
for name in backup other; do
  cat > "$HOME/Library/LaunchAgents/io.agentq.$name.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>io.agentq.$name</string><key>StandardOutPath</key><string>$EVAL_WORK/$name.log</string></dict></plist>
PLIST
done
printf '备份：6 个仓库已推到 GitHub 私有备份\n备份：10 个仓库已推到 GitHub 私有备份，**1 个失败**；没配备份的： x\n' > "$EVAL_WORK/backup.log"
printf 'error: something else\n' > "$EVAL_WORK/other.log"
printf '备份：10 个仓库已推到 GitHub 私有备份，**1 个失败**\n\n- a：已推送\n- b：**推送失败**：remote: This repository was archived so it is read-only.\n' > "$HOME/.claude/nightly/2026-11-14-backup.md"
fake="$EVAL_WORK/launchctl"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  list) printf 'PID\tStatus\tLabel\n-\t1\tio.agentq.backup\n-\t1\tio.agentq.other\n' ;;
  print) exit 113 ;;
esac
exit 0
FAKE
chmod 755 "$fake"
off=$(date +%z | sed 's/\(..\)$/:\1/')
out=$(AGENTQ_TEST_LAUNCHCTL="$fake" AGENTQ_TEST_NOW="2026-11-14T06:05:00$off" "$AGENTQ_BIN" patrol 2>&1)
pj="$HOME/.agentq/patrol.json"
[ -f "$pj" ] || { echo "巡逻没跑成：$out" | tail -5; exit 2; }
grep -q '"backup:2026-11-14"' "$pj" || { echo "备份报告里的失败没报：$out" | tail -5; exit 1; }
grep -q '"launchd:io.agentq.other"' "$pj" || { echo "别的服务退出码 1 没报：$out" | tail -5; exit 1; }
if grep -q '"launchd:io.agentq.backup"' "$pj"; then
  echo "备份脚本的退出码和备份报告重复报了：$(grep -A3 '"launchd:io.agentq.backup"' "$pj" | head -4)"
  exit 1
fi

# 手动重跑修好了：当天报告没有失败，launchd 退出码还是 1，什么都不报
printf '备份：11 个仓库已推到 GitHub 私有备份\n\n- a：已推送\n' > "$HOME/.claude/nightly/2026-11-14-backup.md"
sed -i '' 's/^  list) .*/  list) printf "PID\\tStatus\\tLabel\\n-\\t1\\tio.agentq.backup\\n" ;;/' "$fake"
out=$(AGENTQ_TEST_LAUNCHCTL="$fake" AGENTQ_TEST_NOW="2026-11-14T13:30:00$off" "$AGENTQ_BIN" patrol 2>&1)
if grep -q '"launchd:io.agentq.backup"\|"backup:2026-11-14"' "$pj"; then
  echo "备份手动重跑修好后还挂着问题：$(grep -o '"fingerprint": *"[^"]*"' "$pj")"
  exit 1
fi
exit 0
