#!/bin/sh
# 11-15 产品经理给官网写了「把官网对齐 v0.3.0 发到线上」：它只读了过期的 PROGRESS.md（写 11-10 部署 v0.2.0），
# 没去看线上，线上 11-13 早已是 v0.3.0。线上现状的判断出处要是当场查的线上来源，只有文档佐证的不能变成建议；
# 文档和线上对不上时，建议是「交接文档和线上不一致，更新文档」。
# 假线上是本地起的页面（已是 v0.3.0）。假模型照结果格式回答：格式里有 online（线上现状要给当场查过的地址）就照填，
# 会去 curl 线上页面；它还是写了一条只凭 PROGRESS 的「发到线上」，引擎要把这条挡下。
root="$EVAL_WORK/projects"; fake="$EVAL_WORK/claude"; calls="$EVAL_WORK/claude-calls"; data="$HOME/.agentq"; site="$EVAL_WORK/site"
mkdir -p "$data" "$site"
printf '[user]\n\tname = t\n\temail = t%sexample.invalid\n[commit]\n\tgpgsign = false\n' @ > "$HOME/.gitconfig"
printf '<!doctype html><title>demo</title><p>demo v0.3.0 已发布</p>\n' > "$site/index.html"
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$site" > "$EVAL_WORK/http.log" 2>&1 &
srv=$!
wait_shells() {
  for pid in $(sort -u "$HOME/fake-shells" 2>/dev/null); do
    i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
  done
}
trap 'wait_shells; kill "$srv" 2>/dev/null' EXIT
port=""; i=0
while [ -z "$port" ] && [ $i -lt 50 ]; do
  port=$(sed -n 's/.* port \([0-9][0-9]*\).*/\1/p' "$EVAL_WORK/http.log" | head -1)
  [ -n "$port" ] || { sleep 0.2; i=$((i + 1)); }
done
[ -n "$port" ] || { echo "本地假线上页面没起来：$(cat "$EVAL_WORK/http.log")"; exit 2; }
live="http://127.0.0.1:$port/"
curl -fsS "$live" | grep -q 'v0.3.0' || { echo "假线上页面打不开：$live"; exit 2; }

cat > "$fake" <<FAKE
#!/bin/sh
case "\$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
[ "\$2" = "/usage" ] && exit 0
echo \$PPID >> "\$HOME/fake-shells"
schema=""; prev=""
for a in "\$@"; do [ "\$prev" = "--json-schema" ] && schema="\$a"; prev="\$a"; done
echo "PWD=\$PWD \$*" | tr '\n' ' ' >> "$calls"; echo >> "$calls"
url=\$(sed -n 's/^线上：//p' README.md)
case "\$schema" in
  *'"online"'*)
    ver=\$(curl -fsS "\$url" | sed -n 's/.*\(v[0-9][0-9.]*\).*/\1/p' | head -1)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-eval","structured_output":{"suggestions":[{"title":"把官网对齐 v0.3.0 发到线上","why":"PROGRESS 写线上还是 11-10 部署的 v0.2.0","source":"PROGRESS.md 写 11-10 部署 v0.2.0","online":"PROGRESS.md 写 11-10 部署 v0.2.0"},{"title":"交接文档和线上不一致，更新文档","why":"线上已是 '"\$ver"'，PROGRESS 还写 11-10 部署 v0.2.0","source":"PROGRESS.md 写 11-10 部署 v0.2.0","online":"'"\$url"'"}]}}' ;;
  *)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-eval","structured_output":{"suggestions":[{"title":"把官网对齐 v0.3.0 发到线上","why":"PROGRESS 写线上还是 11-10 部署的 v0.2.0","source":"PROGRESS.md 写 11-10 部署 v0.2.0"},{"title":"交接文档和线上不一致，更新文档","why":"线上已是新版，PROGRESS 还写 11-10 部署 v0.2.0","source":"'"\$url"'"}]}}' ;;
esac
FAKE
chmod 755 "$fake"
off=$(date +%z | sed 's/\(..\)$/:\1/')
p="$root/site"; mkdir -p "$p/docs"
printf '# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 官网对齐 v0.3.0 —— 合并提交 abc1234\n' > "$p/docs/BACKLOG.md"
printf '# 进度交接\n\n- 11-10 官网部署 v0.2.0，线上是 v0.2.0。\n' > "$p/PROGRESS.md"
printf '# demo 官网\n\n线上：%s\n' "$live" > "$p/README.md"
printf '.claude/\n' > "$p/.gitignore"
(cd "$p" && git init -q -b main && git add -A && git commit -q -m init) || exit 2
printf '{"jobs":[{"id":"1113-100000-site","project":"site","project_dir":"%s","item":"官网对齐 v0.3.0","title":"官网对齐 v0.3.0","fingerprint":"fp-site","pid":0,
 "started_at":"2026-11-13T10:00:00%s","ended_at":"2026-11-13T10:30:00%s","status":"done","summary":"合并提交 abc1234",
 "gate":{"state":"merged","branch":"agent/1113-site","merge":"abc1234"}}],"breaker_until":"0001-01-01T00:00:00Z"}\n' "$p" "$off" "$off" > "$data/state.json"
printf '{"projects_root":"%s","claude_path":"%s","notify_enabled":false,"patrol_enabled":false,"tick_after_job":false}\n' "$root" "$fake" > "$data/config.json"
tick() { AGENTQ_TEST_NOW="2026-11-15T12:00:00$off" "$AGENTQ_BIN" tick 2>&1; }

out=$(tick) || { echo "tick 报错：$out" | tail -3; exit 1; }
wait_shells
grep -q 'WebSearch' "$calls" 2>/dev/null || { echo "做完一版、待做清空后没起调研：$out" | tail -3; exit 1; }
tick >/dev/null
b=$(cat "$p/docs/BACKLOG.md")
sug=$(sed -n '/^## 建议/,$p' "$p/docs/BACKLOG.md")
case "$sug" in
  *"- [ ] 交接文档和线上不一致，更新文档 —— "*"出处：$live"*"（建议 "*) ;;
  *) echo "文档和线上对不上，要建议更新文档，出处带查过的线上地址："; echo "$b"; exit 1 ;;
esac
case "$sug" in
  *"发到线上"*) echo "只凭 PROGRESS.md 判断线上现状，不能建议「发到线上」："; echo "$sug"; exit 1 ;;
esac
exit 0
