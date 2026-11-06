#!/bin/sh
# 独立于 agentq 二进制存活的只读看门狗；只写本地报告，不恢复调度或推送。
set -u
umask 077
dir=$HOME/.agentq
mkdir -p "$dir" || exit 1
report=$(mktemp "$dir/.watchdog.XXXXXX") || exit 1
trap 'rm -f "$report"' EXIT HUP INT TERM
printf '%s watchdog\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" > "$report"
if "$HOME/.local/bin/agentq" doctor >> "$report" 2>&1; then
 printf 'OK\n' >> "$report"
 code=0
else
 printf 'ERROR: agentq doctor failed; scheduler/config left unchanged\n' >> "$report"
 code=1
fi
mv "$report" "$dir/watchdog-report.txt"
exit "$code"
