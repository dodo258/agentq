#!/usr/bin/env bash
# 串行占用一个共享资源后执行命令。默认与 agentq 共用锁目录：~/.agentq/locks 存在就用它（默认后端不是 Claude 时引擎会建），否则 ~/.claude/state/locks。可用 AGENTQ_LOCK_DIR 覆盖。
set -u

usage() {
  echo "用法：with-lock.sh <锁名> <命令...>" >&2
  exit 64
}

[ "$#" -ge 2 ] || usage
name=$1
shift

case "$name" in
  ''|.|..|*/*) echo "锁名不能为空，不能是 . 或 ..，也不能包含 /" >&2; exit 64 ;;
esac

claude_home=${CLAUDE_CONFIG_DIR:-"$HOME/.claude"}
default_locks="$claude_home/state/locks"
[ -d "$HOME/.agentq/locks" ] && default_locks="$HOME/.agentq/locks"
locks_dir=${AGENTQ_LOCK_DIR:-"$default_locks"}
dir="$locks_dir/$name"
reap_dir="$locks_dir/.${name}.reap"
umask 077
mkdir -p "$locks_dir" || exit 73

owner="$$.$RANDOM.$(date +%s)"
cleanup() {
  if [ "$(cat "$dir/owner" 2>/dev/null || true)" = "$owner" ]; then
    rm -rf -- "$dir"
  fi
}

# 最多等待 2 小时；每 5 秒检查一次。旧锁只由一个原子取得的清理者回收，避免误删新锁。
i=1
while [ "$i" -le 1440 ]; do
  if mkdir "$dir" 2>/dev/null; then
    printf '%s\n' "$$" > "$dir/pid"
    printf '%s\n' "$owner" > "$dir/owner"
    trap cleanup EXIT HUP INT TERM
    "$@"
    exit $?
  fi

  pid=$(cat "$dir/pid" 2>/dev/null || true)
  # mkdir 成功到 pid 写出之间不能被竞争者当成死锁回收。
  stale=false
  if [ -z "$pid" ]; then
    # 先试 GNU 写法：GNU 的 -f 是 --file-system，会把文件系统信息打到 stdout 再失败。BSD 的 -c 直接失败，落到 -f。
    modified=$(stat -c %Y "$dir" 2>/dev/null || stat -f %m "$dir" 2>/dev/null || true)
    case "$modified" in
      ''|*[!0-9]*) ;; # 读不出修改时间就当未知：不回收，下一轮再看
      *)
        now=$(date +%s)
        [ $((now-modified)) -gt 30 ] && stale=true
        ;;
    esac
  elif ! kill -0 "$pid" 2>/dev/null; then
    stale=true
  fi
  if [ "$stale" = true ] && mkdir "$reap_dir" 2>/dev/null; then
    current_pid=$(cat "$dir/pid" 2>/dev/null || true)
    if [ -z "$current_pid" ] || ! kill -0 "$current_pid" 2>/dev/null; then
      rm -rf -- "$dir"
    fi
    rmdir "$reap_dir" 2>/dev/null || true
    continue
  fi

  if [ $((i % 60)) -eq 1 ]; then
    echo "等待锁 ${name}（持有进程 ${pid:-未知}）…" >&2
  fi
  sleep 5
  i=$((i + 1))
done

echo "等锁 ${name} 超过 2 小时，放弃" >&2
exit 75
