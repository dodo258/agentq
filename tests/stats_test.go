package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/testkit"
)

// 真二进制：只读（状态和任务记录一个字节都不变），status 输出不变。
func TestIntegrationStats(t *testing.T) {
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "demo", "- [ ] 第一条 USAGE\n")
	e.mustRun("tick")
	j := e.jobByTitle("第一条 USAGE")
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	state := filepath.Join(e.home, ".agentq", "state.json")
	before, _ := os.ReadFile(state)
	metaPath := filepath.Join(e.home, ".agentq", "jobs", j.ID, "meta.json")
	beforeMeta, _ := os.ReadFile(metaPath)
	statusBefore := e.mustRun("status")
	out := e.mustRun("stats")
	for _, want := range []string{"最近 7 天：1 次　成 1　败 0", "待做任务　1 次", "demo　1 次", "opus　1 次", "token 6.75M"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q：\n%s", want, out)
		}
	}
	if after, _ := os.ReadFile(state); string(after) != string(before) {
		t.Error("stats 不能写状态")
	}
	if after, _ := os.ReadFile(metaPath); string(after) != string(beforeMeta) {
		t.Error("stats 不能写 meta")
	}
	// 抬头是当前时间，两次之间可能跨一分钟，比内容
	body := func(s string) string { _, rest, _ := strings.Cut(s, "\n"); return rest }
	if statusAfter := e.mustRun("status"); body(statusAfter) != body(statusBefore) {
		t.Errorf("status 不变：\n%s\n----\n%s", statusBefore, statusAfter)
	}
	if out := e.mustRun("stats", "30"); !strings.Contains(out, "最近 30 天") {
		t.Errorf("%s", out)
	}
	if _, code := e.run("stats", "abc"); code == 0 {
		t.Error("坏参数要报错")
	}
	if _, code := e.run("stats", "1", "2"); code != 2 {
		t.Errorf("参数个数不对：%d", code)
	}
}
