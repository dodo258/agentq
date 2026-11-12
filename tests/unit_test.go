package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func ptime(t time.Time) *time.Time { return &t }

func TestRetryBlocked(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	retry := 12 * time.Hour
	st := &State{Jobs: []*Job{
		{Fingerprint: "a", Status: state.StatusDone, EndedAt: ptime(now.Add(-13 * time.Hour))},
		{Fingerprint: "b", Status: state.StatusFailed, EndedAt: ptime(now.Add(-13 * time.Hour))},
		{Fingerprint: "b", Status: state.StatusDone, EndedAt: ptime(now.Add(-1 * time.Hour))},
		{Fingerprint: "c", Status: state.StatusFailed, EndedAt: ptime(now.Add(-1 * time.Hour)), LimitHit: true},
		{Fingerprint: "d", Status: state.StatusRunning},
		{Fingerprint: "e", Status: state.StatusTimeout, EndedAt: ptime(now.Add(-11 * time.Hour))},
	}}
	cases := map[string]bool{
		"a": false, // 上次结束已超过 12h
		"b": true,  // 最近一次只过了 1h
		"c": false, // 撞用量上限不算一次尝试
		"d": false, // 只有运行中的，不算结束
		"e": true,  // 超时也算试过
		"z": false, // 从没跑过
	}
	for fp, want := range cases {
		if got := state.RetryBlocked(st, "", fp, now, retry); got != want {
			t.Errorf("指纹 %s：得到 %v，应为 %v", fp, got, want)
		}
	}
}

func TestStatePruneAndFind(t *testing.T) {
	now := time.Now()
	st := &State{Jobs: []*Job{
		{ID: "0101-000000-old", Status: state.StatusDone, EndedAt: ptime(now.Add(-31 * 24 * time.Hour))},
		{ID: "1010-120000-a", Status: state.StatusDone, EndedAt: ptime(now.Add(-time.Hour))},
		{ID: "1010-120000-ab", Status: state.StatusRunning},
	}}
	st.Prune(now)
	if len(st.Jobs) != 2 {
		t.Fatalf("应只删掉 30 天前结束的：%d", len(st.Jobs))
	}
	if j, err := st.Find("1010-120000-a"); err != nil || j.ID != "1010-120000-a" {
		t.Errorf("完整 id 应精确匹配：%v", err)
	}
	if _, err := st.Find("1010-12"); err == nil {
		t.Error("有歧义的前缀应报错")
	}
	if j, err := st.Find("1010-120000-ab"); err != nil || j.Status != state.StatusRunning {
		t.Errorf("%v", err)
	}
}

// 状态文件内容坏了：tick 备份坏文件、按 meta.json 重建（读不出的 meta.json 跳过并报出来），
// 用量冷却按撞上限任务的结束时间估回来；那次不启动任务。
func TestTickRecoversCorruptState(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	testkit.MkProject(t, root, "alpha", "- [ ] 一条活 —— x\n")
	var out bytes.Buffer
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	a := wireApp(&App{Env: &env.Env{Home: home, Now: func() time.Time { return now }, Out: &out}})
	os.MkdirAll(a.DataDir(), 0o755)
	a.Cfg = config.Default()
	a.Cfg.ProjectsRoot = root
	a.Cfg.ClaudePath = "/nonexistent/claude"
	ended := now.Add(-20 * time.Minute)
	for _, j := range []*Job{
		{ID: "1010-114000-alpha", Project: "alpha", Status: state.StatusFailed, StartedAt: ended.Add(-time.Minute), EndedAt: &ended, LimitHit: true},
		{ID: "1010-100000-alpha", Project: "alpha", Status: state.StatusDone, StartedAt: now.Add(-2 * time.Hour), EndedAt: &ended},
	} {
		b, _ := json.Marshal(j)
		os.MkdirAll(a.JobDir(j.ID), 0o755)
		os.WriteFile(filepath.Join(a.JobDir(j.ID), "meta.json"), b, 0o644)
	}
	os.MkdirAll(a.JobDir("1010-110000-alpha"), 0o755)
	os.WriteFile(filepath.Join(a.JobDir("1010-110000-alpha"), "meta.json"), []byte("{"), 0o644)
	bad := []byte(`{"jobs": [ {"id": `)
	os.WriteFile(a.StatePath(), bad, 0o644)

	if err := a.Sched.Tick(""); err != nil {
		t.Fatalf("内容坏了应重建而不是报错：%v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(a.StatePath() + ".bad-20261010-120000"); !bytes.Equal(b, bad) {
		t.Errorf("应把坏文件原样备份：%q", b)
	}
	st, err := state.Load(a.StatePath())
	if err != nil {
		t.Fatalf("重建后的状态应已保存：%v", err)
	}
	if len(st.Jobs) != 2 || st.Jobs[0].ID != "1010-100000-alpha" || st.Jobs[1].ID != "1010-114000-alpha" {
		t.Errorf("应按启动时间重建 2 个任务：%+v", st.Jobs)
	}
	if want := ended.Add(a.Cfg.LimitCooldown.Duration); !st.CooldownOf("").Equal(want) {
		t.Errorf("用量冷却应估到 %v，实际 %v", want, st.CooldownOf(""))
	}
	n, _ := os.ReadFile(a.NightlyPath(now))
	if !strings.Contains(string(n), "状态文件损坏：") || !strings.Contains(string(n), "1 个任务的 meta.json 读不出，没算进去：1010-110000-alpha") {
		t.Errorf("当天汇总应报警：\n%s", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(a.DataDir(), "jobs")); len(entries) != 3 {
		t.Errorf("不应创建任务：%v", entries)
	}
}

// 状态文件读不了（不是内容坏了，比如没权限）：仍当作未知，不收尾不启动、不备份不覆盖，以非零状态退出。
func TestTickRefusesOnUnreadableState(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	home := t.TempDir()
	root := t.TempDir()
	testkit.MkProject(t, root, "alpha", "- [ ] 一条活 —— x\n")
	var out bytes.Buffer
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: &out}})
	os.MkdirAll(a.DataDir(), 0o755)
	a.Cfg = config.Default()
	a.Cfg.ProjectsRoot = root
	a.Cfg.ClaudePath = "/nonexistent/claude"
	os.WriteFile(a.StatePath(), []byte(`{"jobs":[]}`), 0o644)
	os.Chmod(a.StatePath(), 0o000)
	defer os.Chmod(a.StatePath(), 0o644)
	if err := a.Sched.Tick(""); err == nil {
		t.Error("状态文件读不了 tick 应返回错误")
	}
	if !strings.Contains(out.String(), "当作未知") {
		t.Errorf("应记日志说明：%s", out.String())
	}
	if baks, _ := filepath.Glob(a.StatePath() + ".bad-*"); len(baks) != 0 {
		t.Errorf("读不了不是损坏，不应备份：%v", baks)
	}
	if entries, _ := os.ReadDir(filepath.Join(a.DataDir(), "jobs")); len(entries) != 0 {
		t.Errorf("不应创建任务：%v", entries)
	}
}

func TestScanProjects(t *testing.T) {
	root := t.TempDir()
	testkit.MkProject(t, root, "b", "")
	testkit.MkProject(t, root, "a", "")
	testkit.MkProject(t, root, ".hidden", "")
	os.MkdirAll(filepath.Join(root, "notgit"), 0o755)
	wt := filepath.Join(root, "worktree")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x\n"), 0o644)
	os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644)
	ps, err := backlog.ScanProjects(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("得到 %v", names)
	}
}

func TestPrivateConfigAndRuntimePermissions(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".agentq")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(home, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory permission: %v %v", fi, err)
	}
	if err := config.Save(a.ConfigPath(), a.Cfg); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(a.ConfigPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config permission: %v %v", fi, err)
	}
}
