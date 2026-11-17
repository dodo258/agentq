package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
)

// 实测 codex-cli 0.169.0（Pro 账号）account/rateLimits/read 的响应，去掉了账号 id：只有一个 10080 分钟的周窗口。
const quotaWeekOnly = `{"id":1,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":37,"windowDurationMins":10080,"resetsAt":1795321074},"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"individualLimit":null,"spendControlReached":false,"planType":"prolite","rateLimitReachedType":null},"rateLimitResetCredits":{"availableCount":3,"credits":null},"rateLimitUpsell":null}}`

// 有 5 小时窗口和周窗口的账号（字段同上，按协议 schema 填）。
const quotaBoth = `{"id":1,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":95,"windowDurationMins":300,"resetsAt":1795000000},"secondary":{"usedPercent":12,"windowDurationMins":10080,"resetsAt":1795321074},"planType":"plus"}}}`

// quotaCLI：假 codex app-server，一行一条 JSON-RPC。先发一条同 id 的服务端请求（带 method，不能当成响应），再回额度；标准输入关了就退出。
func quotaCLI(t *testing.T, resp string) string {
	t.Helper()
	return fakeCLI(t, `[ "$1" = app-server ] || exit 2
while IFS= read -r line; do
  case "$line" in
  *'"method":"initialize"'*) echo '{"id":0,"result":{"userAgent":"fake"}}' ;;
  *'"account/rateLimits/read"'*)
    echo '{"id":1,"method":"item/tool/requestUserInput","params":{}}'
    printf '%s\n' '`+resp+`' ;;
  esac
done
`)
}

func readQuota(t *testing.T, path string, now time.Time) ([]backend.UsageWindow, error) {
	t.Helper()
	q := New(backend.Options{Path: path}).(backend.QuotaReader)
	text, err := q.QuotaText(context.Background())
	if err != nil {
		return nil, err
	}
	return q.ParseQuota(text, now)
}

func TestQuotaSessionAndWeek(t *testing.T) {
	now := time.Unix(1794990000, 0)
	ws, err := readQuota(t, quotaCLI(t, quotaBoth), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Label != "session" || ws[0].Used != 95 || ws[1].Label != "week" || ws[1].Used != 12 {
		t.Fatalf("窗口不对：%+v", ws)
	}
	if ws[0].ResetsAt == nil || ws[0].ResetsAt.Unix() != 1795000000 || ws[1].ResetsAt == nil || ws[1].ResetsAt.Unix() != 1795321074 {
		t.Fatalf("重置时间是 Unix 秒：%+v", ws)
	}
	// 5 小时窗口按 usage_stop_session_pct：会话 95% 过了 90 的会话上限，周 12% 没到。
	u := &backend.UsageInfo{CheckedAt: now, Windows: ws}
	lvl, until, why := backend.UsageLevel(u, now, backend.UsagePolicy{Session: 90, Week: 50})
	if lvl != backend.UsageStop || !strings.Contains(why, "会话 95%") || strings.Contains(why, "本周") || until.Unix() != 1795000000+60 {
		t.Fatalf("会话窗口要按会话上限挡：%d %v %q", lvl, until, why)
	}
	if lvl, _, _ := backend.UsageLevel(u, now, backend.UsagePolicy{Session: 97, Week: 50}); lvl != backend.UsageOK {
		t.Fatalf("会话上限放宽后不挡：%d", lvl)
	}
}

func TestQuotaWeekOnly(t *testing.T) {
	now := time.Unix(1794990000, 0)
	ws, err := readQuota(t, quotaCLI(t, quotaWeekOnly), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Label != "week" || ws[0].Used != 37 || ws[0].ResetsAt == nil {
		t.Fatalf("只有周窗口，不编会话窗口：%+v", ws)
	}
	u := &backend.UsageInfo{CheckedAt: now, Windows: ws}
	// 会话上限再低也不影响：没有会话窗口。
	if lvl, _, _ := backend.UsageLevel(u, now, backend.UsagePolicy{Session: 10, Week: 50}); lvl != backend.UsageOK {
		t.Fatalf("只按周算：%d", lvl)
	}
	if lvl, _, why := backend.UsageLevel(u, now, backend.UsagePolicy{Session: 10, Week: 30}); lvl != backend.UsageStop || !strings.Contains(why, "本周 37%") {
		t.Fatalf("周到了周上限要挡：%d %q", lvl, why)
	}
	if d := backend.UsageDescribe(u, now, backend.UsagePolicy{Week: 80}); !strings.HasPrefix(d, "本周 37%（") || strings.Contains(d, "会话") {
		t.Fatalf("status 的额度一行：%q", d)
	}
}

func TestQuotaUnreadable(t *testing.T) {
	cases := []struct{ name, body string }{
		{"老版本不认这个方法", `[ "$1" = app-server ] || exit 2
while IFS= read -r line; do
  case "$line" in
  *'"account/rateLimits/read"'*) echo '{"id":1,"error":{"code":-32601,"message":"method not found"}}' ;;
  esac
done
`},
		{"没登录 ChatGPT", `while IFS= read -r line; do
  case "$line" in
  *'"account/rateLimits/read"'*) echo '{"id":1,"error":{"code":-32600,"message":"codex account authentication required to read rate limits"}}' ;;
  esac
done
`},
		{"一个窗口都没有", `while IFS= read -r line; do
  case "$line" in
  *'"account/rateLimits/read"'*) echo '{"id":1,"result":{"rateLimits":{"primary":null,"secondary":null}}}' ;;
  esac
done
`},
		{"看不懂", `while IFS= read -r line; do
  case "$line" in
  *'"account/rateLimits/read"'*) echo '{"id":1,"result":"???"}' ;;
  esac
done
`},
		{"没有 app-server", "echo 'error: unrecognized subcommand' >&2\nexit 2\n"},
		{"什么也不回就退出", "read -r line\nexit 0\n"},
	}
	for _, c := range cases {
		if ws, err := readQuota(t, fakeCLI(t, c.body), time.Now()); err == nil {
			t.Errorf("%s：要返回错误（当未知），得到 %+v", c.name, ws)
		}
	}
	if _, err := readQuota(t, "/不存在/codex", time.Now()); err == nil {
		t.Error("CLI 不在：要返回错误")
	}
	// 没退出的错误信息里带 stderr，用户看得出原因。
	_, err := New(backend.Options{Path: fakeCLI(t, "echo 'error: unrecognized subcommand app-server' >&2\nexit 2\n")}).(backend.QuotaReader).QuotaText(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unrecognized subcommand") {
		t.Errorf("错误信息要带 stderr：%v", err)
	}
}

// 不回也不退出：到时间就杀掉，不拖住 tick。
func TestQuotaHangTimesOut(t *testing.T) {
	q := New(backend.Options{Path: fakeCLI(t, "exec sleep 60\n")}).(backend.QuotaReader)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := q.QuotaText(ctx); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("要报超时：%v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("超时后要尽快返回：%v", d)
	}
}

// 拿到响应后 app-server 不肯退出：关了标准输入等一会儿就杀，照样返回结果。
func TestQuotaAnswerThenLinger(t *testing.T) {
	body := `while IFS= read -r line; do
  case "$line" in
  *'"account/rateLimits/read"'*) printf '%s\n' '` + quotaWeekOnly + `'; exec sleep 60 ;;
  esac
done
`
	start := time.Now()
	ws, err := readQuota(t, fakeCLI(t, body), time.Now())
	if err != nil || len(ws) != 1 {
		t.Fatalf("%v %+v", err, ws)
	}
	if d := time.Since(start); d > quotaExitWait+5*time.Second {
		t.Fatalf("不退出的要杀掉：%v", d)
	}
}
