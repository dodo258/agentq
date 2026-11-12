package tests

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 自己长大的验收用例：agentq 自己的仓库（这份源码的副本）当 self_project，放在 exclude_projects 里（维护者的机器就是这样）；
// 从它构建一个带 agentq_fake_deps 的 agentq（版本 dev-<提交>），用安装引擎装进临时 HOME，之后的命令都跑装好的那一代。
// 升级时 agentq evolve apply 照这一代的构建标记构建新的一代、再用它 setup --yes，所以也不联网。

const emptyBacklog = "# 待办\n\n## 待做\n\n## 等用户决定\n\n## 已完成\n"

func selfGrowEnv(t *testing.T, task string, cfg map[string]any) (*testEnv, string) {
	t.Helper()
	e, dir := selfRepoEnv(t, task)
	os.WriteFile(e.fake, []byte(installFakeClaude), 0o755) // 安装引擎要查 CLI 的版本和用法
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(emptyBacklog), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "清空待做")
	full := map[string]any{"self_project": "selfrepo", "exclude_projects": []string{"selfrepo"}, "paused": false}
	for k, v := range cfg {
		full[k] = v
	}
	e.mergeConfig(full)
	bin := filepath.Join(t.TempDir(), "agentq")
	build := exec.Command("go", "build", "-tags", "agentq_fake_deps", "-o", bin, ".")
	build.Dir, build.Env = dir, append(os.Environ(), "HOME="+e.home)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("从 self_project 构建失败：%v\n%s", err, out)
	}
	e.bin = bin
	e.mustRun("setup", "--yes")
	e.mustRun("setup", "--yes") // 再装一代：有回滚点（previous.json），演练要把它放回去
	e.bin = filepath.Join(e.home, ".local", "bin", "agentq")
	head := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	if v := e.mustRun("version"); !strings.Contains(v, "dev-"+head[:7]) || strings.Contains(v, "dirty") {
		t.Fatalf("装好的这一代应该是 dev-%s：%s", head[:7], v)
	}
	return e, dir
}

func (e *testEnv) mergeConfig(kv map[string]any) {
	e.t.Helper()
	path := filepath.Join(e.home, ".agentq", "config.json")
	cfg := map[string]any{}
	json.Unmarshal(testkit.ReadBytes(e.t, path), &cfg)
	for k, v := range kv {
		cfg[k] = v
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) upgrades() string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "evolve", "upgrades.jsonl"))
	return string(b)
}

// waitUpgrade 等 agentq evolve apply 在升级记录里写出 kind 这一行。
func (e *testEnv) waitUpgrade(kind string) {
	e.t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	for !strings.Contains(e.upgrades(), `"kind":"`+kind+`"`) {
		if ups := e.upgrades(); time.Now().After(deadline) || strings.Contains(ups, `"kind":"fail"`) || strings.Contains(ups, `"kind":"drill-fail"`) {
			log, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "evolve", "upgrade.log"))
			e.t.Fatalf("等不到升级记录 %s：\n%s\n日志：\n%s", kind, e.upgrades(), log)
		}
		time.Sleep(time.Second)
	}
	// apply 写完记录才放锁：等它退出，后面的断言和命令不和它抢
	waitFor(e.t, time.Minute, "apply 退出", func() bool {
		out, _ := exec.Command("pgrep", "-f", e.home+".*evolve apply").Output()
		return len(strings.TrimSpace(string(out))) == 0
	})
}

// seedJobs 往 state.json 加结束了的任务（观察期的假数据）：开始于 at 之后，状态 status。
func (e *testEnv) seedJobs(at time.Time, status string, ids ...string) {
	e.t.Helper()
	st := e.state()
	for i, id := range ids {
		start := at.Add(time.Duration(i+1) * time.Minute)
		end := start.Add(time.Minute)
		st.Jobs = append(st.Jobs, &state.Job{ID: id, Project: "web", Title: id, Status: status, StartedAt: start, EndedAt: &end})
	}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		e.t.Fatal(err)
	}
}

func generation(t *testing.T, home string) string {
	t.Helper()
	var r struct {
		Generation string `json:"generation"`
	}
	json.Unmarshal(testkit.ReadBytes(t, filepath.Join(home, ".local", "share", "agentq", "receipt.json")), &r)
	return r.Generation
}

// 验收：用假数据造一个使用中的问题（两次任务超时），从信号到写进待做、自动起任务修好、多一道考题、过评测门、空闲升级、
// 观察期没变差，全程不需要人；新的一代装在新目录，旧的一代原样留着，状态和配置不动。
func TestSelfGrowLoop(t *testing.T) {
	t.Parallel()
	task := `mkdir -p evals/900-self-grow-demo internal/textfmt
printf '{"domain":"engine","kind":"program","expect":{}}\n' > evals/900-self-grow-demo/case.json
printf '#!/bin/sh\n# 自己长大的演示题：修好的问题照这个样子留一道能复现它的题\nexit 0\n' > evals/900-self-grow-demo/check.sh
printf 'package textfmt\n\n// selfGrowFixed：演示用的修复。\nconst selfGrowFixed = true\n' > internal/textfmt/zz_selfgrow.go
`
	e, dir := selfGrowEnv(t, task, nil)
	cfgBefore := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "config.json"))
	genA := generation(t, e.home)
	binA := testkit.ReadBytes(t, filepath.Join(genA, "bin", "agentq"))

	// 使用中的问题：两个任务都超时了（信号来自任务记录），说明里带着假 CLI 认的做法
	for _, id := range []string{"1101-100000-web", "1102-100000-web"} {
		end := time.Now().Add(-time.Hour)
		j := &state.Job{ID: id, Project: "web", Title: "x", Status: state.StatusTimeout, StartedAt: end.Add(-time.Hour), EndedAt: &end,
			Summary: "READYMERGE TASKSCRIPT TYPE=fix BRANCH=agent/t-self"}
		b, _ := json.Marshal(j)
		os.MkdirAll(filepath.Join(e.home, ".agentq", "jobs", id), 0o700)
		os.WriteFile(filepath.Join(e.home, ".agentq", "jobs", id, "meta.json"), b, 0o600)
	}
	e.mustRun("tick") // 收信号：同一类两次，写进 self_project 的待做
	todo := e.backlogOf(dir)
	if !strings.Contains(todo, "- [ ] agentq 自身：任务超时反复出现") || !strings.Contains(todo, "加一道能复现它的评测题") {
		t.Fatalf("要写进待做：\n%s", todo)
	}
	e.mustRun("tick") // 被排除的项目也起：记成自己长大派的，不是亲手派的
	var j *Job
	for _, x := range e.state().Jobs {
		if strings.HasPrefix(x.Title, "agentq 自身：任务超时反复出现") {
			j = x
		}
	}
	if j == nil || !j.SelfGrow || j.Manual {
		t.Fatalf("要自动起任务：%+v", e.state().Jobs)
	}
	waitExit(t, j.PID, 30*time.Second)
	e.mustRun("tick") // 收尾，排队过门、起门进程
	waitFor(t, 6*time.Minute, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick") // 门过了合并；空闲：起 agentq evolve apply
	j = e.jobByTitle(j.Title)
	if j.Gate == nil || j.Gate.State != state.GateMerged || j.Gate.Tally["eval"] == nil || j.Gate.Tally["eval"].Pass == 0 {
		t.Fatalf("要过评测门合并：%+v\n%s", j.Gate, e.mustRun("gate", "report", j.ID))
	}
	if report := e.mustRun("gate", "report", j.ID); !strings.Contains(report, "新加的评测题：evals/900-self-grow-demo") {
		t.Fatalf("评测门要认出新加的题：\n%s", report)
	}
	e.waitUpgrade("upgrade")
	ups := e.upgrades()
	if !strings.Contains(ups, "演练前的回滚点已放回") || strings.Index(ups, `"kind":"drill"`) > strings.Index(ups, `"kind":"upgrade"`) {
		t.Fatalf("第一次升级前先演练整代回滚：\n%s", ups)
	}
	merge := j.Gate.Merge // 引擎合并的那个提交（之后 main 上还有收尾写 BACKLOG 的提交，那个没过门）
	if v := e.mustRun("version"); !strings.Contains(v, "dev-"+merge[:7]) {
		t.Fatalf("要升到过了门的合并提交（%s）：%s", merge[:7], v)
	}
	if !strings.Contains(e.mustRun("eval", "list"), "900-self-grow-demo") {
		t.Fatal("新的一代要带着新加的题")
	}
	if genB := generation(t, e.home); genB == genA || string(testkit.ReadBytes(t, filepath.Join(genA, "bin", "agentq"))) != string(binA) {
		t.Fatalf("新的一代要装在新目录，旧的一代原样留着：%s %s", genA, genB)
	}
	if got := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "config.json")); got != cfgBefore {
		t.Fatalf("配置不能动：\n%s\n%s", cfgBefore, got)
	}
	if e.jobByTitle(j.Title).Gate.State != state.GateMerged {
		t.Fatal("状态要留着")
	}

	// 观察期：升级后的 3 个任务都正常，记「留下」
	var at time.Time
	for _, line := range strings.Split(strings.TrimSpace(e.upgrades()), "\n") {
		var ev struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
		}
		json.Unmarshal([]byte(line), &ev)
		if ev.Kind == "upgrade" {
			at = ev.At
		}
	}
	e.seedJobs(at, state.StatusDone, "o1", "o2", "o3")
	e.mustRun("tick")
	if !strings.Contains(e.upgrades(), `"kind":"kept"`) || strings.Contains(e.upgrades(), `"kind":"rollback"`) {
		t.Fatalf("观察期没变差要留下：\n%s", e.upgrades())
	}
	out := e.mustRun("evolve", "upgrades")
	for _, want := range []string{"演练整代回滚", "升级 dev-", "观察期没变差"} {
		if !strings.Contains(out, want) {
			t.Errorf("agentq evolve upgrades 缺 %s：\n%s", want, out)
		}
	}
}

// 验收：有任务在跑不升级（也不接新活）；升级后任务变差，自动回滚到上一代、推通知说原因，回滚过的版本不再升。
func TestSelfGrowRollback(t *testing.T) {
	t.Parallel()
	e, dir := selfGrowEnv(t, "", map[string]any{"notify_enabled": true})
	e.sgProject("demo", "# 待办\n\n## 待做\n\n- [ ] 慢活 SLEEP=15\n\n## 已完成\n")
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("改了点东西\n"), 0o644)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "修")
	b := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	genA := generation(t, e.home)
	versionA := strings.TrimSpace(e.mustRun("version"))

	e.mustRun("tick")
	slow := e.jobByTitle("慢活 SLEEP=15")
	other := e.sgProject("demo2", "# 待办\n\n## 待做\n\n- [ ] 另一条\n\n## 已完成\n")
	// 升级前的 5 个任务都正常；B 是引擎合并、过了评测门的（假数据）
	e.seedJobs(time.Now().Add(-3*time.Hour), state.StatusDone, "p1", "p2", "p3", "p4", "p5")
	st := e.state()
	end := time.Now().Add(-time.Minute)
	st.Jobs = append(st.Jobs, &state.Job{ID: "1111-000000-selfrepo", Project: "selfrepo", Title: "修", Status: state.StatusDone, StartedAt: end.Add(-time.Hour), EndedAt: &end,
		Gate: &state.GateRecord{State: state.GateMerged, Merge: b, Tally: map[string]*state.GateTally{"eval": {Pass: 1}}}})
	state.Save(filepath.Join(e.home, ".agentq", "state.json"), st)

	if log := e.mustRun("tick"); !strings.Contains(log, "等 1 个在跑或在过门的任务做完，这次不接新活") {
		t.Fatalf("有任务在跑要等：\n%s", log)
	}
	if e.upgrades() != "" || generation(t, e.home) != genA {
		t.Fatalf("有任务在跑不升级：%s", e.upgrades())
	}
	if strings.Contains(e.backlogOf(other.Dir), "进行中") || len(e.state().Jobs) != 7 {
		t.Fatal("等升级时不接新活")
	}
	waitExit(t, slow.PID, 30*time.Second)
	e.mustRun("tick")
	e.waitUpgrade("upgrade")
	if v := e.mustRun("version"); !strings.Contains(v, "dev-"+b[:7]) {
		t.Fatalf("要升到 B：%s", v)
	}

	// 升级后 3 个任务失败、超时：变差，回滚到上一代，推通知，B 不再升
	var at time.Time
	for _, line := range strings.Split(strings.TrimSpace(e.upgrades()), "\n") {
		var ev struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
		}
		json.Unmarshal([]byte(line), &ev)
		if ev.Kind == "upgrade" {
			at = ev.At
		}
	}
	e.seedJobs(at, state.StatusFailed, "q1", "q2")
	e.seedJobs(at.Add(5*time.Minute), state.StatusTimeout, "q3")
	e.mustRun("tick")
	e.waitUpgrade("rollback")
	if v := strings.TrimSpace(e.mustRun("version")); v != versionA || generation(t, e.home) != genA {
		t.Fatalf("要回滚到上一代（%s %s）：%s %s", versionA, genA, v, generation(t, e.home))
	}
	if q := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "notify.json")); !strings.Contains(q, "已回滚") || !strings.Contains(q, "卡在门上 3 个") {
		t.Fatalf("要推通知说原因：\n%s", q)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [ ] agentq 自身：升级后任务变差") {
		t.Fatalf("回滚后要写一条待做去修：\n%s", b)
	}
	// 之后又合并了一个 C（以 B 为祖先）：带着回滚过的改动，修它的那一条合并之前不升
	os.WriteFile(filepath.Join(dir, "more.txt"), []byte("又改了点\n"), 0o644)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "又改")
	c := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	st = e.state()
	cend := time.Now()
	st.Jobs = append(st.Jobs, &state.Job{ID: "1111-000001-selfrepo", Project: "selfrepo", Title: "又改", Status: state.StatusDone, StartedAt: cend.Add(-time.Minute), EndedAt: &cend,
		Gate: &state.GateRecord{State: state.GateMerged, Merge: c, Tally: map[string]*state.GateTally{"eval": {Pass: 1}}}})
	state.Save(filepath.Join(e.home, ".agentq", "state.json"), st)
	e.mustRun("tick")
	time.Sleep(2 * time.Second)
	if n := strings.Count(e.upgrades(), `"kind":"upgrade"`); n != 1 || generation(t, e.home) != genA {
		t.Fatalf("回滚过的版本和带着它的版本都不再升：\n%s", e.upgrades())
	}
	if out := e.mustRun("evolve", "upgrades"); !strings.Contains(out, "回滚") || !strings.Contains(out, "带着这个提交的版本不再升") {
		t.Fatalf("agentq evolve upgrades 要看得到回滚：\n%s", out)
	}
}
