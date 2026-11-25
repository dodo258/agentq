#!/bin/sh
# 11-17：门停住的任务在命令行（会话里）用 agentq gate pass|abandon 这类命令定过以后，之前推到 Telegram 的门卡片还挂着四个按钮；
# 再点要么回「没记下，过一会儿再点」，要么把另一个决定写给门，像是坏了。
# 要的是：门已经有了决定或任务不再停在门上时，按钮回「已经定过了」并把消息改成结论、去掉按钮；
# 落决定的那次 tick 里，notify.json 里记着消息号的门卡片就改成结论。
command -v python3 >/dev/null 2>&1 || { echo "没有 python3，起不了假 Telegram，这次判不了"; exit 2; }
id=1117-000000-demo
mkdir -p "$HOME/.agentq/secrets" "$HOME/.agentq/gate/$id" "$EVAL_WORK/projects"
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '{"projects_root":"%s","claude_path":"/nonexistent/claude","paused":true,"notify_enabled":true,"patrol_enabled":false}\n' "$EVAL_WORK/projects" > "$HOME/.agentq/config.json"
printf '{"jobs":[{"id":"%s","project":"demo","project_dir":"%s/projects/demo","item":"修东西","title":"修东西","fingerprint":"fp","pid":0,"started_at":"%s","status":"done","gate":{"state":"stuck","branch":"agent/t-a","head":"0123456789abcdef","ready":{}}}]}\n' "$id" "$EVAL_WORK" "$now" > "$HOME/.agentq/state.json"
printf '{"decide_init":true,"cards":{"0123456789ab":{"id":"0123456789ab","project":"demo","fingerprint":"fp","text_fp":"","title":"修东西","options":[{"label":"A","text":"放行"},{"label":"B","text":"退回"},{"label":"C","text":"放弃"},{"label":"D","text":"再审一次"}],"recommend":"A","at":"%s","gate_job":"%s","message_id":1001}}}\n' "$now" "$id" > "$HOME/.agentq/notify.json"
printf 'TELEGRAM_BOT_TOKEN=123:abc\nTELEGRAM_CHAT_ID=42\n' > "$HOME/.agentq/secrets/telegram.env"
chmod 600 "$HOME/.agentq/secrets/telegram.env"

# 假 Telegram：第一次 getUpdates 交出一次点「A 放行」的回调，其余请求原样记进 calls.log
cat > "$EVAL_WORK/tg.py" <<'EOF'
import http.server, json, socketserver, sys, urllib.parse
log, portfile = sys.argv[1], sys.argv[2]
served = []
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        form = urllib.parse.parse_qs(self.rfile.read(n).decode())
        method = self.path.rsplit("/", 1)[-1]
        with open(log, "a") as f:
            f.write(json.dumps({"method": method, "form": {k: v[0] for k, v in form.items()}}, ensure_ascii=False) + "\n")
        result = True
        if method == "getUpdates":
            result = []
            if not served:
                served.append(1)
                result = [{"update_id": 7, "callback_query": {"id": "cb", "from": {"id": 42, "is_bot": False}, "data": "dc:0123456789ab:A", "chat_instance": "x",
                    "message": {"message_id": 1001, "date": 0, "chat": {"id": 42, "type": "private"}}}}]
        elif method in ("sendMessage", "editMessageText"):
            result = {"message_id": 2000, "date": 0, "chat": {"id": 42, "type": "private"}}
        body = json.dumps({"ok": True, "result": result}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    do_GET = do_POST
    def log_message(self, *a): pass
s = socketserver.TCPServer(("127.0.0.1", 0), H)  # HTTPServer 启动时反查本机域名，CI 的 macOS 机器上会卡很久
open(portfile, "w").write(str(s.server_address[1]))
s.serve_forever()
EOF
calls="$EVAL_WORK/calls.log"
python3 -I "$EVAL_WORK/tg.py" "$calls" "$EVAL_WORK/port" 2> "$EVAL_WORK/tg.err" &
srv=$!
trap 'kill $srv 2>/dev/null; wait $srv 2>/dev/null' EXIT
# 临时 HOME 下第一次起 python 可能要十几秒（CI 的 macOS 机器上 5 秒不够），最多等 30 秒
i=0; while [ ! -s "$EVAL_WORK/port" ] && kill -0 $srv 2>/dev/null && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done
[ -s "$EVAL_WORK/port" ] || { echo "假 Telegram 没起来，这次判不了：$(tail -3 "$EVAL_WORK/tg.err" 2>/dev/null)"; exit 2; }
export AGENTQ_TELEGRAM_API="http://127.0.0.1:$(cat "$EVAL_WORK/port")"

# 用放弃不用放行：放行落下后 tick 会起后台过门进程，判完分删现场时它还在写文件
"$AGENTQ_BIN" gate abandon "$id" >/dev/null 2>&1 || { echo "agentq gate abandon 没记下，这次判不了"; exit 2; }
"$AGENTQ_BIN" tick >/dev/null 2>&1
"$AGENTQ_BIN" tick >/dev/null 2>&1

grep -q '"method": "getUpdates"' "$calls" 2>/dev/null || { echo "tick 没去收按钮，这次判不了：$(head -c 400 "$calls" 2>/dev/null)"; exit 2; }
ans=$(grep '"method": "answerCallbackQuery"' "$calls" | head -1)
case "$ans" in
  *"已经定过了"*) ;;
  *) echo "命令行放弃后点门卡片上的「放行」，没回已经定过了：$ans"; exit 1 ;;
esac
if ! grep '"method": "editMessageText"' "$calls" | grep '"message_id": "1001"' | grep -F '"reply_markup": "{\"inline_keyboard\":[]}"' | grep -q '已经定过了'; then
  echo "门卡片的消息没改成「已经定过了」的结论、去掉按钮：$(grep editMessageText "$calls" | head -c 400)"; exit 1
fi
if [ -f "$HOME/.agentq/gate/$id/decision.json" ] && grep -q pass "$HOME/.agentq/gate/$id/decision.json"; then
  echo "命令行放弃之后，卡片上的「放行」又写给了门：$(cat "$HOME/.agentq/gate/$id/decision.json")"; exit 1
fi
exit 0
