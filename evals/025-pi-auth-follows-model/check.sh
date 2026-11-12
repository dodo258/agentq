#!/bin/sh
# 11-12：pi 后端启动前的登录检查只查 settings.json 的 defaultProvider。backends.pi.model 写了别家的「提供商/模型」时，
# 查的是默认那家：默认那家没登录就白白挡住，配的那家没登录反倒放行，任务起来才 401。
# 要的是：配置的模型写成「提供商/模型」时，按这个模型查（pi auth check --model）。
root="$EVAL_WORK/projects"; p="$root/demo"; fake="$EVAL_WORK/pi"
mkdir -p "$p/docs" "$p/.agentq" "$HOME/.agentq" "$HOME/.pi/agent"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo 1.5.1; exit 0 ;;
  --help) echo '--mode --session-id --no-approve'; exit 0 ;;
  auth) echo "$*" >> "$HOME/pi-auth"; echo '{"status":"not_ready","provider":"otherp","reason":"credentials_not_configured"}'; exit 1 ;;
esac
exit 1
FAKE
chmod 755 "$fake"
printf '{"defaultProvider":"fakep","defaultModel":"m"}\n' > "$HOME/.pi/agent/settings.json"
printf '#!/bin/sh\nexit 0\n' > "$p/.agentq/verify"
chmod 755 "$p/.agentq/verify"
printf '# 待办\n\n## 待做\n\n- [ ] 写好 ok.txt —— 随便\n\n## 已完成\n' > "$p/docs/BACKLOG.md"
printf '.claude/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t@t commit -q -m "初始项目") || exit 2
printf '{"projects_root":"%s","backend":"pi","permission_mode":"full","backends":{"pi":{"path":"%s","model":"otherp/m1"}},"patrol_enabled":false,"notify_enabled":false}\n' "$root" "$fake" > "$HOME/.agentq/config.json"

out=$("$AGENTQ_BIN" run demo 2>&1) && { echo "没登录也启动了：$out" | tail -3; exit 1; }
grep -q -- '--model otherp/m1' "$HOME/pi-auth" 2>/dev/null || { echo "登录检查没按配置的模型查：$(cat "$HOME/pi-auth" 2>/dev/null)"; exit 1; }
case "$out" in
  *"pi auth login otherp"*) ;;
  *) echo "提示的登录命令不对：$(printf '%s' "$out" | tail -3)"; exit 1 ;;
esac
