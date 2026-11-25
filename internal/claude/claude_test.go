package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runner(t *testing.T, script string) (Runner, string, *[]time.Duration) {
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	os.WriteFile(p, []byte("#!/bin/sh\ncd "+dir+"\necho \"$*\" >> calls\nn=$(wc -l < calls | tr -d ' ')\n"+script), 0o755)
	var slept []time.Duration
	return Runner{Path: p, Timeout: 10 * time.Second, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}, dir, &slept
}

func req(dir string) Req {
	return Req{Dir: dir, Prompt: "活", Effort: "high", Model: "opus", Name: "x", Out: filepath.Join(dir, "o"), Log: func(string, ...any) {}}
}

const good = `echo '{"is_error":false,"result":"好","session_id":"s","num_turns":5,"modelUsage":{"m":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4}}}'`

// 撞额度：睡到重置，用同一会话续上。
func TestLimitSleepsThenResumes(t *testing.T) {
	r, dir, slept := runner(t, `[ $n -gt 1 ] && { `+good+`; exit; }
echo '{"is_error":true,"result":"You have hit your session limit · resets 3:45pm","session_id":"s","num_turns":7,"api_error_status":429}'`)
	rep, err := r.Run(t.Context(), req(dir))
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	l := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if err != nil || rep.Text != "好" || rep.Tokens != 10 || len(*slept) != 1 || len(l) != 2 {
		t.Fatalf("%+v %v %v %s", rep, err, *slept, calls)
	}
	if !strings.HasPrefix(l[0], "--session-id "+rep.Session) || !strings.HasPrefix(l[1], "-r "+rep.Session) || !strings.Contains(l[1], "接着做完") {
		t.Errorf("%s", calls)
	}
}

// 上游出错退避重试，一直不行才报错；没开起来的会话换号、照原来的话重来。
func TestUpstream(t *testing.T) {
	r, dir, slept := runner(t, `[ $n -gt 2 ] && { `+good+`; exit; }
echo '{"is_error":true,"result":"API Error: 500","session_id":"","num_turns":0,"api_error_status":500}'`)
	if rep, err := r.Run(t.Context(), req(dir)); err != nil || rep.Text != "好" || len(*slept) != 2 {
		t.Fatalf("%v %v", err, *slept)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "-r ") || strings.Count(string(calls), "-p 活") != 3 {
		t.Errorf("%s", calls)
	}
	r, dir, _ = runner(t, `echo 坏了 >&2; exit 1`)
	if _, err := r.Run(t.Context(), req(dir)); err == nil || !strings.Contains(err.Error(), "坏了") {
		t.Errorf("%v", err)
	}
}

func TestAuth(t *testing.T) {
	r, dir, slept := runner(t, `echo '{"is_error":true,"result":"Please run /login","session_id":"","num_turns":0}'`)
	if _, err := r.Run(t.Context(), req(dir)); err == nil || !strings.Contains(err.Error(), "没登录") || len(*slept) != 0 {
		t.Errorf("%v %v", err, *slept)
	}
}

// 正常做完、结果里提到限流的不算撞额度。
func TestMentionOfLimitIsNotLimit(t *testing.T) {
	r, dir, slept := runner(t, `echo '{"is_error":false,"result":"修好了 rate limit 的代码","session_id":"s","num_turns":30}'`)
	if rep, err := r.Run(t.Context(), req(dir)); err != nil || rep.Text == "" || len(*slept) != 0 {
		t.Errorf("%v %v", err, *slept)
	}
}

func TestResetAt(t *testing.T) {
	now := time.Date(2026, 11, 26, 13, 0, 0, 0, time.Local)
	for text, want := range map[string]time.Time{
		"resets 3:45pm": time.Date(2026, 11, 26, 15, 46, 0, 0, time.Local),
		"没写":            now.Add(30 * time.Minute),
		"resets 9am":    now.Add(30 * time.Minute),
	} {
		if got := ResetAt(text, now); !got.Equal(want) {
			t.Errorf("%s：%s", text, got)
		}
	}
}

// 桌面端会话的 CLAUDE 变量不带给子进程。
func TestEnvClean(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ANTHROPIC_API_KEY", "x")
	for _, kv := range Env() {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "ANTHROPIC_") {
			t.Error(kv)
		}
	}
}
