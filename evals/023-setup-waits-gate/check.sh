#!/bin/sh
# 11-12：任务已经记成 done、它的门还在跑（gate.state=running）时，安装器判空闲照换安装；_gate 跑完还会 tick 接新活。
# 要的是：门在跑就不换，等门走完（排队的门、已合并的不挡）。
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
mkdir -p "$HOME/.agentq"
st="$HOME/.agentq/state.json"
printf '{"jobs":[{"id":"1112-120000-demo","status":"done","gate":{"state":"running","branch":"agent/1112-demo"}}]}\n' > "$st"
if out=$("$AGENTQ_BIN" setup --yes --claude "$fake" 2>&1); then
  echo "门还在跑，setup 却换了安装：$out" | tail -3
  exit 1
fi
case "$out" in
  *gate*) ;;
  *) echo "setup 没装，但不是因为门在跑：$out" | tail -3; exit 2 ;;
esac
printf '{"jobs":[{"id":"1112-120000-demo","status":"done","gate":{"state":"merged","branch":"agent/1112-demo"}}]}\n' > "$st"
out=$("$AGENTQ_BIN" setup --yes --claude "$fake" 2>&1) || { echo "门走完了 setup 还是不装：$out" | tail -3; exit 1; }
