package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/testkit"
)

// envBin 写一个假的被评判 agentq：001 题的 setup 先打一大段自检（超过错误里留的 400 字），最后报取锁定依赖失败，退出码 1；
// failFirst > 0 时只前几次这样（偶发的网络抖动），之后照真的走。plan 为真时 setup --plan 也失败（真改坏了，007 判错）。
// 返回记 001 题 setup 次数的文件。
func envBin(t *testing.T, dir string, failFirst int, plan bool) (bin, calls string) {
	t.Helper()
	calls = filepath.Join(dir, "setup-001")
	report := `{ "os_version": "27.2.1", "system": "darwin", "architecture": "arm64", "git": "git version 2.54.0", "cli_path": "` + strings.Repeat("/x", 120) + `/001-impl-go/tools/claude", "cli_version": "2.1.296 (Claude Code)", "headless": true, "authenticated": true }`
	body := "#!/bin/sh\n"
	if plan {
		body += "[ \"$1 $2\" = 'setup --plan' ] && { echo 装不上 >&2; exit 3; }\n"
	}
	body += "case \"$*\" in setup*001-impl-go/tools/claude*)\n" +
		"  echo x >> " + fsx.ShQuote(calls) + "\n"
	if failFirst > 0 {
		body += "  [ \"$(wc -l < " + fsx.ShQuote(calls) + ")\" -gt " + strconv.Itoa(failFirst) + " ] && exec " + fsx.ShQuote(agentqBin) + " \"$@\"\n"
	}
	body += "  printf '%s\\nSupport interpreter: Python 3.14.5\\n' " + fsx.ShQuote(report) + "\n" +
		"  echo 'agentq: fetch locked third party: exit status 128' >&2\n" +
		"  exit 1;;\nesac\n" +
		"exec " + fsx.ShQuote(agentqBin) + " \"$@\"\n"
	bin = filepath.Join(dir, "env-agentq")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func setupCalls(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

// eval judge：候选上因为取依赖失败判不了的题重跑一次，还不行就单列成环境问题，不算变差，退出码 0；
// 同一次里真变差的题照旧算变差。偶发失败重跑过了就什么都不记。
func TestEvalJudgeEnvNotWorse(t *testing.T) {
	t.Parallel()
	home, tmp := evalHome(t)
	if out, code := runEval(t, home, tmp, "run", "--fake", "001", "002", "007"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := runEval(t, home, tmp, "baseline"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	verdictPath := filepath.Join(tmp, "verdict.json")
	readVerdict := func() gate.EvalVerdict {
		var v gate.EvalVerdict
		if err := json.Unmarshal([]byte(testkit.ReadFile(t, verdictPath)), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// 一直取不到依赖，同时 007 真改坏了。
	bin, calls := envBin(t, t.TempDir(), 0, true)
	out, code := runEval(t, home, tmp, "judge", "--bin", bin, "--out", verdictPath, "001", "002", "007")
	v := readVerdict()
	worse := strings.Join(v.Worse, "\n")
	if code != 1 || !strings.Contains(worse, "007-install-existing-skill：分数 100 → 0") || strings.Contains(worse, "001-impl-go") {
		t.Fatalf("真变差照旧拦，取依赖失败的不算变差：%d %+v\n%s", code, v, out)
	}
	if len(v.Env) != 1 || v.Env[0] != "001-impl-go（取锁定的第三方依赖失败）" || len(v.Unjudged) != 0 {
		t.Fatalf("环境问题要单列：%+v\n%s", v, out)
	}
	if n := setupCalls(t, calls); n != 2 || !strings.Contains(out, "1 题因为环境问题判不了，重跑一次") {
		t.Fatalf("环境问题重跑一次：setup %d 次\n%s", n, out)
	}

	// 只有取不到依赖：没有变差，退出码 0，告诉门按判不了处理。
	bin, _ = envBin(t, t.TempDir(), 0, false)
	out, code = runEval(t, home, tmp, "judge", "--bin", bin, "--out", verdictPath, "001", "002")
	if v = readVerdict(); code != 0 || len(v.Worse) != 0 || len(v.Env) != 1 || !v.Baseline || !strings.Contains(out, "评测门按判不了处理") {
		t.Fatalf("%d %+v\n%s", code, v, out)
	}

	// 偶发：第一次取不到，重跑好了。
	bin, calls = envBin(t, t.TempDir(), 1, false)
	out, code = runEval(t, home, tmp, "judge", "--bin", bin, "--out", verdictPath, "001", "002")
	if v = readVerdict(); code != 0 || len(v.Worse) != 0 || len(v.Env) != 0 || len(v.Failed) != 0 || setupCalls(t, calls) != 2 {
		t.Fatalf("重跑好了就不记：%d %+v\n%s", code, v, out)
	}
}
