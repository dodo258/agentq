package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// state.json 内容坏了：tick 备份坏文件、按 meta.json 重建，运行中的任务照常超时被杀，写汇总报警，那次不启动新任务。
func TestIntegrationCorruptStateRecovers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"timeout": "1s"})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 睡很久 —— SLEEP=30\n- [ ] 第二条 —— x\n")
	e.mustRun("tick")
	j := e.jobByTitle("睡很久")
	statePath := filepath.Join(e.home, ".agentq", "state.json")
	bad := []byte(`{"jobs": [ {"id": `)
	if err := os.WriteFile(statePath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "状态文件损坏，下次 tick 会备份它") {
		t.Errorf("status 应说明状态文件损坏：\n%s", out)
	}
	time.Sleep(1500 * time.Millisecond)

	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 0") || !strings.Contains(out, "状态文件损坏后按 meta.json 重建，本次只收尾不启动") {
		t.Errorf("tick 应按 meta.json 收尾、不启动：\n%s", out)
	}
	baks, _ := filepath.Glob(statePath + ".bad-*")
	if len(baks) != 1 {
		t.Fatalf("应备份出 1 个 state.json.bad-*：%v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); !bytes.Equal(b, bad) {
		t.Errorf("备份内容应是坏文件原样：%q", b)
	}
	if got := e.jobByTitle("睡很久"); got.Status != state.StatusTimeout {
		t.Errorf("应按 meta.json 的启动时间超时：%+v", got)
	}
	if err := syscall.Kill(-j.PID, 0); err == nil {
		t.Errorf("进程组 %d 应已被杀掉", j.PID)
	}
	n := e.nightly()
	if !strings.Contains(n, "状态文件损坏：") || !strings.Contains(n, "已备份为 "+baks[0]) || !strings.Contains(n, "按 1 个任务的 meta.json 重建（运行中 1 个") {
		t.Errorf("当天汇总应报警：\n%s", n)
	}
	if !strings.Contains(n, "alpha「睡很久」超时") {
		t.Errorf("当天汇总应有超时那一行：\n%s", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "jobs")); len(entries) != 1 {
		t.Errorf("重建那次不应启动新任务：%v", entries)
	}

	// 重建的状态已保存，下一次 tick 照常接活
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Errorf("重建后下一次 tick 应照常启动：\n%s", out)
	}
	waitExit(t, e.jobByTitle("第二条").PID, 10*time.Second)
}

// 启动后写 state.json 失败：刚启动的 claude 要马上被杀，任务记失败并写汇总，不留下没人管的进程。
func TestIntegrationSaveFailKillsJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 睡很久 —— SLEEP=30\n")
	e.extra = []string{"AGENTQ_FAKE_BREAK_STATE=1"}
	out, _ := e.run("tick")
	if !strings.Contains(out, "启动后写状态文件失败，已杀掉刚启动的 claude") {
		t.Errorf("应记日志说明杀掉了：\n%s", out)
	}
	metas, _ := filepath.Glob(filepath.Join(e.home, ".agentq", "jobs", "*", "meta.json"))
	if len(metas) != 1 {
		t.Fatalf("应有 1 个任务的 meta.json：%v", metas)
	}
	b, _ := os.ReadFile(metas[0])
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	if j.Status != state.StatusFailed || j.PID <= 0 || !strings.Contains(j.Summary, "启动后写状态文件失败") {
		t.Errorf("任务应记失败：%+v", j)
	}
	waitExit(t, j.PID, 3*time.Second)
	if err := syscall.Kill(-j.PID, 0); err == nil {
		t.Errorf("进程组 %d 应已被杀掉", j.PID)
	}
	if n := e.nightly(); !strings.Contains(n, "alpha「睡很久」失败：启动后写状态文件失败，已杀掉刚启动的 claude") {
		t.Errorf("当天汇总应有这一行：\n%s", n)
	}
}

// 传给 claude 的环境变量按白名单：ANTHROPIC_* 和其他密钥都不传，白名单里的照传。
func TestIntegrationEnvWhitelist(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲 —— x\n")
	e.extra = []string{"ANTHROPIC_API_KEY=sk-ant-test", "ANTHROPIC_AUTH_TOKEN=tok-test", "ANTHROPIC_BASE_URL=http://127.0.0.1:1",
		"OPENAI_API_KEY=sk-test", "GITHUB_TOKEN=ghp-test", "SSH_AUTH_SOCK=/tmp/agentq-test.sock",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-test", "LC_CTYPE=UTF-8", "TMPDIR=/tmp/agentq-tmp/"}
	e.mustRun("tick")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 10*time.Second)
	b, err := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j.PID)))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, want := range []string{"ENV[PATH=", "ENV[HOME=" + e.home + "]", "ENV[SSH_AUTH_SOCK=/tmp/agentq-test.sock]",
		"ENV[CLAUDE_CODE_OAUTH_TOKEN=oauth-test]", "ENV[LC_CTYPE=UTF-8]", "ENV[TMPDIR=/tmp/agentq-tmp/]", "ENV[AGENTQ_JOB=" + j.ID + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("应传 %s：\n%s", want, log)
		}
	}
	// sh 自己会设 PWD、SHLVL、_、OLDPWD；除此之外只能有白名单里的
	for _, line := range strings.Split(log, "\n") {
		kv, ok := strings.CutPrefix(line, "ENV[")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case proc.ChildEnvKeep[name], strings.HasPrefix(name, "LC_"), strings.HasPrefix(name, "AGENTQ_"),
			name == "PWD", name == "SHLVL", name == "_", name == "OLDPWD":
		default:
			t.Errorf("不在白名单的变量传给了 claude：%s", line)
		}
	}
}
