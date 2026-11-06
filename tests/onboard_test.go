package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/gitx"
	"agentq/internal/onboard"
	"agentq/internal/setup"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// onboardApp：装好了的临时环境（临时 HOME、假 claude、假 launchctl），projects_root 是 $HOME/projects。
func onboardApp(t *testing.T) (*App, *setup.Installer, *fakeLaunchctl, *bytes.Buffer) {
	t.Helper()
	i, o := installFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "") // 夹具故意设了别的目录；start 和 doctor 入口一样不接受
	f := &fakeLaunchctl{}
	i.Launchctl = f.run
	mustInstall(t, i, o)
	var out bytes.Buffer
	a, err := newApp(i.Home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a.Service.Launchctl = f.run
	if err := os.MkdirAll(a.ProjectsRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	return a, i, f, &out
}

// newLegacyConfigApp：配置文件已有、但键都缺（老配置），全部走缺键默认。这些用例测的是推送、巡逻打开后的行为，和新建配置的默认无关。
func newLegacyConfigApp(t *testing.T, home string, out io.Writer) (*App, error) {
	t.Helper()
	testkit.WriteInstall(t, filepath.Join(home, ".agentq", "config.json"), "{}\n", 0o600)
	return newApp(home, out)
}

// onboardGit 在 dir 里跑 git，HOME 指向临时目录。
func onboardGit(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitx.GitEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// onboardRepo 建一个配好身份的仓库，withCommit 决定有没有首个提交。
func onboardRepo(t *testing.T, a *App, name string, withCommit bool) string {
	t.Helper()
	dir := filepath.Join(a.ProjectsRoot(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	onboardGit(t, a.Home, dir, "init", "-q", "-b", "main")
	onboardGit(t, a.Home, dir, "config", "user.name", "t")
	onboardGit(t, a.Home, dir, "config", "user.email", "t")
	if withCommit {
		testkit.WriteInstall(t, filepath.Join(dir, "README.md"), "readme\n", 0o644)
		onboardGit(t, a.Home, dir, "add", "README.md")
		onboardGit(t, a.Home, dir, "commit", "-q", "-m", "first")
	}
	return dir
}

func onboardFilesAll() []string {
	var out []string
	for _, f := range onboard.OnboardFiles {
		out = append(out, f.Path)
	}
	return out
}

// init 正常：五个文件都从模板生成并提交，只提交它们；别的改动（工作区、暂存区）原样；重复运行什么都不变。
func TestInitScaffoldsCommitsOnlyItsFilesAndIsIdempotent(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := onboardRepo(t, a, "my-app", true)
	testkit.WriteInstall(t, filepath.Join(dir, "README.md"), "dirty\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, "staged.txt"), "staged\n", 0o644)
	onboardGit(t, a.Home, dir, "add", "staged.txt")
	testkit.WriteInstall(t, filepath.Join(dir, "untracked.txt"), "untracked\n", 0o644)
	before := onboardGit(t, a.Home, dir, "status", "--porcelain")

	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range onboard.OnboardFiles {
		want, err := installAssets.ReadFile(f.Template)
		if err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, filepath.Join(dir, f.Path), string(want))
	}
	got := strings.Fields(onboardGit(t, a.Home, dir, "show", "--pretty=format:", "--name-only", "HEAD"))
	if len(got) != len(onboard.OnboardFiles) {
		t.Fatalf("提交里应只有这几个文件：%v", got)
	}
	if after := onboardGit(t, a.Home, dir, "status", "--porcelain"); after != before {
		t.Fatalf("无关改动被动了：\n前 %s\n后 %s", before, after)
	}
	testkit.AssertContains(t, out.String(), "my-app", "agentq add")

	head := onboardGit(t, a.Home, dir, "rev-parse", "HEAD")
	out.Reset()
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	if onboardGit(t, a.Home, dir, "rev-parse", "HEAD") != head {
		t.Fatal("重复运行不应再提交")
	}
	testkit.AssertContains(t, out.String(), "已经接入")
}

// 不写目录时用当前目录。
func TestInitDefaultsToCurrentDirectory(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	dir := onboardRepo(t, a, "cwd-app", true)
	t.Chdir(dir)
	if err := a.Onboard.CmdInit(nil); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "docs", "BACKLOG.md")) {
		t.Fatal("当前目录没接入")
	}
}

// 已有的文件一个字节都不动，只补缺的。
func TestInitKeepsExistingFiles(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := onboardRepo(t, a, "has-files", true)
	testkit.WriteInstall(t, filepath.Join(dir, "AGENTS.md"), "我自己的 AGENTS\n", 0o644)
	backlog := "# 我的待办\n\n## 待做\n\n- [ ] 旧条目\n"
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), backlog, 0o644)
	onboardGit(t, a.Home, dir, "add", ".")
	onboardGit(t, a.Home, dir, "commit", "-q", "-m", "mine")
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertBytes(t, filepath.Join(dir, "AGENTS.md"), "我自己的 AGENTS\n")
	testkit.AssertBytes(t, filepath.Join(dir, "docs", "BACKLOG.md"), backlog)
	got := onboardGit(t, a.Home, dir, "show", "--pretty=format:", "--name-only", "HEAD")
	if strings.Contains(got, "AGENTS.md") || strings.Contains(got, "BACKLOG.md") || !strings.Contains(got, "PROGRESS.md") {
		t.Fatalf("提交内容不对：%s", got)
	}
	testkit.AssertContains(t, out.String(), "已有的没动")
}

// 已有的 BACKLOG 用不了时当场说，不覆盖。
func TestInitWarnsAboutUnusableBacklog(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := onboardRepo(t, a, "odd-backlog", true)
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), "# 没有那一节\n", 0o644)
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "待做", "没提交的改动")
}

// 还没有提交的仓库：生成首个提交；暂存着的无关文件不进提交。
func TestInitUnbornRepositoryKeepsStagedWork(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	dir := onboardRepo(t, a, "unborn", false)
	testkit.WriteInstall(t, filepath.Join(dir, "staged.txt"), "staged\n", 0o644)
	onboardGit(t, a.Home, dir, "add", "staged.txt")
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	got := onboardGit(t, a.Home, dir, "show", "--pretty=format:", "--name-only", "HEAD")
	if strings.Contains(got, "staged.txt") || !strings.Contains(got, "BACKLOG.md") {
		t.Fatalf("首个提交内容不对：%s", got)
	}
	if st := onboardGit(t, a.Home, dir, "status", "--porcelain"); !strings.Contains(st, "A  staged.txt") {
		t.Fatalf("暂存的文件应保持暂存：%s", st)
	}
}

// 不满足条件：报具体哪条和要执行什么，不擅自 git init、不改分支、不移动目录。
func TestInitRefusesWithSpecificFix(t *testing.T) {
	a, _, _, _ := onboardApp(t)

	t.Run("不是 git 仓库", func(t *testing.T) {
		dir := filepath.Join(a.ProjectsRoot(), "plain")
		os.MkdirAll(dir, 0o755)
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "init -b main", dir)
		if exists(filepath.Join(dir, ".git")) || exists(filepath.Join(dir, "AGENTS.md")) {
			t.Fatal("不该动目录")
		}
	})
	t.Run("目录不存在", func(t *testing.T) {
		if err := a.Onboard.CmdInit([]string{filepath.Join(a.ProjectsRoot(), "nope")}); err == nil {
			t.Fatal("应拒绝")
		}
	})
	t.Run("子目录", func(t *testing.T) {
		dir := onboardRepo(t, a, "with-sub", true)
		sub := filepath.Join(dir, "src")
		os.MkdirAll(sub, 0o755)
		err := a.Onboard.CmdInit([]string{sub})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "不是它的主目录")
	})
	t.Run("worktree", func(t *testing.T) {
		dir := onboardRepo(t, a, "with-wt", true)
		wt := filepath.Join(t.TempDir(), "wt")
		onboardGit(t, a.Home, dir, "worktree", "add", "-q", "-b", "side", wt)
		err := a.Onboard.CmdInit([]string{wt})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "worktree")
	})
	t.Run("分支不是 main 也没有 main", func(t *testing.T) {
		dir := onboardRepo(t, a, "on-trunk", true)
		onboardGit(t, a.Home, dir, "branch", "-m", "main", "trunk")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "branch -m", "trunk", "main")
		if br := onboardGit(t, a.Home, dir, "symbolic-ref", "--short", "HEAD"); br != "trunk" {
			t.Fatalf("不该改分支：%s", br)
		}
		if exists(filepath.Join(dir, "AGENTS.md")) {
			t.Fatal("不该生成文件")
		}
	})
	t.Run("在别的分支有 main", func(t *testing.T) {
		dir := onboardRepo(t, a, "on-feature", true)
		onboardGit(t, a.Home, dir, "checkout", "-q", "-b", "feature")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "switch main")
	})
	t.Run("游离 HEAD", func(t *testing.T) {
		dir := onboardRepo(t, a, "detached", true)
		onboardGit(t, a.Home, dir, "checkout", "-q", "--detach")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "switch main")
	})
	t.Run("没提交的别的分支", func(t *testing.T) {
		dir := filepath.Join(a.ProjectsRoot(), "unborn-trunk")
		os.MkdirAll(dir, 0o755)
		onboardGit(t, a.Home, dir, "init", "-q", "-b", "trunk")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "checkout -b main")
	})
	t.Run("不在 projects_root 下一层", func(t *testing.T) {
		dir := filepath.Join(a.Home, "elsewhere")
		os.MkdirAll(dir, 0o755)
		onboardGit(t, a.Home, dir, "init", "-q", "-b", "main")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "mv ", "projects_root")
		if !exists(dir) || exists(filepath.Join(dir, "AGENTS.md")) {
			t.Fatal("不该移动或生成")
		}
	})
	t.Run("多一层也不行", func(t *testing.T) {
		dir := filepath.Join(a.ProjectsRoot(), "group", "deep")
		os.MkdirAll(dir, 0o755)
		onboardGit(t, a.Home, dir, "init", "-q", "-b", "main")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "projects_root")
	})
	t.Run("目录名以点开头", func(t *testing.T) {
		dir := onboardRepo(t, a, ".hidden", true)
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "以 . 开头")
	})
	t.Run("不认识的参数", func(t *testing.T) {
		if err := a.Onboard.CmdInit([]string{"--force"}); err == nil {
			t.Fatal("应拒绝")
		}
	})
}

// runHintCommand 把错误信息里第一行缩进的命令真的跑一遍。
func runHintCommand(t *testing.T, a *App, msg string) {
	t.Helper()
	for _, line := range strings.Split(msg, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		cmd := exec.Command("/bin/sh", "-c", strings.TrimSpace(line))
		cmd.Env = gitx.GitEnv(a.Home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("提示的命令跑不通：%s：%v\n%s", line, err, out)
		}
		return
	}
	t.Fatalf("错误信息里没有命令：%s", msg)
}

// projects_root 还不存在：照提示做要把项目移进去，不能把仓库改名成 projects_root。
func TestInitRootMissingHintMovesIntoRoot(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	if err := os.RemoveAll(a.ProjectsRoot()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.Home, "code", "my-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	onboardGit(t, a.Home, dir, "init", "-q", "-b", "main")
	onboardGit(t, a.Home, dir, "config", "user.name", "t")
	onboardGit(t, a.Home, dir, "config", "user.email", "t")
	err := a.Onboard.CmdInit([]string{dir})
	if err == nil {
		t.Fatal("应拒绝")
	}
	testkit.AssertContains(t, err.Error(), "mkdir -p", "my-app")
	if exists(a.ProjectsRoot()) {
		t.Fatal("不该替用户建目录、移动项目")
	}
	runHintCommand(t, a, err.Error())
	moved := filepath.Join(a.ProjectsRoot(), "my-app")
	if !gitx.IsGitMain(moved) {
		t.Fatalf("照提示做之后项目应在 %s", moved)
	}
	if err := a.Onboard.CmdInit([]string{moved}); err != nil {
		t.Fatalf("照提示做之后应能接入：%v", err)
	}
}

// 目标已有同名目录：mv 会移进去变成子目录，不能给这条命令。
func TestInitRootHintWhenNameTaken(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	onboardRepo(t, a, "my-app", true)
	dir := filepath.Join(a.Home, "code", "my-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	onboardGit(t, a.Home, dir, "init", "-q", "-b", "main")
	err := a.Onboard.CmdInit([]string{dir})
	if err == nil {
		t.Fatal("应拒绝")
	}
	if strings.Contains(err.Error(), "mv ") {
		t.Fatalf("同名已存在时不该给 mv：%s", err)
	}
	testkit.AssertContains(t, err.Error(), "已有", "my-app", "projects_root")
}

// 仓库本身就是 projects_root（照旧提示改名过来的）：单独说明怎么恢复，不提示不可能执行的命令；照做能恢复。
func TestInitProjectIsRootItself(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	root := a.ProjectsRoot()
	onboardGit(t, a.Home, root, "init", "-q", "-b", "main")
	onboardGit(t, a.Home, root, "config", "user.name", "t")
	onboardGit(t, a.Home, root, "config", "user.email", "t")
	err := a.Onboard.CmdInit([]string{root})
	if err == nil {
		t.Fatal("应拒绝")
	}
	testkit.AssertContains(t, err.Error(), "就是 projects_root", "my-project")
	if strings.Contains(err.Error(), "projects_root 改成") {
		t.Fatalf("不该建议把 projects_root 改到家目录：%s", err)
	}
	runHintCommand(t, a, err.Error())
	moved := filepath.Join(root, "my-project")
	if !gitx.IsGitMain(moved) || gitx.IsGitMain(root) {
		t.Fatalf("应恢复成 %s", moved)
	}
	if err := a.Onboard.CmdInit([]string{moved}); err != nil {
		t.Fatalf("恢复后应能接入：%v", err)
	}
}

// 在放项目的目录里运行（不是仓库）：告诉他进项目目录，不提示 git init 把它变成仓库。
func TestInitInProjectsRootNotRepo(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	err := a.Onboard.CmdInit([]string{a.ProjectsRoot()})
	if err == nil {
		t.Fatal("应拒绝")
	}
	testkit.AssertContains(t, err.Error(), "放项目的目录")
	if strings.Contains(err.Error(), "git -C") {
		t.Fatalf("不该提示在这里 git init：%s", err)
	}
}

// 提交失败（钩子拒绝、文件被忽略）：这次生成的都撤掉，别的不动；原因要说出来。
func TestInitRollsBackWhenCommitFails(t *testing.T) {
	a, _, _, _ := onboardApp(t)

	dir := onboardRepo(t, a, "hook-rejects", true)
	testkit.WriteInstall(t, filepath.Join(dir, "staged.txt"), "staged\n", 0o644)
	onboardGit(t, a.Home, dir, "add", "staged.txt")
	before := onboardGit(t, a.Home, dir, "status", "--porcelain")
	testkit.WriteInstall(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), "#!/bin/sh\necho 钩子不让提交\nexit 1\n", 0o755)
	head := onboardGit(t, a.Home, dir, "rev-parse", "HEAD")
	err := a.Onboard.CmdInit([]string{dir})
	if err == nil {
		t.Fatal("提交失败应报错")
	}
	testkit.AssertContains(t, err.Error(), "已撤掉", "钩子不让提交")
	for _, f := range onboardFilesAll() {
		if exists(filepath.Join(dir, f)) {
			t.Errorf("%s 应撤掉", f)
		}
	}
	if exists(filepath.Join(dir, "docs")) {
		t.Error("这次建的 docs 目录应撤掉")
	}
	if onboardGit(t, a.Home, dir, "rev-parse", "HEAD") != head {
		t.Error("不应多提交")
	}
	if after := onboardGit(t, a.Home, dir, "status", "--porcelain"); after != before {
		t.Errorf("状态变了：前 %q 后 %q", before, after)
	}

	dir = onboardRepo(t, a, "ignored", true)
	testkit.WriteInstall(t, filepath.Join(dir, ".gitignore"), "LESSONS.md\n", 0o644)
	err = a.Onboard.CmdInit([]string{dir})
	if err == nil {
		t.Fatal("文件被忽略应报错")
	}
	testkit.AssertContains(t, err.Error(), ".gitignore")
	if exists(filepath.Join(dir, "AGENTS.md")) {
		t.Error("应全部撤掉")
	}
}

func commitCount(t *testing.T, a *App, dir string) string {
	t.Helper()
	return onboardGit(t, a.Home, dir, "rev-list", "--count", "HEAD")
}

// add 正常：加在「待做」末尾，模板注释里的示例不算；只提交 BACKLOG；别的改动不碰；子目录和 --project 都能找到项目。
func TestAddAppendsToTodoAndCommitsOnlyBacklog(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := onboardRepo(t, a, "my-app", true)
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	testkit.WriteInstall(t, filepath.Join(dir, "README.md"), "dirty\n", 0o644)
	sub := filepath.Join(dir, "src", "deep")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	out.Reset()
	if err := a.Onboard.CmdAdd([]string{"第一条"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "my-app", "第一条", "agentq start")
	t.Chdir(a.Home)
	if err := a.Onboard.CmdAdd([]string{"--project", "my-app", "第二条"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Onboard.CmdAdd([]string{"第三条", "--project=my-app"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Onboard.CmdAdd([]string{"--project", "my-app", "--", "-以减号开头"}); err != nil {
		t.Fatal(err)
	}
	items, err := backlog.Project{Name: "my-app", Dir: dir}.Items()
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
		if !it.Eligible() {
			t.Errorf("%q 应该能被接", it.Title)
		}
	}
	if strings.Join(titles, "|") != "第一条|第二条|第三条|-以减号开头" {
		t.Fatalf("顺序不对：%v", titles)
	}
	if got := onboardGit(t, a.Home, dir, "show", "--pretty=format:", "--name-only", "HEAD"); got != "docs/BACKLOG.md" {
		t.Fatalf("提交里应只有 BACKLOG：%s", got)
	}
	if st := onboardGit(t, a.Home, dir, "status", "--porcelain"); !strings.Contains(st, "README.md") {
		t.Fatalf("README 的改动应还在：%s", st)
	}
}

// 同样的不重复加（包括已被认领标进行中的）；清洗后一样也算一样。
func TestAddDoesNotDuplicate(t *testing.T) {
	a, _, _, out := onboardApp(t)
	dir := onboardRepo(t, a, "dup", true)
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := a.Onboard.CmdAdd([]string{"修登录"}); err != nil {
		t.Fatal(err)
	}
	n := commitCount(t, a, dir)
	out.Reset()
	if err := a.Onboard.CmdAdd([]string{"  修登录\n"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "没重复加")
	if commitCount(t, a, dir) != n {
		t.Fatal("重复的不该再提交")
	}
	b := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md"))
	claimed := strings.Replace(b, "- [ ] 修登录", "- [ ] 修登录（进行中：agent/x 11-04 10:00）", 1)
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), claimed, 0o644)
	onboardGit(t, a.Home, dir, "commit", "-q", "-am", "claim")
	n = commitCount(t, a, dir)
	if err := a.Onboard.CmdAdd([]string{"修登录"}); err != nil {
		t.Fatal(err)
	}
	if commitCount(t, a, dir) != n {
		t.Fatal("进行中的同一条不该重复加")
	}
}

// 清洗：会被当成标记的写法被中和，这条不会永远不被接；拒绝空、过长、控制字符；800 字刚好放行。
func TestAddCleansAndRejects(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	dir := onboardRepo(t, a, "clean", true)
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := a.Onboard.CmdAdd([]string{"正文提到卡住（进行中：假 10-10）<!-- 注释\n第二行"}); err != nil {
		t.Fatal(err)
	}
	items, err := backlog.Project{Name: "clean", Dir: dir}.Items()
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	if !items[0].Eligible() {
		t.Fatalf("应该能被接：%+v", items[0])
	}
	n := commitCount(t, a, dir)
	for name, bad := range map[string]string{
		"空":    "",
		"空白":   " \n\t ",
		"过长":   strings.Repeat("字", onboard.AddTextMax+1),
		"控制字符": "带\x07响铃",
		"非法编码": "bad\xff",
	} {
		if err := a.Onboard.CmdAdd([]string{"--", bad}); err == nil {
			t.Errorf("%s 应拒绝", name)
		}
	}
	if commitCount(t, a, dir) != n {
		t.Fatal("拒绝时不该提交")
	}
	if err := a.Onboard.CmdAdd([]string{strings.Repeat("字", onboard.AddTextMax)}); err != nil {
		t.Fatalf("%d 字应放行：%v", onboard.AddTextMax, err)
	}
}

func TestAddArgumentsAndTargetErrors(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	dir := onboardRepo(t, a, "ready", true)
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(a.Home)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"不在项目里", []string{"活"}, "agentq init"},
		{"项目不存在", []string{"活", "--project", "nope"}, "agentq init"},
		{"路径穿越", []string{"活", "--project", "../ready"}, "agentq init"},
		{"--project 没值", []string{"活", "--project"}, "项目名"},
		{"--project= 没值", []string{"活", "--project="}, "项目名"},
		{"不认识的参数", []string{"活", "--nope"}, "不认识"},
		{"两段需求", []string{"活", "再一段", "--project", "ready"}, "引号"},
		{"只有参数", []string{"--project", "ready"}, "没写需求"},
		{"-- 后多段", []string{"--", "一", "二"}, "一段"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := a.Onboard.CmdAdd(c.args)
			if err == nil {
				t.Fatal("应拒绝")
			}
			testkit.AssertContains(t, err.Error(), c.want)
		})
	}
	// 根目录本身不属于任何项目
	t.Chdir(a.ProjectsRoot())
	if err := a.Onboard.CmdAdd([]string{"活"}); err == nil {
		t.Fatal("projects_root 本身不是项目")
	}
	// BACKLOG 有没提交的改动、主目录不在 main：和其他入口一样拒绝，文件不动
	t.Chdir(dir)
	path := filepath.Join(dir, "docs", "BACKLOG.md")
	testkit.WriteInstall(t, path, testkit.ReadFile(t, path)+"手改\n", 0o644)
	if err := a.Onboard.CmdAdd([]string{"活"}); err == nil {
		t.Fatal("有没提交的改动应拒绝")
	}
	onboardGit(t, a.Home, dir, "checkout", "-q", "--", "docs/BACKLOG.md")
	onboardGit(t, a.Home, dir, "checkout", "-q", "-b", "feature")
	if err := a.Onboard.CmdAdd([]string{"活"}); err == nil {
		t.Fatal("不在 main 应拒绝")
	}
}

// failReader：一读就算失败。用来断言「不该问」。
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("不该读标准输入")
	return 0, errors.New("不该读")
}

func configPath(a *App) string { return filepath.Join(a.Home, ".agentq", "config.json") }

// writeCustomConfig：把配置改成手写的样子（不认识的键、特殊排版），用来断言「别的字节不动」。
func writeCustomConfig(t *testing.T, a *App, transform func(string) string) string {
	t.Helper()
	b := testkit.ReadFile(t, configPath(a))
	custom := strings.Replace(b, "{", "{\n  \"user_extension\" :   \"keep  me\",", 1)
	if transform != nil {
		custom = transform(custom)
	}
	testkit.WriteInstall(t, configPath(a), custom, 0o600)
	if err := json.Unmarshal([]byte(custom), &map[string]any{}); err != nil {
		t.Fatalf("测试写坏了配置：%v\n%s", err, custom)
	}
	cfg, err := config.Load(configPath(a))
	if err != nil {
		t.Fatal(err)
	}
	a.Cfg = cfg
	return custom
}

// wantKeysSet：old 只把这些键换成给定的值，别的字节不变。
func wantKeysSet(t *testing.T, old string, kv ...string) string {
	t.Helper()
	cur := []byte(old)
	for i := 0; i < len(kv); i += 2 {
		var err error
		if cur, err = config.SetJSONKey(cur, kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return string(cur)
}

func startReloadCfg(t *testing.T, a *App) config.Config {
	t.Helper()
	cfg, err := config.Load(configPath(a))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// 不是终端、没带 --accept-bypass：不改、不开后台、退出非 0；给 y 也不算（管道里的 y 不算本人确认）。
func TestStartRefusesWithoutTerminal(t *testing.T) {
	for name, in := range map[string]string{"空输入": "", "管道里的 y": "y\n"} {
		t.Run(name, func(t *testing.T) {
			a, i, f, out := onboardApp(t)
			before := testkit.ReadFile(t, configPath(a))
			err := a.Onboard.CmdStart(nil, onboard.StartOptions{In: strings.NewReader(in), Interactive: false, Installer: i})
			if err == nil {
				t.Fatal("应退出非 0")
			}
			testkit.AssertContains(t, err.Error(), "--accept-bypass")
			testkit.AssertContains(t, out.String(), "所有文件", "密钥", "容器", "permission_mode")
			testkit.AssertBytes(t, configPath(a), before)
			if len(f.calls) != 0 {
				t.Fatalf("不该碰服务：%v", f.calls)
			}
			if r := mustReceipt(t, i); len(r.Services) != 0 {
				t.Fatal("不该装服务")
			}
		})
	}
}

// --accept-bypass：权限只改这一个键，再把 paused 改掉，其余字节不变；夜班、复盘、巡逻开关不动；不装 Telegram listen；服务真装上。
func TestStartAcceptBypassChangesOnlyThoseKeys(t *testing.T) {
	a, i, f, out := onboardApp(t)
	custom := writeCustomConfig(t, a, nil)
	if err := a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{In: failReader{t}, Interactive: false, Installer: i}); err != nil {
		t.Fatal(err)
	}
	want := wantKeysSet(t, custom, "permission_mode", `"bypassPermissions"`, "paused", "false")
	testkit.AssertBytes(t, configPath(a), want)
	cfg := startReloadCfg(t, a)
	if cfg.Paused || cfg.NightEnabled || cfg.ReviewEnabled || cfg.PatrolEnabled || cfg.NotifyEnabled {
		t.Fatalf("开关不对：%+v", cfg)
	}
	r := mustReceipt(t, i)
	if len(r.Services) != 3 {
		t.Fatalf("应装 tick、夜班、复盘三个服务：%v", r.Services)
	}
	for _, s := range r.Services {
		if strings.HasSuffix(s, ".listen") {
			t.Fatal("不该装 Telegram listen")
		}
		if !f.loaded[s] {
			t.Errorf("%s 没装上", s)
		}
	}
	testkit.AssertContains(t, out.String(), "agentq status", "agentq pause", "所有文件")
}

// 终端里问：y 才改；其余回答、直接回车、读到头都不改不开，提示能手动设。
func TestStartInteractiveAnswers(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n"} {
		a, i, _, out := onboardApp(t)
		if err := a.Onboard.CmdStart(nil, onboard.StartOptions{In: strings.NewReader(answer), Interactive: true, Installer: i}); err != nil {
			t.Fatalf("%q：%v", answer, err)
		}
		cfg := startReloadCfg(t, a)
		if cfg.Paused || cfg.PermissionMode != "bypassPermissions" {
			t.Fatalf("%q：%+v", answer, cfg)
		}
		testkit.AssertContains(t, out.String(), "[y/N]")
	}
	for _, answer := range []string{"n\n", "\n", "", "ok\n", "yy\n"} {
		a, i, f, out := onboardApp(t)
		before := testkit.ReadFile(t, configPath(a))
		err := a.Onboard.CmdStart(nil, onboard.StartOptions{In: strings.NewReader(answer), Interactive: true, Installer: i})
		if !errors.Is(err, env.ErrLogged) {
			t.Fatalf("%q 应是拒绝退出：%v", answer, err)
		}
		testkit.AssertBytes(t, configPath(a), before)
		if len(f.calls) != 0 || len(mustReceipt(t, i).Services) != 0 {
			t.Fatalf("%q 不该开后台", answer)
		}
		testkit.AssertContains(t, out.String(), "没改权限", "permission_mode")
	}
}

// 配置里没有这个键（老用户，默认就是完全权限）或已经是完全权限：不问，权限这个键的字节不动。
func TestStartNoQuestionWhenAlreadyFull(t *testing.T) {
	for name, transform := range map[string]func(string) string{
		"没有这个键": func(s string) string {
			return strings.Replace(s, "  \"permission_mode\": \"default\",\n", "", 1)
		},
		"已经是完全权限": func(s string) string {
			return strings.Replace(s, `"permission_mode": "default"`, `"permission_mode": "bypassPermissions"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, i, _, out := onboardApp(t)
			custom := writeCustomConfig(t, a, transform)
			if err := a.Onboard.CmdStart(nil, onboard.StartOptions{In: failReader{t}, Interactive: false, Installer: i}); err != nil {
				t.Fatal(err)
			}
			testkit.AssertBytes(t, configPath(a), wantKeysSet(t, custom, "paused", "false"))
			if strings.Contains(out.String(), "所有文件") {
				t.Errorf("不该讲权限：%s", out)
			}
		})
	}
}

// 其他显式模式（acceptEdits）也要问：写得了文件但跑不了命令，无人值守做不完。
func TestStartAsksForOtherExplicitModes(t *testing.T) {
	a, i, _, _ := onboardApp(t)
	before := writeCustomConfig(t, a, func(s string) string {
		return strings.Replace(s, `"permission_mode": "default"`, `"permission_mode": "acceptEdits"`, 1)
	})
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{In: strings.NewReader(""), Interactive: false, Installer: i}); err == nil {
		t.Fatal("应拒绝")
	}
	testkit.AssertBytes(t, configPath(a), before)
}

// 服务装不上：paused 不改，安装回到原样；已经同意的权限留着，下次不用再问。
func TestStartServiceFailureKeepsPaused(t *testing.T) {
	a, i, f, out := onboardApp(t)
	custom := writeCustomConfig(t, a, nil)
	gen := mustReceipt(t, i).Generation
	fail := func(args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			return []byte("Bootstrap failed: 5"), errors.New("exit status 5")
		}
		return f.run(args...)
	}
	i.Launchctl = fail
	a.Service.Launchctl = fail
	err := a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{In: failReader{t}, Installer: i})
	if err == nil {
		t.Fatal("服务装不上应报错")
	}
	testkit.AssertContains(t, err.Error(), "没装上")
	testkit.AssertBytes(t, configPath(a), wantKeysSet(t, custom, "permission_mode", `"bypassPermissions"`))
	r := mustReceipt(t, i)
	if r.Generation != gen || len(r.Services) != 0 {
		t.Fatalf("安装应回到原样：%+v", r)
	}
	if strings.Contains(out.String(), "后台已打开") {
		t.Fatal("没开就不该说开了")
	}
}

// Linux 上 systemd 用不了：同样报错、paused 不改。
func TestStartSystemdUnavailable(t *testing.T) {
	a, i, _, _ := onboardApp(t)
	before := writeCustomConfig(t, a, func(s string) string {
		return strings.Replace(s, `"permission_mode": "default"`, `"permission_mode": "bypassPermissions"`, 1)
	})
	dead := func(args ...string) ([]byte, error) { return nil, errors.New("Failed to connect to bus") }
	i.Launchctl, i.Systemctl = nil, dead
	a.Service.Launchctl, a.Service.Systemctl = nil, dead
	err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i})
	if err == nil {
		t.Fatal("应报错")
	}
	testkit.AssertContains(t, err.Error(), "systemctl --user is not usable")
	testkit.AssertBytes(t, configPath(a), before)
}

// 检查不过：什么都不改。
func TestStartChecksFail(t *testing.T) {
	t.Run("没有完整安装", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		var out bytes.Buffer
		a, err := newApp(home, &out)
		if err != nil {
			t.Fatal(err)
		}
		before := testkit.ReadFile(t, configPath(a))
		err = a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: &setup.Installer{Assets: installAssets, Home: home, Launchctl: (&fakeLaunchctl{}).run}})
		if err == nil {
			t.Fatal("应报错")
		}
		testkit.AssertContains(t, err.Error(), "什么都没改", "安装脚本")
		testkit.AssertBytes(t, configPath(a), before)
	})
	t.Run("Claude 没登录", func(t *testing.T) {
		a, i, f, _ := onboardApp(t)
		before := testkit.ReadFile(t, configPath(a))
		testkit.WriteInstall(t, a.Cfg.ClaudePath, strings.Replace(probeClaude, `"loggedIn":true`, `"loggedIn":false`, 1), 0o755)
		err := a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i})
		if err == nil {
			t.Fatal("应报错")
		}
		testkit.AssertContains(t, err.Error(), "什么都没改", "claude auth login")
		testkit.AssertBytes(t, configPath(a), before)
		if len(f.calls) != 0 {
			t.Fatalf("不该碰服务：%v", f.calls)
		}
	})
	t.Run("只装了技能", func(t *testing.T) {
		i, o := installFixture(t)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		o.Scope = "skills"
		mustInstall(t, i, o)
		var out bytes.Buffer
		a, err := newApp(i.Home, &out)
		if err != nil {
			t.Fatal(err)
		}
		err = a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i})
		if err == nil {
			t.Fatal("应报错")
		}
		testkit.AssertContains(t, err.Error(), "只装了技能")
	})
	t.Run("不认识的参数", func(t *testing.T) {
		a, i, _, _ := onboardApp(t)
		if err := a.Onboard.CmdStart([]string{"--force"}, onboard.StartOptions{Installer: i}); err == nil {
			t.Fatal("应拒绝")
		}
	})
}

// 可重复运行：服务已在就不再走安装（不换代、运行中任务不挡路）；服务被卸下了就再装上。
func TestStartRepeatable(t *testing.T) {
	a, i, f, _ := onboardApp(t)
	if err := a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	gen := mustReceipt(t, i).Generation
	if err := a.Sched.CmdSetPaused(true); err != nil {
		t.Fatal(err)
	}
	st := &State{Jobs: []*Job{{ID: "running-job", Project: "x", Status: state.StatusRunning, PID: os.Getpid()}}}
	if err := state.Save(a.StatePath(), st); err != nil {
		t.Fatal(err)
	}
	if err := a.Onboard.EnsureServices(i); err != nil {
		t.Fatalf("运行中的任务不该挡住已装好的服务：%v", err)
	}
	if err := os.Remove(a.StatePath()); err != nil {
		t.Fatal(err)
	}
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{In: failReader{t}, Installer: i}); err != nil {
		t.Fatal(err)
	}
	if mustReceipt(t, i).Generation != gen {
		t.Fatal("服务已在时不该换代")
	}
	if cfg := startReloadCfg(t, a); cfg.Paused {
		t.Fatal("start 应取消暂停")
	}
	f.loaded[a.Cfg.LaunchdLabel] = false
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	if !f.loaded[a.Cfg.LaunchdLabel] {
		t.Fatal("服务被卸下后应重新装上")
	}
}

// listenEnv：可选配好 Telegram、打开推送。tick 会连 Telegram，指向本地假服务，绝不联网。
func listenEnv(t *testing.T, creds, notify bool) (*App, *setup.Installer, *fakeLaunchctl, *bytes.Buffer) {
	t.Helper()
	a, i, f, out := onboardApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	t.Cleanup(srv.Close)
	a.Telegram.TelegramAPI = srv.URL
	if creds {
		testkit.WriteInstall(t, a.Telegram.TelegramEnvPath(), "TELEGRAM_BOT_TOKEN=x\nTELEGRAM_CHAT_ID=1\n", 0o600)
	}
	writeCustomConfig(t, a, func(s string) string {
		s = strings.Replace(s, `"permission_mode": "default"`, `"permission_mode": "bypassPermissions"`, 1)
		if notify {
			s = strings.Replace(s, `"notify_enabled": false`, `"notify_enabled": true`, 1)
		}
		return s
	})
	return a, i, f, out
}

// 没配 Telegram 不装监听；配了但推送关着也不装，还要说一声；凭据坏了不装也说一声。
func TestStartSkipsListenWithoutTelegram(t *testing.T) {
	t.Run("没配", func(t *testing.T) {
		a, i, f, _ := listenEnv(t, false, true)
		if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(mustReceipt(t, i).Services, a.Service.ListenLabel()) || f.loaded[a.Service.ListenLabel()] {
			t.Fatal("没配 Telegram 不该装监听")
		}
	})
	t.Run("配了但推送关着", func(t *testing.T) {
		a, i, f, out := listenEnv(t, true, false)
		if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(mustReceipt(t, i).Services, a.Service.ListenLabel()) || f.loaded[a.Service.ListenLabel()] {
			t.Fatal("推送关着不该装监听")
		}
		testkit.AssertContains(t, out.String(), "notify_enabled=false", "agentq start")
	})
	t.Run("凭据权限不对", func(t *testing.T) {
		a, i, f, out := listenEnv(t, true, true)
		if err := os.Chmod(a.Telegram.TelegramEnvPath(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
			t.Fatal(err)
		}
		if f.loaded[a.Service.ListenLabel()] {
			t.Fatal("凭据读不了不该装监听")
		}
		testkit.AssertContains(t, out.String(), "先不装消息监听", "chmod 600")
	})
}

// 配好且推送开着：和调度服务一起装；之后才配也能补上；重复运行不换代；升级保留。
func TestStartInstallsListenWhenConfigured(t *testing.T) {
	a, i, f, out := listenEnv(t, true, true)
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	r := mustReceipt(t, i)
	if len(r.Services) != 4 || !slices.Contains(r.Services, a.Service.ListenLabel()) || !f.loaded[a.Service.ListenLabel()] {
		t.Fatalf("应装四个服务：%v", r.Services)
	}
	testkit.AssertContains(t, out.String(), "消息监听也装上了")
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	if mustReceipt(t, i).Generation != r.Generation {
		t.Fatal("都装好了不该换代")
	}
	// 升级不带 --listen 也保留
	i.Out = io.Discard
	if err := i.Setup(setup.SetupOptions{Scope: "full", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(mustReceipt(t, i).Services, a.Service.ListenLabel()) {
		t.Fatal("升级丢了监听")
	}

	// 先 start，之后才配 Telegram：再 start 补上
	a, i, f, _ = listenEnv(t, false, true)
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	if f.loaded[a.Service.ListenLabel()] {
		t.Fatal("还没配不该装")
	}
	testkit.WriteInstall(t, a.Telegram.TelegramEnvPath(), "TELEGRAM_BOT_TOKEN=x\nTELEGRAM_CHAT_ID=1\n", 0o600)
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	if !f.loaded[a.Service.ListenLabel()] {
		t.Fatal("配好后再 start 应补上监听")
	}
}

// 监听服务装失败：整个事务回滚，paused 不改，不说开了。
func TestStartListenFailureKeepsPaused(t *testing.T) {
	a, i, f, out := listenEnv(t, true, true)
	before := testkit.ReadFile(t, configPath(a))
	fail := func(args ...string) ([]byte, error) {
		if args[0] == "bootstrap" && strings.HasSuffix(args[len(args)-1], ".listen.plist") {
			return []byte("Bootstrap failed: 5"), errors.New("exit status 5")
		}
		return f.run(args...)
	}
	i.Launchctl, a.Service.Launchctl = fail, fail
	err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i})
	if err == nil {
		t.Fatal("装失败应报错")
	}
	testkit.AssertContains(t, err.Error(), "没装上")
	testkit.AssertBytes(t, configPath(a), before)
	if r := mustReceipt(t, i); len(r.Services) != 0 {
		t.Fatalf("应回滚：%v", r.Services)
	}
	if strings.Contains(out.String(), "后台已打开") {
		t.Fatal("没开就不该说开了")
	}
}

// setup --listen：skills 范围、没带 --services 都拒绝。
func TestSetupListenRequiresServices(t *testing.T) {
	i, o := installFixture(t)
	o.Listen = true
	if err := i.Setup(o); err == nil || !strings.Contains(err.Error(), "--services") {
		t.Fatalf("应拒绝：%v", err)
	}
	if exists(i.ReceiptPath()) {
		t.Fatal("拒绝时不该装")
	}
	o.Scope = "skills"
	if err := i.Setup(o); err == nil || !strings.Contains(err.Error(), "full scope") {
		t.Fatalf("应拒绝：%v", err)
	}
}

// notify setup：凭据配好就打开推送（只改这一个键），指路到 start，start 不用再多一步。
func TestNotifySetupEnablesNotifyWhenConfigured(t *testing.T) {
	const creds = "TELEGRAM_BOT_TOKEN=x\nTELEGRAM_CHAT_ID=1\n"
	t.Run("没配凭据：只给步骤，不改配置", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		before := testkit.ReadFile(t, configPath(a))
		if err := a.NotifySetup(); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, configPath(a), before)
		testkit.AssertContains(t, out.String(), "BotFather", "再运行一次 agentq notify setup")
		if strings.Contains(out.String(), "推送已打开") {
			t.Fatal("没配好不该说打开了")
		}
	})
	t.Run("配好了：只改 notify_enabled，不再讲步骤", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		custom := writeCustomConfig(t, a, nil)
		testkit.WriteInstall(t, a.Telegram.TelegramEnvPath(), creds, 0o600)
		if err := a.NotifySetup(); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, configPath(a), wantKeysSet(t, custom, "notify_enabled", "true"))
		if !a.Cfg.NotifyEnabled {
			t.Fatal("内存里的配置也要跟上")
		}
		testkit.AssertContains(t, out.String(), "推送已打开", "agentq start")
		if strings.Contains(out.String(), "BotFather") {
			t.Fatalf("配好了不用再讲步骤：%s", out)
		}
	})
	t.Run("已经打开：一个字节都不动", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		custom := writeCustomConfig(t, a, func(s string) string {
			return strings.Replace(s, `"notify_enabled": false`, `"notify_enabled": true`, 1)
		})
		testkit.WriteInstall(t, a.Telegram.TelegramEnvPath(), creds, 0o600)
		if err := a.NotifySetup(); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, configPath(a), custom)
	})
	t.Run("脚本写好凭据：打开", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		testkit.WriteInstall(t, filepath.Join(a.DataDir(), "secrets", "telegram-setup.sh"),
			"umask 077\nprintf 'TELEGRAM_BOT_TOKEN=x\\nTELEGRAM_CHAT_ID=1\\n' > \""+a.Telegram.TelegramEnvPath()+"\"\n", 0o700)
		if err := a.NotifySetup(); err != nil {
			t.Fatal(err)
		}
		if cfg := startReloadCfg(t, a); !cfg.NotifyEnabled {
			t.Fatal("应打开推送")
		}
		testkit.AssertContains(t, out.String(), "agentq notify test", "agentq start")
	})
	t.Run("脚本失败：不改", func(t *testing.T) {
		a, _, _, _ := onboardApp(t)
		before := testkit.ReadFile(t, configPath(a))
		testkit.WriteInstall(t, filepath.Join(a.DataDir(), "secrets", "telegram-setup.sh"), "exit 3\n", 0o700)
		if err := a.NotifySetup(); err == nil {
			t.Fatal("脚本失败应报错")
		}
		testkit.AssertBytes(t, configPath(a), before)
	})
	t.Run("凭据不能用：不改，说原因", func(t *testing.T) {
		a, _, _, out := onboardApp(t)
		before := testkit.ReadFile(t, configPath(a))
		testkit.WriteInstall(t, a.Telegram.TelegramEnvPath(), creds, 0o644)
		if err := a.NotifySetup(); err != nil {
			t.Fatal(err)
		}
		testkit.AssertBytes(t, configPath(a), before)
		testkit.AssertContains(t, out.String(), "chmod 600")
		if strings.Contains(out.String(), "推送已打开") {
			t.Fatal("不能用不该说打开了")
		}
	})
	t.Run("打开后 start 一起装监听", func(t *testing.T) {
		a, i, f, _ := listenEnv(t, true, false)
		if err := a.NotifySetup(); err != nil {
			t.Fatal(err)
		}
		if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
			t.Fatal(err)
		}
		if !f.loaded[a.Service.ListenLabel()] {
			t.Fatal("应装上监听")
		}
	})
}

// 服务缺失的提示按安装方式写：受管安装写 start（install 会拒绝），旧版只装二进制的写 install。
func TestServiceInstallCommand(t *testing.T) {
	a, _, _, _ := onboardApp(t)
	if got := a.Service.ServiceInstallCommand(); got != "agentq start" {
		t.Errorf("受管安装：%s", got)
	}
	bare := wireApp(&App{Env: &env.Env{Home: t.TempDir()}})
	if got := bare.Service.ServiceInstallCommand(); got != "agentq install" {
		t.Errorf("旧版安装：%s", got)
	}
}

// 夜班、复盘的定点服务被卸下：start 能装回来（巡逻提示就是让人跑 start）。
func TestStartRestoresRoutineServices(t *testing.T) {
	a, i, f, _ := onboardApp(t)
	if err := a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	label := a.Service.RoutineLabel(config.KindNight)
	if !f.loaded[label] {
		t.Fatal("夜班服务应已装上")
	}
	f.loaded[label] = false
	if err := a.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatal(err)
	}
	if !f.loaded[label] {
		t.Fatal("被卸下的定点服务应装回来")
	}
}

// 配置文件不存在时跑普通命令：写出的配置和安装器新建的一致（暂停、权限 default、后台开关全关）。
func TestMissingConfigIsWrittenLikeInstaller(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []config.Config{a.Cfg, startReloadCfg(t, a)} {
		if !cfg.Paused || cfg.PermissionMode != "default" || cfg.NightEnabled || cfg.ReviewEnabled || cfg.PatrolEnabled || cfg.PatrolAnalyze || cfg.NotifyEnabled {
			t.Fatalf("不是安全默认：%+v", cfg)
		}
	}
	// 和安装器同一份：只有 claude_path 可能不同
	i, o := installFixture(t)
	mustInstall(t, i, o)
	installed, _, err := setup.ReadInstallConfig(i.Home)
	if err != nil {
		t.Fatal(err)
	}
	installed.ClaudePath = a.Cfg.ClaudePath
	got, _ := json.Marshal(a.Cfg)
	want, _ := json.Marshal(installed)
	if string(got) != string(want) {
		t.Fatalf("和安装器不一致：\n%s\n%s", got, want)
	}
	// resume 时配置不见了：也按这一份写，只有 paused 听命令的
	if err := os.Remove(configPath(a)); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPaused(configPath(a), false); err != nil {
		t.Fatal(err)
	}
	if cfg := startReloadCfg(t, a); cfg.Paused || cfg.PermissionMode != "default" || cfg.NotifyEnabled {
		t.Fatalf("%+v", cfg)
	}
}

// 已有配置一个字节都不动；缺的键（老配置）在内存里仍按原来的默认算。
func TestExistingConfigUntouchedAndKeepsLegacyDefaults(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".agentq", "config.json")
	legacy := "{\n  \"projects_root\": \"~/projects\",\n  \"mine\":   1\n}\n"
	testkit.WriteInstall(t, p, legacy, 0o600)
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	testkit.AssertBytes(t, p, legacy)
	if a.Cfg.Paused || a.Cfg.PermissionMode != "bypassPermissions" || !a.Cfg.NotifyEnabled {
		t.Fatalf("老配置缺的键语义不该变：%+v", a.Cfg)
	}
}

// 汇报：列出排着的条目；没有待做时告诉下一步。
func TestStartReport(t *testing.T) {
	a, _, _, out := onboardApp(t)
	a.Onboard.StartReport()
	testkit.AssertContains(t, out.String(), "没有待做", "agentq add", "agentq status", "agentq pause")

	dir := onboardRepo(t, a, "rep", true)
	if err := a.Onboard.CmdInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := a.Onboard.CmdAdd([]string{"能接的 —— 细节"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Onboard.CmdAdd([]string{"第二条"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	a.Onboard.StartReport()
	testkit.AssertContains(t, out.String(), "rep「能接的」", "rep「第二条」")
	if strings.Contains(out.String(), "没有待做") {
		t.Fatal("有待做")
	}
}

// doctor 和 run：权限是 default 时各给一行提示；完全权限时不提。
func TestPermissionHintInDoctorAndRun(t *testing.T) {
	a, i, _, out := onboardApp(t)
	var report bytes.Buffer
	i.Out = &report
	if err := i.Doctor(); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, report.String(), setup.PermissionDefaultHint)
	if err := a.CmdRun("nope", ""); err == nil {
		t.Fatal("项目不存在应报错")
	}
	testkit.AssertContains(t, out.String(), setup.PermissionDefaultHint)

	writeCustomConfig(t, a, func(s string) string {
		return strings.Replace(s, `"permission_mode": "default"`, `"permission_mode": "bypassPermissions"`, 1)
	})
	report.Reset()
	out.Reset()
	if err := i.Doctor(); err != nil {
		t.Fatal(err)
	}
	if err := a.CmdRun("nope", ""); err == nil {
		t.Fatal("项目不存在应报错")
	}
	if strings.Contains(report.String(), "写不了文件") || strings.Contains(out.String(), "写不了文件") {
		t.Fatalf("完全权限不该提示：%s %s", report.String(), out)
	}
}

func TestIsTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if onboard.IsTerminal(null) {
		t.Fatal("/dev/null 不是终端")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if onboard.IsTerminal(r) {
		t.Fatal("管道不是终端")
	}
}

// 没有 python3 也能装，只给一行警告。依据：调度器和必需技能都不调 Python，只有可选 Waza 辅助脚本用。
func TestSetupWorksWithoutPython(t *testing.T) {
	i, o := installFixture(t)
	bin := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	i.Out = &out
	mustInstall(t, i, o)
	testkit.AssertContains(t, out.String(), "Warning: python3 not found")
}

// version 不读写配置；--version 同义；多余参数报错。
func TestVersionCommand(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{{"version"}, {"--version"}} {
		cmd := exec.Command(agentqBin, args...)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v：%v %s", args, err, out)
		}
		if !strings.HasPrefix(string(out), "agentq dev") {
			t.Fatalf("%v：%s", args, out)
		}
	}
	if exists(filepath.Join(home, ".agentq")) {
		t.Fatal("version 不该建 .agentq")
	}
	extra := exec.Command(agentqBin, "version", "extra")
	extra.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	_ = extra.Run()
	if code := extra.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("多余参数应退出 2：%d", code)
	}
}

// 用法第一屏先讲上手的命令。
func TestUsageShowsOnboardingFirst(t *testing.T) {
	help := exec.Command(agentqBin, "help")
	help.Env = append(os.Environ(), "HOME="+t.TempDir())
	b, err := help.Output()
	if err != nil {
		t.Fatal(err)
	}
	usage := string(b)
	first := usage[:strings.Index(usage, "agentq tick")]
	for _, want := range []string{"agentq init", "agentq add", "agentq start", "agentq version"} {
		if !strings.Contains(first, want) {
			t.Errorf("%s 应排在 tick 前面", want)
		}
	}
}
