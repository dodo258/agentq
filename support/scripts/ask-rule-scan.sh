#!/usr/bin/env bash
# 只读扫描规则和技能里“要求用户确认/选择”的表述，供复盘核对授权边界；不修改任何文件。
set -u

claude_home=${CLAUDE_CONFIG_DIR:-"$HOME/.claude"}
projects_root=${AGENTQ_PROJECTS_ROOT:-"$HOME/projects"}
pat='问用户|问一下用户|询问用户|让用户选|请用户确认|等用户确认|用户确认后|征求用户|你选|二选一|让你选|先问'

scan_file() {
  file=$1
  [ -f "$file" ] || return 0
  grep -n -E "$pat" "$file" 2>/dev/null | while IFS= read -r line; do
    display=$file
    case "$display" in
      "$HOME"/*) display="~/${display#"$HOME"/}" ;;
    esac
    printf '%s:%s\n' "$display" "$line" | cut -c1-220
  done
}

scan_file "$claude_home/CLAUDE.md"

if [ -d "$claude_home/skills" ]; then
  find -L "$claude_home/skills" -mindepth 2 -maxdepth 2 -type f -name SKILL.md -print0 2>/dev/null |
    while IFS= read -r -d '' file; do scan_file "$file"; done
fi

if [ -d "$projects_root" ]; then
  find "$projects_root" -mindepth 2 -maxdepth 2 -type f \( -name AGENTS.md -o -name CLAUDE.md \) -print0 2>/dev/null |
    while IFS= read -r -d '' file; do scan_file "$file"; done
  find "$projects_root" -mindepth 4 -maxdepth 6 -type f -path '*/.claude/skills/*/SKILL.md' -print0 2>/dev/null |
    while IFS= read -r -d '' file; do scan_file "$file"; done
fi
