package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 假 gh：公开仓上次发布是很久以前的 v0.3.0，CI 绿，发完查 tag、Release 都有。
const releaseFakeGH = `#!/bin/sh
echo "$*" >> "$HOME/gh-calls"
case "$1 $2" in
  release*) echo '[{"tagName":"v0.3.0","publishedAt":"2026-01-01T00:00:00Z"}]' ;;
  "api repos/o/demo/git/ref/tags/"*) printf '{"ref":"refs/tags/%s"}' "${2##*/}" ;;
  "api repos/o/demo/releases/tags/"*) printf '{"tag_name":"%s"}' "${2##*/}" ;;
  api*) echo '{"total_count":1,"check_runs":[{"name":"linux","status":"completed","conclusion":"success"}]}' ;;
esac
`

// releaseDemo：projects_root 下的 demo 带着自动发版规则、CHANGELOG（「未发布」里是 unreleased，空的就是没条目）、记下参数的假 .agentq/release；
// 假 gh 放进 PATH。
func releaseDemo(t *testing.T, e *testEnv, unreleased string) string {
	t.Helper()
	dir := orphanRepo(t, e, "demo", "")
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# demo\n\n- 自动发版：条件满足时发\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(releaseChangelog(unreleased)), 0o644)
	os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
	os.WriteFile(filepath.Join(dir, ".agentq", "release"), []byte("#!/bin/sh\necho \"$* $AGENTQ_RELEASE_REPO\" >> \"$HOME/release-calls\"\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "发版脚本")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(releaseFakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	e.extra = append(e.extra, "PATH="+bin+":"+os.Getenv("PATH"))
	return dir
}

func releaseChangelog(unreleased string) string {
	if unreleased != "" {
		unreleased += "\n\n"
	}
	return "# 更新记录\n\n## 未发布\n\n" + unreleased + "## v0.3.0\n\n- 旧\n"
}

// 自动发版走真二进制：tick 判条件、起脱离 tick 的 agentq _release，后台进程跑项目的 .agentq/release（检查、真发各一次），
// 记 CHANGELOG、记发成；下一次 tick 不再发。临时 HOME、临时 projects_root、假 gh、假发版脚本。
func TestReleaseTickRunsScriptOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": false, "release_repos": map[string]string{"demo": "o/demo"}})
	dir := releaseDemo(t, e, "- 新功能甲")

	out := e.mustRun("tick")
	if !strings.Contains(out, "自动发版 demo：开始发 v0.4.0") {
		t.Fatalf("要开始发：\n%s", out)
	}
	// 后台进程最后写的是日志里「发了」那一行（记录文件在它之前写），等它出现再断言
	logPath := filepath.Join(e.home, ".agentq", "release", "demo.log")
	waitFor(t, 20*time.Second, "后台进程发完", func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), "自动发版 demo：发了 v0.4.0")
	})
	var rec struct {
		State, Version string
	}
	if b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "release", "demo.json")); err != nil || json.Unmarshal(b, &rec) != nil || rec.State != "done" || rec.Version != "v0.4.0" {
		t.Fatalf("记成发成：%+v %v", rec, err)
	}
	calls, _ := os.ReadFile(filepath.Join(e.home, "release-calls"))
	if string(calls) != "--check v0.4.0 o/demo\nv0.4.0 o/demo\n" {
		t.Fatalf("脚本要检查、真发各一次：\n%s", calls)
	}
	if cl := testkit.ReadFile(t, filepath.Join(dir, "CHANGELOG.md")); !strings.Contains(cl, "## 未发布\n\n## v0.4.0\n\n- 新功能甲") {
		t.Errorf("CHANGELOG 要记成 v0.4.0：\n%s", cl)
	}
	testkit.AssertContains(t, testkit.ReadFile(t, logPath), "自动发版 demo：跑 .agentq/release --check v0.4.0", "自动发版 demo：跑 .agentq/release v0.4.0")

	out = e.mustRun("tick")
	if strings.Contains(out, "开始发") {
		t.Fatalf("发过了不再发：\n%s", out)
	}
	if calls2, _ := os.ReadFile(filepath.Join(e.home, "release-calls")); string(calls2) != string(calls) {
		t.Errorf("脚本只调这一次：\n%s", calls2)
	}
}

// 等发版（真二进制）：「未发布」没条目时有任务在跑照常接活；条目有了、只差有任务在跑：tick 不接新活，agentq status 说在等几个；
// 在跑的做完，下一次 tick 起发版（发版中也不接）；发完恢复接活。
func TestReleaseWaitsForRunningJobs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": false, "release_repos": map[string]string{"demo": "o/demo"}})
	dir := releaseDemo(t, e, "")
	orphanRepo(t, e, "work", "- [ ] 慢活 —— SLEEP=25\n")

	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") || strings.Contains(out, "等发版") {
		t.Fatalf("没有要发的照常接活：\n%s", out)
	}
	slow := e.jobByTitle("慢活")
	orphanRepo(t, e, "work2", "- [ ] 第二条 —— SLEEP=1\n- [ ] 第三条 —— SLEEP=1\n") // 同一个项目同时只跑一条
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") || strings.Contains(out, "等发版") {
		t.Fatalf("CHANGELOG 没条目不等发版，有任务在跑照常接活：\n%s", out)
	}
	waitExit(t, e.jobByTitle("第二条").PID, 20*time.Second)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(releaseChangelog("- 新功能甲")), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "记一条")

	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 0") || !strings.Contains(out, "等发版：停接新活，等 1 个任务做完（发 demo v0.4.0）") || strings.Contains(out, "开始发") {
		t.Fatalf("只差有任务在跑：等发版、不接新活：\n%s", out)
	}
	if st := e.mustRun("status"); !strings.Contains(st, "等发版：停接新活，等 1 个任务做完") {
		t.Fatalf("agentq status 要说在等发版：\n%s", st)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Fatalf("等发版时不接新活：\n%s", out)
	}
	if j := e.jobByTitle("慢活"); j.Status != "running" {
		t.Fatalf("慢活应该还在跑（不然上面等的就不是它）：%+v", j)
	}

	waitExit(t, slow.PID, 40*time.Second)
	out = e.mustRun("tick")
	if !strings.Contains(out, "自动发版 demo：开始发 v0.4.0") || !strings.Contains(out, "启动 0") {
		t.Fatalf("在跑的做完了下一次 tick 起发版、不接新活：\n%s", out)
	}
	logPath := filepath.Join(e.home, ".agentq", "release", "demo.log")
	waitFor(t, 20*time.Second, "后台进程发完", func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), "自动发版 demo：发了 v0.4.0")
	})
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") || strings.Contains(out, "等发版") {
		t.Fatalf("发完恢复接活：\n%s", out)
	}
	if st := e.mustRun("status"); strings.Contains(st, "等发版") || strings.Contains(st, "发版：") {
		t.Errorf("发完 status 不再说发版：\n%s", st)
	}
}

// 等发版时不升级（自己长大）：别的项目在等发版，self_project 合并了过评测门的新提交，有升级要做也不升、不挡着说升级；
// 在跑的做完先发版，发版中也不升；发完下一次 tick 才升级。
func TestSelfGrowWaitsForRelease(t *testing.T) {
	t.Parallel()
	e, self := selfGrowEnv(t, "", map[string]any{"release_repos": map[string]string{"demo": "o/demo"}})
	genA := generation(t, e.home)
	dir := releaseDemo(t, e, "")
	e.sgProject("work", "# 待办\n\n## 待做\n\n- [ ] 慢活 SLEEP=20\n\n## 已完成\n")
	e.mustRun("tick")
	slow := e.jobByTitle("慢活 SLEEP=20")
	e.sgProject("work2", "# 待办\n\n## 待做\n\n- [ ] 另一条\n\n## 已完成\n")
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(releaseChangelog("- 新功能甲")), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "记一条")
	if out := e.mustRun("tick"); !strings.Contains(out, "等发版：停接新活，等 1 个任务做完") {
		t.Fatalf("要等发版：\n%s", out)
	}

	// self_project 合并了过评测门的 B（假数据）：平时这时会挡着新活等空闲升级
	os.WriteFile(filepath.Join(self, "fix.txt"), []byte("改了点东西\n"), 0o644)
	testkit.GitT(t, e.home, self, "add", "-A")
	testkit.GitT(t, e.home, self, "commit", "-q", "-m", "修")
	b := testkit.GitT(t, e.home, self, "rev-parse", "HEAD")
	st := e.state()
	end := time.Now().Add(-time.Minute)
	st.Jobs = append(st.Jobs, &state.Job{ID: "1111-000000-selfrepo", Project: "selfrepo", Title: "修", Status: state.StatusDone, StartedAt: end.Add(-time.Hour), EndedAt: &end,
		Gate: &state.GateRecord{State: state.GateMerged, Merge: b, Tally: map[string]*state.GateTally{"eval": {Pass: 1}}}})
	state.Save(filepath.Join(e.home, ".agentq", "state.json"), st)
	out := e.mustRun("tick")
	if !strings.Contains(out, "等发版：停接新活，等 1 个任务做完") || strings.Contains(out, "自己长大") {
		t.Fatalf("等发版时不看升级：\n%s", out)
	}
	if strings.Contains(e.backlogOf(filepath.Join(e.root, "work2")), "进行中") {
		t.Fatal("等发版时不接新活")
	}

	waitExit(t, slow.PID, 40*time.Second)
	out = e.mustRun("tick")
	if !strings.Contains(out, "自动发版 demo：开始发 v0.4.0") || strings.Contains(out, "自己长大") {
		t.Fatalf("空闲了先发版，不升级：\n%s", out)
	}
	waitFor(t, 20*time.Second, "后台进程发完", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "release", "demo.log"))
		return strings.Contains(string(b), "自动发版 demo：发了 v0.4.0")
	})
	if e.upgrades() != "" || generation(t, e.home) != genA {
		t.Fatalf("发版前后都没升级：%s", e.upgrades())
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "自己长大：升级到 "+b[:7]) {
		t.Fatalf("发完再升级：\n%s", out)
	}
	e.waitUpgrade("upgrade")
}
