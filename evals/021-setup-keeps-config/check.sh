#!/bin/sh
# 已经在用的人重装或升级：setup 只补缺的，用户改过的配置值、开关都原样留着，已有的任务记录不动。
fake="$EVAL_WORK/claude"
cat > "$fake" <<'FAKE'
#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)' ;;
  --help) echo '--print --output-format --session-id --effort' ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}' ;;
  *) echo '{"is_error":false,"result":"ready"}' ;;
esac
FAKE
chmod 755 "$fake"
out=$("$AGENTQ_BIN" setup --yes --claude "$fake" 2>&1) || { echo "第一次 setup 失败：$out" | tail -3; exit 1; }
cfg="$HOME/.agentq/config.json"
[ -f "$cfg" ] || { echo "setup 没写配置"; exit 1; }
# 用户自己改配置：不暂停、并发 1、记录永久保留、夜班打开
sed -i.bak -e 's/"paused": *true/"paused": false/' -e 's/"max_parallel": *[0-9]*/"max_parallel": 1/' \
  -e 's/"retention_days": *[0-9]*/"retention_days": 0/' -e 's/"night_enabled": *false/"night_enabled": true/' "$cfg" && rm -f "$cfg.bak"
for kv in '"paused": false' '"max_parallel": 1' '"retention_days": 0' '"night_enabled": true'; do
  grep -q "$kv" "$cfg" || { echo "题目布置不出 $kv（配置格式变了？）：$(head -c 300 "$cfg")"; exit 2; }
done
mkdir -p "$HOME/.agentq/jobs/1101-120000-demo"
printf '{"id":"1101-120000-demo","status":"done"}\n' > "$HOME/.agentq/jobs/1101-120000-demo/meta.json"
out=$("$AGENTQ_BIN" setup --yes --claude "$fake" 2>&1) || { echo "第二次 setup 失败：$out" | tail -3; exit 1; }
for kv in '"paused": false' '"max_parallel": 1' '"retention_days": 0' '"night_enabled": true'; do
  grep -q "$kv" "$cfg" || { echo "重装把用户的 $kv 改了：$(tr -d '\n' < "$cfg" | head -c 400)"; exit 1; }
done
[ -f "$HOME/.agentq/jobs/1101-120000-demo/meta.json" ] || { echo "重装删了任务记录"; exit 1; }
