package release

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/evolve"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// 全部用临时 HOME、临时 projects_root、真 git、假 gh、假 .agentq/release。

// 假 gh：记下参数；release list 回 $HOME/gh-release.json，api 回 $HOME/gh-checks.json，issue 回 $HOME/gh-issue.json；有 $HOME/gh-fail 就报错。
const fakeGH = `#!/bin/sh
echo "$*" >> "$HOME/gh-calls"
[ ! -f "$HOME/gh-fail" ] || { echo 'HTTP 502' >&2; exit 1; }
case "$1" in
  release) cat "$HOME/gh-release.json" 2>/dev/null || echo '[]' ;;
  api) cat "$HOME/gh-checks.json" ;;
  issue) cat "$HOME/gh-issue.json" ;;
esac
`

// 假发版脚本：记下参数；--check 时有 $HOME/check-fail 就报说明书的问题，真发时有 $HOME/release-fail 就失败。
const fakeScript = `#!/bin/sh
echo "$* repo=$AGENTQ_RELEASE_REPO" >> "$HOME/release-calls"
if [ "$1" = --check ]; then
  [ ! -f "$HOME/check-fail" ] || { echo 'README.md:31 写了 agentq run --fast，agentq run --help 里没有'; exit 1; }
  exit 0
fi
[ ! -f "$HOME/release-fail" ] || { echo 'gh release download：没有 SHA256SUMS'; exit 1; }
echo "发了 $1"
`

const green = `{"total_count":2,"check_runs":[{"name":"linux","status":"completed","conclusion":"success"},{"name":"stress","status":"completed","conclusion":"skipped"}]}`

type rig struct {
	t       *testing.T
	s       *Service
	now     time.Time
	home    string
	dir     string
	spawned []string
}

func newRig(t *testing.T, edit func(c *config.Config)) *rig {
	t.Helper()
	home, root, bin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, now: time.Date(2026, 11, 30, 10, 0, 0, 0, time.Local), home: home}
	cfg := config.Default()
	cfg.ProjectsRoot = root
	cfg.ReleaseRepos = map[string]string{"demo": "o/demo"}
	if edit != nil {
		edit(&cfg)
	}
	e := &env.Env{Home: home, Cfg: cfg, Now: func() time.Time { return r.now }, Out: &bytes.Buffer{}}
	k := stack.New(e)
	r.s = &Service{Env: e, Backlog: k.Backlog, Gitx: k.Gitx, Notify: k.Notify, Evolve: &evolve.Service{Env: e, Backlog: k.Backlog, Notify: k.Notify}}
	r.s.spawn = func(name, version string) error { r.spawned = append(r.spawned, name+" "+version); return nil }
	r.dir = r.project("demo", "- 自动发版：条件满足时由 agentq 调 `.agentq/release` 发新版本\n")
	r.file("gh-release.json", `[{"tagName":"v0.3.0","publishedAt":"2026-11-13T11:45:34Z"}]`)
	r.file("gh-checks.json", green)
	return r
}

// project 建一个有 AGENTS.md、CHANGELOG（「未发布」里一条新功能）、BACKLOG、假发版脚本的仓库。
func (r *rig) project(name, rule string) string {
	r.t.Helper()
	dir := testkit.MkProject(r.t, r.s.Cfg.ProjectsRoot, name, "")
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# 项目\n\n## 发版\n"+rule), 0o644)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# 更新记录\n\n## 未发布\n\n- 新功能甲\n  续行\n- 修复：乙\n\n## v0.3.0\n\n- 旧的\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
	os.WriteFile(filepath.Join(dir, ".agentq", "release"), []byte(fakeScript), 0o755)
	testkit.GitT(r.t, r.home, dir, "add", "-A")
	testkit.GitT(r.t, r.home, dir, "commit", "-q", "-m", "init")
	return dir
}

func (r *rig) file(name, text string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.home, name), []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (r *rig) commit(rel, text string) {
	r.t.Helper()
	os.WriteFile(filepath.Join(r.dir, rel), []byte(text), 0o644)
	testkit.GitT(r.t, r.home, r.dir, "add", "-A")
	testkit.GitT(r.t, r.home, r.dir, "commit", "-q", "-m", "改 "+rel)
}

func (r *rig) record() Record {
	r.t.Helper()
	rec, err := r.s.load("demo")
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}

func (r *rig) notices() string { return r.read(filepath.Join(r.home, ".agentq", "notify.json")) }

// tick：过一小时再判一次（联网查公开仓一小时最多一次）。
func (r *rig) tick(st *state.State) {
	r.now = r.now.Add(ghEvery + time.Minute)
	r.s.StartIfDue(st)
}

// 条件差一条就不发，原因记进发版记录、变了才记日志；不联网的条件不满足时不调 gh。
func TestNotDue(t *testing.T) {
	cases := []struct {
		name, why string
		ghCalled  bool
		setup     func(r *rig, st *state.State)
	}{
		{"没写规则", "没有「- 自动发版：」", false, func(r *rig, _ *state.State) { r.commit("AGENTS.md", "# 项目\n\n正文提到自动发版：不算\n") }},
		{"没有脚本", "没有能执行的 .agentq/release", false, func(r *rig, _ *state.State) { os.Chmod(scriptPath(r.dir), 0o644) }},
		{"未发布是空的", "「## 未发布」里没有条目", false, func(r *rig, _ *state.State) {
			r.commit("CHANGELOG.md", "# 更新记录\n\n## 未发布\n\n## v0.3.0\n\n- 旧的\n")
		}},
		{"主目录不在默认分支", "", false, func(r *rig, _ *state.State) { testkit.GitT(r.t, r.home, r.dir, "checkout", "-q", "-b", "side") }},
		{"公开仓上次发布不到 7 天", "不到 7 天", true, func(r *rig, _ *state.State) {
			r.file("gh-release.json", `[{"tagName":"v0.3.0","publishedAt":"2026-11-27T11:45:34Z"}]`)
		}},
		{"CI 没过", "CI 没过（linux：failure）", true, func(r *rig, _ *state.State) {
			r.file("gh-checks.json", `{"total_count":1,"check_runs":[{"name":"linux","status":"completed","conclusion":"failure"}]}`)
		}},
		{"CI 还在跑", "CI 还在跑", true, func(r *rig, _ *state.State) {
			r.file("gh-checks.json", `{"total_count":1,"check_runs":[{"name":"linux","status":"in_progress","conclusion":""}]}`)
		}},
		{"还没有 CI 结果", "还没有 CI 结果", true, func(r *rig, _ *state.State) { r.file("gh-checks.json", `{"total_count":0,"check_runs":[]}`) }},
		{"gh 查不了", "查不了公开仓 o/demo 的发布", true, func(r *rig, _ *state.State) { r.file("gh-fail", "") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			st := &state.State{}
			c.setup(r, st)
			r.tick(st)
			if len(r.spawned) != 0 {
				t.Fatalf("不该发：%v", r.spawned)
			}
			rec := r.record()
			if rec.Note == "" || !strings.Contains(rec.Note, c.why) {
				t.Errorf("原因：%q，要有 %q", rec.Note, c.why)
			}
			if got := r.read(filepath.Join(r.home, "gh-calls")) != ""; got != c.ghCalled {
				t.Errorf("调没调 gh：%v，要 %v", got, c.ghCalled)
			}
			if logs := r.s.Out.(*bytes.Buffer).String(); strings.Count(logs, "这次不发") != 1 {
				t.Errorf("记一行日志：\n%s", logs)
			}
			r.tick(st)
			if logs := r.s.Out.(*bytes.Buffer).String(); strings.Count(logs, "这次不发") != 1 || len(r.spawned) != 0 {
				t.Errorf("原因没变不再记日志：%v\n%s", r.spawned, logs)
			}
			if len(r.read(filepath.Join(r.home, "release-calls"))) != 0 {
				t.Error("不发时不调脚本")
			}
		})
	}
}

// 没登记在 release_repos 的项目，规则写了也不发，也不留记录（别人 clone 了带规则的仓库不会替作者发版）。
func TestNotRegistered(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.ReleaseRepos = nil })
	r.tick(&state.State{})
	if len(r.spawned) != 0 || r.read(filepath.Join(r.home, "gh-calls")) != "" {
		t.Fatalf("没登记不发也不联网：%v", r.spawned)
	}
	if _, err := os.Stat(r.s.dir()); err == nil {
		t.Error("没登记不留记录")
	}
}

// 条件都满足：起一次后台进程，版本按「未发布」升小版本号；发版中不再起第二个，tick 不接新活。
func TestStartsOnce(t *testing.T) {
	r := newRig(t, nil)
	st := &state.State{}
	r.tick(st)
	if !slices.Equal(r.spawned, []string{"demo v0.4.0"}) {
		t.Fatalf("要起一次：%v", r.spawned)
	}
	if rec := r.record(); rec.State != stRunning || rec.Version != "v0.4.0" {
		t.Fatalf("记成发版中：%+v", rec)
	}
	if b := r.s.Busy(); !strings.Contains(b, "正在自动发版 demo v0.4.0") {
		t.Errorf("发版中 tick 不接新活：%q", b)
	}
	r.now = r.now.Add(10 * time.Second)
	r.s.StartIfDue(st)
	if len(r.spawned) != 1 {
		t.Fatalf("发版中不再起：%v", r.spawned)
	}
}

// 后台进程：先说明书检查，过了把 CHANGELOG 记成这个版本并提交，再真发；发成推一条，之后 7 天内不再发。
func TestRunReleases(t *testing.T) {
	r := newRig(t, nil)
	st := &state.State{}
	r.tick(st)
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err != nil {
		t.Fatal(err)
	}
	if calls := r.read(filepath.Join(r.home, "release-calls")); calls != "--check v0.4.0 repo=o/demo\nv0.4.0 repo=o/demo\n" {
		t.Fatalf("脚本要各调一次：\n%s", calls)
	}
	cl := r.read(filepath.Join(r.dir, "CHANGELOG.md"))
	if !strings.Contains(cl, "## 未发布\n\n## v0.4.0\n\n- 新功能甲") || len(Unreleased(cl)) != 0 {
		t.Errorf("CHANGELOG 要记成 v0.4.0：\n%s", cl)
	}
	if out := testkit.GitT(t, r.home, r.dir, "status", "--porcelain"); out != "" {
		t.Errorf("CHANGELOG 要提交：%s", out)
	}
	if rec := r.record(); rec.State != stDone || !rec.Released.Equal(r.now) {
		t.Errorf("记成发成：%+v", rec)
	}
	testkit.AssertContains(t, r.notices(), `"cat": "release"`, "发布了 demo v0.4.0", "https://github.com/o/demo/releases/tag/v0.4.0", "· 新功能甲 续行", "· 修复：乙")
	if logs := r.s.Out.(*bytes.Buffer).String(); !strings.Contains(logs, "发了 v0.4.0") {
		t.Errorf("脚本输出进日志：\n%s", logs)
	}

	// 「未发布」又有了条目，公开仓那边还看不到这次发布：按本机记录，7 天内不重复
	r.commit("CHANGELOG.md", strings.Replace(cl, "## 未发布\n", "## 未发布\n\n- 修复：丙\n", 1))
	r.now = r.now.Add(3 * 24 * time.Hour)
	r.tick(st)
	if len(r.spawned) != 1 || !strings.Contains(r.record().Note, "离上次发成") {
		t.Fatalf("7 天内不重复：%v %q", r.spawned, r.record().Note)
	}
	r.now = r.now.Add(5 * 24 * time.Hour)
	r.file("gh-release.json", `[{"tagName":"v0.4.0","publishedAt":"2026-11-30T12:00:00Z"}]`)
	r.tick(st)
	if !slices.Equal(r.spawned, []string{"demo v0.4.0", "demo v0.4.1"}) {
		t.Fatalf("满 7 天、只有修复：升补丁号：%v", r.spawned)
	}
}

// 说明书检查不过：不真发、不改 CHANGELOG，往「待做」写一条先修并推一条；那一条还在「待做」里就不再试，做完移走、有新提交了再试。
func TestRunDocsCheckFails(t *testing.T) {
	r := newRig(t, nil)
	st := &state.State{}
	r.file("check-fail", "")
	r.tick(st)
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err == nil {
		t.Fatal("说明书检查不过要报错退出")
	}
	if calls := r.read(filepath.Join(r.home, "release-calls")); calls != "--check v0.4.0 repo=o/demo\n" {
		t.Fatalf("只跑检查、不真发：\n%s", calls)
	}
	if cl := r.read(filepath.Join(r.dir, "CHANGELOG.md")); strings.Contains(cl, "## v0.4.0") {
		t.Errorf("没发不改 CHANGELOG：\n%s", cl)
	}
	bl := r.read(filepath.Join(r.dir, "docs", "BACKLOG.md"))
	testkit.AssertContains(t, bl, "- [ ] 发版前说明书检查没过（v0.4.0） —— ", "README.md:31 写了 agentq run --fast", "验收：`.agentq/release --check v0.4.0` 退出码 0")
	if out := testkit.GitT(t, r.home, r.dir, "status", "--porcelain"); out != "" {
		t.Errorf("待做要提交：%s", out)
	}
	testkit.AssertContains(t, r.notices(), "前说明书检查没过，这次没发", "写进了「待做」")
	if rec := r.record(); rec.State != stDocs || slices.Contains(rec.Failed, "v0.4.0") {
		t.Errorf("记成说明书没过，不算发失败：%+v", rec)
	}

	r.now = r.now.Add(time.Minute) // 刚发完马上又 tick：不能沿用起进程前那次联网的结果
	r.s.StartIfDue(st)
	r.tick(st)
	if len(r.spawned) != 1 || !strings.Contains(r.record().Note, "说明书检查没过") {
		t.Fatalf("待做还在不再试：%v %q", r.spawned, r.record().Note)
	}
	// 修好合并：那一条移出「待做」
	r.commit("docs/BACKLOG.md", strings.Replace(bl, "- [ ] 发版前说明书检查没过", "- [x] 发版前说明书检查没过", 1))
	os.Remove(filepath.Join(r.home, "check-fail"))
	r.tick(st)
	if len(r.spawned) != 2 {
		t.Fatalf("修好了再试：%v %q", r.spawned, r.record().Note)
	}
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(r.read(filepath.Join(r.dir, "docs", "BACKLOG.md")), "说明书检查没过（v0.4.0）") != 1 {
		t.Error("同一个版本只写一条待做")
	}
}

// 脚本失败：推一条、记下这个版本，以后不再试它；后台进程中途没了也一样。
func TestRunFailsNoRetry(t *testing.T) {
	r := newRig(t, nil)
	st := &state.State{}
	r.file("release-fail", "")
	r.tick(st)
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err == nil {
		t.Fatal("失败要报错退出")
	}
	rec := r.record()
	if rec.State != stFailed || !slices.Equal(rec.Failed, []string{"v0.4.0"}) || !strings.Contains(rec.Why, "没有 SHA256SUMS") {
		t.Fatalf("记成失败：%+v", rec)
	}
	testkit.AssertContains(t, r.notices(), `"cat": "alert"`, "自动发版 demo v0.4.0 没成，停在原地", "没有 SHA256SUMS")

	// CHANGELOG 已经记成 v0.4.0；又有了新条目、过了 7 天，算出来还是 v0.4.0：不重试
	cl := r.read(filepath.Join(r.dir, "CHANGELOG.md"))
	r.commit("CHANGELOG.md", strings.Replace(cl, "## 未发布\n", "## 未发布\n\n- 新功能丁\n", 1))
	os.Remove(filepath.Join(r.home, "release-fail"))
	r.now = r.now.Add(8 * 24 * time.Hour)
	r.tick(st)
	if len(r.spawned) != 1 || !strings.Contains(r.record().Note, "上次发 v0.4.0 没成，停在原地") {
		t.Fatalf("同一个版本不重试：%v %q", r.spawned, r.record().Note)
	}
	if n := strings.Count(r.read(filepath.Join(r.home, "release-calls")), "\n"); n != 2 {
		t.Errorf("脚本只跑过那一次（检查加真发）：%d", n)
	}
}

// 记着「发版中」、后台进程却没了（关机、被杀）：当失败，推一条，不重试。
func TestInterruptedCountsAsFailed(t *testing.T) {
	r := newRig(t, nil)
	st := &state.State{}
	r.tick(st)
	r.now = r.now.Add(2 * startGrace)
	if r.s.Busy() != "" {
		t.Fatal("没人拿锁、过了空档就不算在发")
	}
	r.tick(st)
	rec := r.record()
	if rec.State != stFailed || !slices.Contains(rec.Failed, "v0.4.0") || len(r.spawned) != 1 {
		t.Fatalf("中途没了当失败、不重试：%+v %v", rec, r.spawned)
	}
	testkit.AssertContains(t, r.notices(), "发版进程中途没了")
}

// self_project：本机这一代观察期过了才发；默认分支上有比它新的代码不发，只多了文档照发。发成后往官网项目写「对齐新版本」。
func TestSelfProjectObservedAndSite(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.SelfProject, c.SiteProject = "demo", "site" })
	site := r.project("site", "")
	st := &state.State{}
	head := testkit.GitT(t, r.home, r.dir, "rev-parse", "HEAD")
	r.s.Evolve.Version = "dev-" + head[:7]
	r.tick(st)
	if len(r.spawned) != 0 || !strings.Contains(r.record().Note, "还没过观察期") {
		t.Fatalf("没过观察期不发：%v %q", r.spawned, r.record().Note)
	}
	evs := `{"at":"2026-11-29T10:00:00+08:00","kind":"upgrade","to":"` + r.s.Evolve.Version + `"}` + "\n" +
		`{"at":"2026-11-29T20:00:00+08:00","kind":"kept","to":"` + r.s.Evolve.Version + `"}` + "\n"
	os.MkdirAll(filepath.Join(r.home, ".agentq", "evolve"), 0o700)
	os.WriteFile(filepath.Join(r.home, ".agentq", "evolve", "upgrades.jsonl"), []byte(evs), 0o600)
	r.commit("main.go", "package main\n")
	r.tick(st)
	if len(r.spawned) != 0 || !strings.Contains(r.record().Note, "多改了 1 个文件") {
		t.Fatalf("有没观察过的代码不发：%v %q", r.spawned, r.record().Note)
	}
	testkit.GitT(t, r.home, r.dir, "rm", "-q", "main.go")
	testkit.GitT(t, r.home, r.dir, "commit", "-q", "-m", "撤")
	r.commit("README.md", "说明\n")
	r.tick(st)
	if !slices.Equal(r.spawned, []string{"demo v0.4.0"}) {
		t.Fatalf("只多了文档照发：%v %q", r.spawned, r.record().Note)
	}
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, r.read(filepath.Join(site, "docs", "BACKLOG.md")), "- [ ] 对齐 demo 新版本 v0.4.0 —— ", "https://github.com/o/demo/releases/tag/v0.4.0", "新功能甲")
}

// 没配官网项目、或发的不是 self_project：不写「对齐新版本」。
func TestNoSiteTodoForOthers(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.SiteProject = "site" })
	site := r.project("site", "")
	r.tick(&state.State{})
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.read(filepath.Join(site, "docs", "BACKLOG.md")), "对齐") {
		t.Error("不是 self_project 不写")
	}
}

func TestChangelog(t *testing.T) {
	text := "# 更新记录\n\n## 未发布\n\n- 甲\n  续\n- 修复：乙\n\n## v0.3.0\n\n- 旧\n"
	if got := Unreleased(text); !slices.Equal(got, []string{"甲 续", "修复：乙"}) {
		t.Fatalf("%q", got)
	}
	if got := Unreleased("## Unreleased\n\n- Fix: a\n"); !slices.Equal(got, []string{"Fix: a"}) {
		t.Fatalf("英文标题：%q", got)
	}
	if Unreleased("# 更新记录\n\n## v0.3.0\n\n- 旧\n") != nil || Unreleased("## 未发布\n\n## v0.3.0\n- 旧\n") != nil {
		t.Error("没有这一节、这一节是空的：没有条目")
	}
	for _, c := range []struct {
		last    string
		entries []string
		want    string
	}{
		{"v0.3.0", []string{"甲", "修复：乙"}, "v0.4.0"},
		{"v0.3.0", []string{"修复：乙", "Fix: c", "fix：d"}, "v0.3.1"},
		{"v1.9.4", []string{"甲"}, "v1.10.0"},
		{"", []string{"修复：乙"}, "v0.1.0"},
	} {
		if got, err := NextVersion(c.last, c.entries); err != nil || got != c.want {
			t.Errorf("%s %v：%s %v，要 %s", c.last, c.entries, got, err, c.want)
		}
	}
	if _, err := NextVersion("release-3", []string{"甲"}); err == nil {
		t.Error("认不出的 tag 要报错")
	}
	if _, err := NextVersion("v0.3.0", nil); err == nil {
		t.Error("没有条目要报错")
	}
	got, err := Stamp(text, "v0.4.0")
	if err != nil || got != "# 更新记录\n\n## 未发布\n\n## v0.4.0\n\n- 甲\n  续\n- 修复：乙\n\n## v0.3.0\n\n- 旧\n" {
		t.Fatalf("%v\n%s", err, got)
	}
	if _, err := Stamp(got, "v0.5.0"); err == nil {
		t.Error("「未发布」是空的不能记")
	}
}

// 规则只认「- 自动发版：」开头的那一行，正文顺带提到不算。
func TestRuleOn(t *testing.T) {
	dir := t.TempDir()
	for text, want := range map[string]bool{
		"## 发版\n- 自动发版：条件满足时发\n":       true,
		"* Auto release: when ready\n": true,
		"我们以后想做自动发版：还没定\n":             false,
		"- 不自动发版：等用户点头\n":              false,
		"- 发版：自动发版的事以后再说\n":            false,
		"- 自动发版这事：先不做\n":               false,
		"  - 自动发版: 写在 CLAUDE.md 也算\n":  true,
	} {
		os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(text), 0o644)
		if got := ruleOn(dir); got != want {
			t.Errorf("%q：%v，要 %v", text, got, want)
		}
	}
}

func (r *rig) ghCalls() int { return strings.Count(r.read(filepath.Join(r.home, "gh-calls")), "\n") }

// 别的条件都满足、只差有任务在跑或在过门：等发版（tick 不接新活、不升级），一小时内不再联网；在跑的做完，下一次就发。
func TestWaitsForActiveJobs(t *testing.T) {
	cases := []struct {
		name string
		job  *state.Job
		done func(j *state.Job)
	}{
		{"在跑", &state.Job{ID: "j", Project: "other", Status: state.StatusRunning}, func(j *state.Job) { j.Status = state.StatusDone }},
		{"在过门", &state.Job{ID: "j", Project: "demo", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateQueued}},
			func(j *state.Job) { j.Gate.State = state.GateMerged }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			st := &state.State{Jobs: []*state.Job{c.job}}
			r.now = r.now.Add(ghEvery + time.Minute)
			note := r.s.StartIfDue(st)
			if len(r.spawned) != 0 || !strings.Contains(note, "等发版：停接新活，等 1 个任务做完") || !strings.Contains(note, "demo v0.4.0") {
				t.Fatalf("要等发版：%q %v", note, r.spawned)
			}
			if rec := r.record(); rec.Wait != "v0.4.0" || rec.Tag != "v0.3.0" || !r.s.Waiting() {
				t.Fatalf("记下在等：%+v", rec)
			}
			if l := r.s.StatusLine(st); !strings.Contains(l, "等发版：停接新活，等 1 个任务做完") {
				t.Errorf("status 那一行：%q", l)
			}
			calls := r.ghCalls()
			r.now = r.now.Add(5 * time.Minute)
			if note := r.s.StartIfDue(st); !strings.Contains(note, "等发版") || len(r.spawned) != 0 || r.ghCalls() != calls {
				t.Fatalf("一小时内照旧等、不再联网：%q %v %d→%d", note, r.spawned, calls, r.ghCalls())
			}
			if logs := r.s.Out.(*bytes.Buffer).String(); strings.Count(logs, "等发版") != 1 || strings.Contains(logs, "这次不发") {
				t.Errorf("进入等发版记一行日志：\n%s", logs)
			}
			c.done(c.job)
			r.now = r.now.Add(5 * time.Minute)
			note = r.s.StartIfDue(st)
			if !slices.Equal(r.spawned, []string{"demo v0.4.0"}) || !strings.Contains(note, "正在自动发版 demo v0.4.0") {
				t.Fatalf("做完了就发：%q %v", note, r.spawned)
			}
			if rec := r.record(); rec.Wait != "" || rec.State != stRunning || r.s.Waiting() {
				t.Fatalf("开始发了就不再等：%+v", rec)
			}
			if l := r.s.StatusLine(st); !strings.HasPrefix(l, "发版：正在自动发版 demo v0.4.0") {
				t.Errorf("status 那一行：%q", l)
			}
		})
	}
}

// 有任务在跑、别的条件不满足：不进入等发版，tick 照常接活（StartIfDue 返回空），也不联网查。
func TestNoWaitWhenNotDue(t *testing.T) {
	cases := []struct {
		name, why string
		edit      func(c *config.Config)
		setup     func(r *rig)
	}{
		{"CHANGELOG 没条目", "「## 未发布」里没有条目", nil, func(r *rig) { r.commit("CHANGELOG.md", "# 更新记录\n\n## 未发布\n\n## v0.3.0\n\n- 旧的\n") }},
		{"离上次发成不到 7 天", "不到 7 天", nil, func(r *rig) {
			if err := r.s.save("demo", Record{State: stDone, Version: "v0.3.0", Released: r.now.Add(-3 * 24 * time.Hour)}); err != nil {
				r.t.Fatal(err)
			}
		}},
		{"观察期没过", "还没过观察期", func(c *config.Config) { c.SelfProject = "demo" }, func(r *rig) {
			r.s.Evolve.Version = "dev-" + testkit.GitT(r.t, r.home, r.dir, "rev-parse", "--short=7", "HEAD")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, c.edit)
			c.setup(r)
			st := &state.State{Jobs: []*state.Job{{ID: "j", Project: "other", Status: state.StatusRunning}}}
			r.now = r.now.Add(ghEvery + time.Minute)
			if note := r.s.StartIfDue(st); note != "" || len(r.spawned) != 0 {
				t.Fatalf("不满足不等发版，照常接活：%q %v", note, r.spawned)
			}
			if rec := r.record(); rec.Wait != "" || !strings.Contains(rec.Note, c.why) || r.s.Waiting() || r.s.StatusLine(st) != "" {
				t.Fatalf("记的是不发的原因：%+v", rec)
			}
			if r.ghCalls() != 0 {
				t.Error("本机条件不满足不联网")
			}
		})
	}
}

// 等发版中途条件变了（CHANGELOG 被清空）：不再等，照常接活；「未发布」里条目变了，版本号按记下的公开仓版本现算，不再联网。
func TestWaitFollowsConditions(t *testing.T) {
	r := newRig(t, nil)
	r.commit("CHANGELOG.md", "# 更新记录\n\n## 未发布\n\n- 修复：乙\n\n## v0.3.0\n\n- 旧的\n")
	st := &state.State{Jobs: []*state.Job{{ID: "j", Project: "other", Status: state.StatusRunning}}}
	r.now = r.now.Add(ghEvery + time.Minute)
	if note := r.s.StartIfDue(st); !strings.Contains(note, "demo v0.3.1") {
		t.Fatalf("只有修复等的是补丁版本：%q", note)
	}
	calls := r.ghCalls()
	r.commit("CHANGELOG.md", "# 更新记录\n\n## 未发布\n\n- 新功能甲\n- 修复：乙\n\n## v0.3.0\n\n- 旧的\n")
	r.now = r.now.Add(5 * time.Minute)
	if note := r.s.StartIfDue(st); !strings.Contains(note, "demo v0.4.0") || r.ghCalls() != calls {
		t.Fatalf("加了新功能等的是小版本，不再联网：%q %d→%d", note, calls, r.ghCalls())
	}
	r.commit("CHANGELOG.md", "# 更新记录\n\n## 未发布\n\n## v0.3.0\n\n- 旧的\n")
	r.now = r.now.Add(5 * time.Minute)
	if note := r.s.StartIfDue(st); note != "" || r.s.Waiting() || r.record().Wait != "" {
		t.Fatalf("条件不满足了不再等：%q %+v", note, r.record())
	}
}

// 发版做完或失败后不再挡着：StartIfDue 返回空，tick 照常接活。
func TestResumeAfterRelease(t *testing.T) {
	for _, fail := range []bool{false, true} {
		r := newRig(t, nil)
		st := &state.State{}
		r.tick(st)
		if fail {
			r.file("release-fail", "")
		}
		err := r.s.Run([]string{"demo", "v0.4.0"})
		if (err != nil) != fail {
			t.Fatalf("发版结果：%v", err)
		}
		r.now = r.now.Add(5 * time.Minute)
		st.Jobs = append(st.Jobs, &state.Job{ID: "j", Project: "other", Status: state.StatusRunning})
		if note := r.s.StartIfDue(st); note != "" || r.s.Busy() != "" || r.s.Waiting() || len(r.spawned) != 1 {
			t.Fatalf("失败=%v 之后要恢复接活：%q %v", fail, note, r.spawned)
		}
	}
}
