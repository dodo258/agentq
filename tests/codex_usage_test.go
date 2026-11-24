package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// codexQuota：假 codex app-server 回的 account/rateLimits/read 响应。windows 是 [已用百分比, 窗口分钟, 重置时刻]，没有的窗口传 nil（回 null）。
func codexQuota(t *testing.T, e *testEnv, primary, secondary []any) {
	t.Helper()
	win := func(w []any) any {
		if w == nil {
			return nil
		}
		return map[string]any{"usedPercent": w[0], "windowDurationMins": w[1], "resetsAt": w[2].(time.Time).Unix()}
	}
	b, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"rateLimits": map[string]any{"limitId": "codex", "primary": win(primary), "secondary": win(secondary)}}})
	os.MkdirAll(filepath.Join(e.home, "fake"), 0o755)
	if err := os.WriteFile(filepath.Join(e.home, "fake", "quota.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func codexQuotaCalls(e *testEnv) []map[string]any {
	b, _ := os.ReadFile(filepath.Join(e.home, "fake", "calls.log"))
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		var c map[string]any
		if json.Unmarshal([]byte(line), &c) == nil && c["ev"] == "quota" {
			out = append(out, c)
		}
	}
	return out
}

func expireCodexUsage(t *testing.T, e *testEnv) {
	t.Helper()
	st := e.state()
	u := st.UsageOf("codex")
	u.CheckedAt = u.CheckedAt.Add(-time.Hour)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
}

// Codex 后端领活前读订阅额度：有 5 小时窗口就按 usage_stop_session_pct，只有周窗口只按周，读不出当未知照常跑；status 的额度一行显示 Codex 各窗口。
func TestCodexUsageGate(t *testing.T) {
	t.Parallel()
	e := codexEnv(t, map[string]any{"usage_stop_session_pct": 80, "usage_stop_week_pct": 50})
	e.extra = append(e.extra, "CODEX_HOME="+filepath.Join(e.home, "ch"), "OPENAI_API_KEY=k1", "CODEX_API_KEY=k2")
	testkit.MkProject(t, e.root, "alpha", codexItem("aaa", "ok")+"\n")
	now := time.Now()
	sessReset, weekReset := now.Add(2*time.Hour).Truncate(time.Minute), now.Add(72*time.Hour).Truncate(time.Minute)

	// 5 小时 + 周：会话 85% 过了 80 的会话上限，周 12% 没到周上限 → 不领新活。
	codexQuota(t, e, []any{85, 300, sessReset}, []any{12, 10080, weekReset})
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 0") || !strings.Contains(out, "订阅额度到了后台上限（会话 85%，后台上限 80%）") || strings.Contains(out, "本周 12%，后台上限") {
		t.Fatalf("会话窗口到了会话上限要挡：\n%s", out)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Fatalf("挡住时不起任务：%d", n)
	}
	calls := codexQuotaCalls(e)
	if len(calls) != 1 || calls[0]["args"] != "app-server" || calls[0]["api_key"] != "" || calls[0]["codex_home"] != filepath.Join(e.home, "ch") ||
		!strings.Contains(fmt.Sprint(calls[0]["cwd"]), "agentq-codex-quota-") {
		t.Fatalf("一次 tick 读一次，起 app-server，在临时目录里，只带 CODEX_HOME 不带 API Key：%+v", calls)
	}
	out = e.mustRun("status")
	want := "额度：会话 85%（" + textfmt.WhenCN(sessReset) + " 重置，后台上限 80%）；本周 12%（" + textfmt.WhenCN(weekReset) + " 重置，后台上限 50%）"
	if !strings.Contains(out, want) || !strings.Contains(out, "调度：订阅额度到了后台上限") {
		t.Fatalf("status 要显示 Codex 各窗口：想要 %q\n%s", want, out)
	}

	// 只有周窗口（实测的 Pro 账号就是这样）：会话上限不管用，周 37% 没到 50 → 照常领活。
	codexQuota(t, e, []any{37, 10080, weekReset}, nil)
	expireCodexUsage(t, e)
	out = e.mustRun("tick")
	if !strings.Contains(out, "启动 1") {
		t.Fatalf("只有周窗口、没到周上限要照常启动：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "额度：本周 37%（"+textfmt.WhenCN(weekReset)+" 重置，后台上限 50%）") || strings.Contains(out, "额度：会话") {
		t.Errorf("只有周窗口就只显示周：\n%s", out)
	}
	waitExit(t, e.jobByTitle("条目 ITEM-aaa").PID, 20*time.Second)
}

// 读不出（老版本没有这个方法、没登录 ChatGPT……）：当未知，照常领活，status 和汇总写明。
func TestCodexUsageUnknown(t *testing.T) {
	t.Parallel()
	e := codexEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", codexItem("bbb", "ok")+"\n")
	out := e.mustRun("tick") // 没有 fake/quota.json：假 app-server 回报错
	if !strings.Contains(out, "启动 1") || !strings.Contains(out, "额度未知，照常启动") {
		t.Fatalf("读不出要照常启动：\n%s", out)
	}
	if u := e.state().UsageOf("codex"); u == nil || !strings.Contains(u.Err, "fake: no rate limits") {
		t.Errorf("state 要记下读失败的原因：%+v", u)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "额度：未知（codex app-server 读额度报错：fake: no rate limits），照常接活") {
		t.Errorf("status 要标出未知：\n%s", out)
	}
	waitExit(t, e.jobByTitle("条目 ITEM-bbb").PID, 20*time.Second)
}
