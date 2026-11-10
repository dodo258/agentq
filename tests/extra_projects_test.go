package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	bl "agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/gate"
	"agentq/internal/onboard"
	"agentq/internal/testkit"
)

// outsideRepo 在 projects_root 外建一个配好身份的仓库。
func outsideRepo(t *testing.T, a *App, name string) string {
	t.Helper()
	dir := filepath.Join(a.Home, "code", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	onboardGit(t, a.Home, real, "init", "-q", "-b", "main")
	onboardGit(t, a.Home, real, "config", "user.name", "t")
	onboardGit(t, a.Home, real, "config", "user.email", "t")
	testkit.WriteInstall(t, filepath.Join(real, "README.md"), "x\n", 0o644)
	onboardGit(t, a.Home, real, "add", "README.md")
	onboardGit(t, a.Home, real, "commit", "-q", "-m", "first")
	return real
}

func extraInFile(t *testing.T, a *App) []string {
	t.Helper()
	var c struct {
		Extra []string `json:"extra_projects"`
	}
	if err := json.Unmarshal(testkit.ReadBytes(t, a.ConfigPath()), &c); err != nil {
		t.Fatal(err)
	}
	return c.Extra
}

// 第一次 add：没有 BACKLOG 就接入，项目在外面就登记，打一行说明；第二次不再登记、不再接入。
func TestAddOutsideRootRegistersAndOnboards(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := outsideRepo(t, a, "far-app")
	t.Chdir(dir)
	if err := a.Onboard.CmdAdd([]string{"第一条"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "已登记到 extra_projects", "已接入 far-app", "已加到 far-app")
	if got := extraInFile(t, a); !slices.Equal(got, []string{dir}) {
		t.Fatalf("%v", got)
	}
	items, err := (bl.Project{Name: "far-app", Dir: dir}).Items()
	if err != nil || len(items) != 1 || items[0].Title != "第一条" {
		t.Fatalf("%+v %v", items, err)
	}
	if !exists(filepath.Join(dir, "AGENTS.md")) {
		t.Fatal("没接入")
	}
	projects, err := a.Backlog.ActiveProjects()
	if err != nil || len(projects) != 1 || projects[0].Dir != dir {
		t.Fatalf("%+v %v", projects, err)
	}
	// 重新读配置，第二次在子目录里
	a2, err := newApp(a.Home, out)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a2.Cfg.ExtraProjects, []string{dir}) {
		t.Fatalf("%v", a2.Cfg.ExtraProjects)
	}
	sub := filepath.Join(dir, "docs")
	t.Chdir(sub)
	out.Reset()
	if err := a2.Onboard.CmdAdd([]string{"第二条"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Contains(s, "已登记") || strings.Contains(s, "已接入") {
		t.Fatalf("第二次不该再登记、再接入：\n%s", s)
	}
	if got := extraInFile(t, a); !slices.Equal(got, []string{dir}) {
		t.Fatalf("%v", got)
	}
	// 点名也认得：--project
	t.Chdir(a.Home)
	if err := a2.Onboard.CmdAdd([]string{"第三条", "--project", "far-app"}); err != nil {
		t.Fatal(err)
	}
	// init 对登记过的不再说不在 projects_root 下
	if err := a2.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
}

// 外面仓库的 worktree 里 add：认到主目录。
func TestAddFromWorktreeOfOutsideRepo(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	dir := outsideRepo(t, a, "far-app")
	wt := filepath.Join(a.Home, "wt-far")
	onboardGit(t, a.Home, dir, "worktree", "add", "-q", "-b", "feature", wt)
	realWt, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(realWt)
	if err := a.Onboard.CmdAdd([]string{"从 worktree 加"}); err != nil {
		t.Fatal(err)
	}
	if got := extraInFile(t, a); !slices.Equal(got, []string{dir}) {
		t.Fatalf("%v", got)
	}
}

// projects_root 下更深一层的仓库：一层目录不是仓库，往下的才是，也算项目。
func TestAddNestedRepoUnderRoot(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	root, err := filepath.EvalSymlinks(a.ProjectsRoot())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "group", "inner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	onboardGit(t, a.Home, dir, "init", "-q", "-b", "main")
	onboardGit(t, a.Home, dir, "config", "user.name", "t")
	onboardGit(t, a.Home, dir, "config", "user.email", "t")
	t.Chdir(dir)
	if err := a.Onboard.CmdAdd([]string{"里面的活"}); err != nil {
		t.Fatal(err)
	}
	if got := extraInFile(t, a); !slices.Equal(got, []string{dir}) {
		t.Fatalf("%v", got)
	}
}

// gitIdentity 给临时 HOME 配全局 git 身份，替用户建仓库的初始提交要用。
func gitIdentity(t *testing.T, home string) {
	t.Helper()
	testkit.WriteInstall(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = t\n\temail = t@t\n", 0o644)
}

// 不是 Git 仓库的目录：add 就地 git init，现有文件做初始提交（.gitignore 挡住密钥），再接入、登记、加条目，不问用户。
func TestAddInitsPlainDirectory(t *testing.T) {
	t.Run("projects_root 外有文件的目录", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		gitIdentity(t, a.Home)
		home, err := filepath.EvalSymlinks(a.Home)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, "code", "hello")
		testkit.WriteInstall(t, filepath.Join(dir, "main.py"), "print('hi')\n", 0o644)
		testkit.WriteInstall(t, filepath.Join(dir, ".env"), "SECRET=1\n", 0o600)
		t.Chdir(dir)
		if err := a.Onboard.CmdAdd([]string{"给这个项目加一个 hello 命令"}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, out.String(), "还不是 Git 仓库，已经 git init", "已接入 hello", "已登记到 extra_projects", "已加到 hello")
		files := onboardGit(t, a.Home, dir, "ls-files")
		testkit.AssertContains(t, files, "main.py", ".gitignore", "docs/BACKLOG.md")
		if strings.Contains(files, ".env") {
			t.Fatalf(".env 不该提交：\n%s", files)
		}
		if br := onboardGit(t, a.Home, dir, "symbolic-ref", "--short", "HEAD"); br != "main" {
			t.Fatalf("分支 %s", br)
		}
		if got := extraInFile(t, a); !slices.Equal(got, []string{dir}) {
			t.Fatalf("%v", got)
		}
		testkit.AssertContains(t, testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")), "- [ ] 给这个项目加一个 hello 命令")
	})
	t.Run("projects_root 下的空目录，自己有 .gitignore", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		gitIdentity(t, a.Home)
		root, err := filepath.EvalSymlinks(a.ProjectsRoot())
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "new-app")
		testkit.WriteInstall(t, filepath.Join(dir, ".gitignore"), "mine/\n", 0o644)
		t.Chdir(dir)
		if err := a.Onboard.CmdAdd([]string{"做个新东西"}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, filepath.Join(dir, ".gitignore"), "mine/\n")
		if len(extraInFile(t, a)) != 0 {
			t.Fatal("projects_root 下的不用登记")
		}
		testkit.AssertContains(t, testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")), "- [ ] 做个新东西")
	})
}

// 拒绝的情况：什么都不登记、不生成。
func TestAddOutsideRootRefuses(t *testing.T) {
	t.Run("家目录、projects_root 不是仓库", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		gitIdentity(t, a.Home)
		for _, dir := range []string{a.Home, a.ProjectsRoot()} {
			real, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(real)
			err = a.Onboard.CmdAdd([]string{"x"})
			if err == nil {
				t.Fatalf("%s 应拒绝", dir)
			}
			if exists(filepath.Join(real, ".git")) || exists(filepath.Join(real, "docs")) || len(extraInFile(t, a)) != 0 {
				t.Fatalf("%s 不该动任何东西", dir)
			}
		}
	})
	t.Run("坏的 .git", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		gitIdentity(t, a.Home)
		dir := filepath.Join(a.Home, "broken")
		testkit.WriteInstall(t, filepath.Join(dir, ".git", "junk"), "x", 0o644)
		t.Chdir(dir)
		err := a.Onboard.CmdAdd([]string{"x"})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "认不出这个仓库")
		testkit.AssertBytes(t, filepath.Join(dir, ".git", "junk"), "x")
	})
	t.Run("没配 git 身份", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		dir := filepath.Join(a.Home, "plain")
		testkit.WriteInstall(t, filepath.Join(dir, "main.py"), "print(1)\n", 0o644)
		testkit.WriteInstall(t, filepath.Join(a.Home, ".gitconfig"), "[user]\n\tuseConfigOnly = true\n", 0o644) // 不让 git 自己猜身份
		t.Chdir(dir)
		err := a.Onboard.CmdAdd([]string{"x"})
		if err == nil {
			t.Fatal("应失败")
		}
		testkit.AssertContains(t, err.Error(), "user.name", "已撤掉")
		if exists(filepath.Join(dir, ".git")) || exists(filepath.Join(dir, ".gitignore")) || len(extraInFile(t, a)) != 0 {
			t.Fatal("建的仓库要撤掉")
		}
		testkit.AssertBytes(t, filepath.Join(dir, "main.py"), "print(1)\n")
	})
	t.Run("家目录是仓库", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		home, err := filepath.EvalSymlinks(a.Home)
		if err != nil {
			t.Fatal(err)
		}
		onboardGit(t, a.Home, home, "init", "-q", "-b", "main")
		dir := filepath.Join(home, "plain")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		err = a.Onboard.CmdAdd([]string{"x"})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "家目录")
		if len(extraInFile(t, a)) != 0 || exists(filepath.Join(home, "docs")) {
			t.Fatal("不该把家目录当项目")
		}
	})
	t.Run("和 projects_root 下的项目重名", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		onboardRepo(t, a, "same", true)
		dir := outsideRepo(t, a, "same")
		t.Chdir(dir)
		err := a.Onboard.CmdAdd([]string{"x"})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "同名", "改个名字")
		if len(extraInFile(t, a)) != 0 || exists(filepath.Join(dir, "docs")) {
			t.Fatal("不该动")
		}
	})
	t.Run("和登记过的外面项目重名", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		first := outsideRepo(t, a, "same")
		t.Chdir(first)
		if err := a.Onboard.CmdAdd([]string{"x"}); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(a.Home, "elsewhere", "same")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		onboardGit(t, a.Home, other, "init", "-q", "-b", "main")
		real, err := filepath.EvalSymlinks(other)
		if err != nil {
			t.Fatal(err)
		}
		t.Chdir(real)
		if err := a.Onboard.CmdAdd([]string{"y"}); err == nil {
			t.Fatal("应拒绝")
		}
		if got := extraInFile(t, a); !slices.Equal(got, []string{first}) {
			t.Fatalf("%v", got)
		}
	})
	t.Run("认不出默认分支", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		dir := outsideRepo(t, a, "on-trunk")
		onboardGit(t, a.Home, dir, "branch", "-m", "main", "trunk")
		t.Chdir(dir)
		if err := a.Onboard.CmdAdd([]string{"x"}); err == nil {
			t.Fatal("认不出默认分支要拒绝")
		}
		if len(extraInFile(t, a)) != 0 || exists(filepath.Join(dir, "docs")) {
			t.Fatal("不该动")
		}
	})
}

// 第一次 add 在 projects_root 下：没有 BACKLOG 也接入，不登记。
func TestAddAutoInitUnderRoot(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := onboardRepo(t, a, "fresh-app", true)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
	if err := a.Onboard.CmdAdd([]string{"第一条"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "已接入 fresh-app", "已加到 fresh-app")
	if len(extraInFile(t, a)) != 0 {
		t.Fatal("projects_root 下的不用登记")
	}
	// --project 也一样
	other := onboardRepo(t, a, "fresh-two", true)
	if err := a.Onboard.CmdAdd([]string{"x", "--project", "fresh-two"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(other, "docs", "BACKLOG.md")) {
		t.Fatal("没接入")
	}
}

// exclude_projects 优先：登记了也不进自动流程，明确点名还能跑；status 列出不能用的。
func TestExtraProjectsExcludeWins(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := outsideRepo(t, a, "far-secret")
	a.Cfg.ExcludeProjects = []string{"far-secret"}
	t.Chdir(dir)
	if err := a.Onboard.CmdAdd([]string{"x"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "exclude_projects", "agentq run far-secret")
	if got := extraInFile(t, a); !slices.Equal(got, []string{dir}) {
		t.Fatalf("%v", got)
	}
	active, err := a.Backlog.ActiveProjects()
	if err != nil || len(active) != 0 {
		t.Fatalf("%+v %v", active, err)
	}
	all, err := a.Backlog.AllProjects()
	if err != nil || len(all) != 1 {
		t.Fatalf("%+v %v", all, err)
	}
	if got := a.ProjectDir("far-secret"); got != dir {
		t.Fatal(got)
	}
}

func TestExtraProjectsEdgeCases(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	onboardRepo(t, a, "same", true)
	far := outsideRepo(t, a, "far")
	gone := filepath.Join(a.Home, "gone", "away")
	plain := filepath.Join(a.Home, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	shadow := outsideRepo(t, a, "same")
	a.Cfg.ExtraProjects = []string{gone, plain, far, shadow}
	all, err := a.Backlog.AllProjects()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.Name+"="+p.Dir)
	}
	want := []string{"far=" + far, "same=" + filepath.Join(a.ProjectsRoot(), "same")}
	if !slices.Equal(names, want) {
		t.Fatalf("%v，想要 %v", names, want)
	}
	if got := a.Backlog.MissingExtras(); !slices.Equal(got, []string{gone, plain, shadow}) {
		t.Fatalf("%v", got)
	}
}

// 配置校验：extra_projects 要绝对路径。
func TestExtraProjectsValidate(t *testing.T) {
	for _, bad := range []string{"relative/x", "~/x", "/", "/a/../b", ""} {
		c := config.Default()
		c.ExtraProjects = []string{bad}
		if c.Validate() == nil {
			t.Errorf("%q 应该不行", bad)
		}
	}
	c := config.Default()
	c.ExtraProjects = []string{"/data/repo"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

// 老项目第一次接手：按项目文件写检查命令并提交成可执行文件，测试门认得；已有的文件不动。五种类型的判断在 internal/onboard 的单元测试。
func TestInitWritesVerifyScript(t *testing.T) {
	if onboard.VerifyRel != gate.VerifyScript {
		t.Fatalf("两边的路径要一样：%s %s", onboard.VerifyRel, gate.VerifyScript)
	}
	t.Run("判断得出", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		dir := onboardRepo(t, a, "go-app", true)
		testkit.WriteInstall(t, filepath.Join(dir, "go.mod"), "module x\n", 0o644)
		onboardGit(t, a.Home, dir, "add", "go.mod")
		onboardGit(t, a.Home, dir, "commit", "-q", "-m", "mod")
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, out.String(), ".agentq/verify", "go test ./...")
		if got := onboardGit(t, a.Home, dir, "ls-files", "-s", "--", ".agentq/verify"); !strings.HasPrefix(got, "100755 ") {
			t.Fatalf("要提交成可执行文件：%q", got)
		}
		testkit.AssertContains(t, testkit.ReadFile(t, filepath.Join(dir, ".agentq", "verify")), "exit "+strconv.Itoa(gate.ScriptCannotRun))
	})
	t.Run("判断不了", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		dir := onboardRepo(t, a, "plain-app", true)
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, out.String(), "没判断出检查命令")
		if exists(filepath.Join(dir, ".agentq")) {
			t.Fatal("判断不了就不写")
		}
	})
	t.Run("已有文件不动", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		dir := onboardRepo(t, a, "old-app", true)
		const script, agents, claude = "#!/bin/sh\nexit 0\n", "我的项目地图\n", "我的项目规则\n"
		testkit.WriteInstall(t, filepath.Join(dir, ".agentq", "verify"), script, 0o755)
		testkit.WriteInstall(t, filepath.Join(dir, "AGENTS.md"), agents, 0o644)
		testkit.WriteInstall(t, filepath.Join(dir, "CLAUDE.md"), claude, 0o644)
		testkit.WriteInstall(t, filepath.Join(dir, "go.mod"), "module x\n", 0o644)
		onboardGit(t, a.Home, dir, "add", "-A")
		onboardGit(t, a.Home, dir, "commit", "-q", "-m", "old")
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, filepath.Join(dir, ".agentq", "verify"), script)
		testkit.AssertBytes(t, filepath.Join(dir, "AGENTS.md"), agents)
		testkit.AssertBytes(t, filepath.Join(dir, "CLAUDE.md"), claude)
		if !exists(filepath.Join(dir, "docs", "BACKLOG.md")) {
			t.Fatal("别的缺的还要补")
		}
	})
	t.Run("只有 CLAUDE.md 也补 AGENTS.md，CLAUDE.md 不动", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		dir := onboardRepo(t, a, "claude-app", true)
		testkit.WriteInstall(t, filepath.Join(dir, "CLAUDE.md"), "我的\n", 0o644)
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, filepath.Join(dir, "CLAUDE.md"), "我的\n")
		if !exists(filepath.Join(dir, "AGENTS.md")) {
			t.Fatal("缺 AGENTS.md 要生成")
		}
	})
	t.Run("被 gitignore 忽略", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		dir := onboardRepo(t, a, "ignored-app", true)
		testkit.WriteInstall(t, filepath.Join(dir, ".gitignore"), ".agentq/\n", 0o644)
		testkit.WriteInstall(t, filepath.Join(dir, "go.mod"), "module x\n", 0o644)
		onboardGit(t, a.Home, dir, "add", "-A")
		onboardGit(t, a.Home, dir, "commit", "-q", "-m", "ignore")
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, out.String(), "被 .gitignore 忽略")
		if !exists(filepath.Join(dir, "docs", "BACKLOG.md")) || exists(filepath.Join(dir, ".agentq")) {
			t.Fatal("别的照常接入，脚本不写")
		}
	})
	t.Run("提交失败时撤干净", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		dir := onboardRepo(t, a, "hook-app", true)
		testkit.WriteInstall(t, filepath.Join(dir, "go.mod"), "module x\n", 0o644)
		onboardGit(t, a.Home, dir, "add", "go.mod")
		onboardGit(t, a.Home, dir, "commit", "-q", "-m", "mod")
		testkit.WriteInstall(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), "#!/bin/sh\nexit 1\n", 0o755)
		if err := a.Onboard.CmdInit([]string{dir}); err == nil {
			t.Fatal("应报错")
		}
		if exists(filepath.Join(dir, ".agentq")) || exists(filepath.Join(dir, "docs")) {
			t.Fatal("要撤干净")
		}
	})
}

// 真二进制：项目目录外的仓库，add 之后 tick 接得上任务，任务在那个目录里跑；再 add 一条不重复登记。
func TestExtraProjectRunsThroughTick(t *testing.T) {
	e, _ := sgEnv(t, nil)
	dir := filepath.Join(e.home, "code", "far-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.home, real, "init", "-q", "-b", "main")
	testkit.WriteInstall(t, filepath.Join(real, "README.md"), "x\n", 0o644)
	testkit.GitT(t, e.home, real, "add", "README.md")
	testkit.GitT(t, e.home, real, "commit", "-q", "-m", "first")
	cmdIn := func(args ...string) string {
		t.Helper()
		e.dir = real
		defer func() { e.dir = "" }()
		out, code := e.run(args...)
		if code != 0 {
			t.Fatalf("agentq %v：%d\n%s", args, code, out)
		}
		return out
	}
	testkit.AssertContains(t, cmdIn("add", "外面的活 SLEEP=1"), "已登记到 extra_projects", "已接入 far-app")
	e.mustRun("tick")
	e.waitShells()
	var got []string
	for _, j := range e.state().Jobs {
		got = append(got, j.Project+"="+j.ProjectDir)
	}
	if !slices.Equal(got, []string{"far-app=" + real}) {
		t.Fatalf("%v", got)
	}
	testkit.AssertContains(t, e.mustRun("status"), "far-app")
}

// 自己配的 projects_root 丢了：extra 照跑，错误要说出来。
func TestCustomRootMissingWithExtra(t *testing.T) {
	far := filepath.Join(t.TempDir(), "far-app")
	if err := os.MkdirAll(far, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(far)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := sgEnv(t, map[string]any{"projects_root": filepath.Join(t.TempDir(), "mounted"), "extra_projects": []string{real}})
	testkit.GitT(t, e.home, real, "init", "-q", "-b", "main")
	testkit.WriteInstall(t, filepath.Join(real, "docs", "BACKLOG.md"), "# 待办\n\n## 待做\n\n- [ ] 外面的活 SLEEP=1\n\n## 已完成\n", 0o644)
	testkit.GitT(t, e.home, real, "add", "-A")
	testkit.GitT(t, e.home, real, "commit", "-q", "-m", "first")
	out := e.mustRun("tick")
	testkit.AssertContains(t, out, "扫描项目目录失败")
	e.waitShells()
	var got []string
	for _, j := range e.state().Jobs {
		got = append(got, j.Project)
	}
	if !slices.Equal(got, []string{"far-app"}) {
		t.Fatalf("extra 要照跑：%v\n%s", got, out)
	}
	status := e.mustRun("status")
	testkit.AssertContains(t, status, "扫描失败")
}

// 列表不全时 AllProjects 返回错误；默认目录不存在不算错。
func TestAllProjectsPartial(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	far := outsideRepo(t, a, "far")
	a.Cfg.ExtraProjects = []string{far}
	a.Cfg.ProjectsRoot = filepath.Join(a.Home, "mounted")
	all, err := a.Backlog.AllProjects()
	if err == nil || len(all) != 1 || all[0].Dir != far {
		t.Fatalf("%+v %v", all, err)
	}
	if p, err := a.Chat.ActiveProject("far"); err != nil || p.Dir != far {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := a.Chat.ActiveProject("在根目录里的"); err == nil {
		t.Fatal("看不到的要报错")
	}
	a.Cfg.ProjectsRoot = "~/projects"
	if err := os.RemoveAll(a.ProjectsRoot()); err != nil {
		t.Fatal(err)
	}
	all, err = a.Backlog.AllProjects()
	if err != nil || len(all) != 1 {
		t.Fatalf("默认目录不存在不算错：%+v %v", all, err)
	}
}

// agentq projects：根目录下的和外面的都列出来，被排除的标出来；根目录丢了列看得到的并报错。
func TestProjectsCommand(t *testing.T) {
	far := filepath.Join(t.TempDir(), "far-app")
	secret := filepath.Join(t.TempDir(), "secret")
	var dirs []string
	for _, d := range []string{far, secret} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	e, _ := sgEnv(t, map[string]any{"extra_projects": dirs, "exclude_projects": []string{"secret"}})
	inRoot := e.sgProject("in-root", "# 待办\n\n## 待做\n\n## 已完成\n")
	want := "far-app\t" + far + "\nin-root\t" + inRoot.Dir + "\nsecret\t" + secret + "\texclude_projects\n"
	if got := e.mustRun("projects"); got != want {
		t.Fatalf("%q\n想要 %q", got, want)
	}
	e2, _ := sgEnv(t, map[string]any{"extra_projects": []string{far}, "projects_root": filepath.Join(t.TempDir(), "mounted")})
	out, code := e2.run("projects")
	if code != 1 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "far-app\t"+far, "上面可能不全")
}

// 技能和对话提示词按名字找项目要走 agentq projects，不能只看 projects_root。
func TestSkillsFindExtraProjects(t *testing.T) {
	for _, name := range []string{"skills/dispatch/SKILL.md", "skills/project-status/SKILL.md"} {
		b, err := installAssets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, string(b), "agentq projects", "extra_projects")
	}
	testkit.AssertContains(t, chat.ChatPromptHead, "agentq projects")
}

// 没有 ~/projects：tick 不往日志写扫描失败，当作没有活；配了别的目录丢了还是报。
func TestTickQuietWithoutDefaultProjectsRoot(t *testing.T) {
	e, _ := sgEnv(t, map[string]any{"projects_root": "~/projects"})
	if err := os.RemoveAll(filepath.Join(e.home, "projects")); err != nil {
		t.Fatal(err)
	}
	out := e.mustRun("tick")
	if strings.Contains(out, "扫描项目目录失败") || strings.Contains(e.launchdLog(), "扫描项目目录失败") {
		t.Fatalf("不该报：\n%s", out)
	}
	status := e.mustRun("status")
	if strings.Contains(status, "扫描失败") {
		t.Fatalf("status 不该说扫描失败：\n%s", status)
	}
	// 自己配的目录不见了：还是要说
	e2, _ := sgEnv(t, map[string]any{"projects_root": filepath.Join(t.TempDir(), "mounted")})
	if out := e2.mustRun("tick"); !strings.Contains(out, "扫描项目目录失败") {
		t.Fatalf("配的目录丢了要说：\n%s", out)
	}
}
