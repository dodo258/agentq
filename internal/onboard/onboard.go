// Package onboard 是上手三件事：agentq init 把项目接入，add 加一条待做，start 打开后台。这里只做入口和白话提示：写 BACKLOG 走 backlog，装服务走 setup，改配置走 config，调度走 sched。
package onboard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/runner"
	"agentq/internal/sched"
	"agentq/internal/sense"
	"agentq/internal/service"
	"agentq/internal/setup"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Service 是上手命令的模块实例。
type Service struct {
	*env.Env
	Backlog  *backlog.Service
	Gitx     *gitx.Service
	Sched    *sched.Service
	Sense    *sense.Service
	Service  *service.Service
	Telegram *telegram.Service
	// Assets 是编译进二进制的 skills、templates、support，embed 在根目录，由组装层注入。
	Assets fs.FS
}

// AddTextMax：一条待做最多多少字，和 Router、Telegram 写待做的上限一致。
const AddTextMax = 800

// OnboardFiles 是 init 负责补齐的文件：模板在二进制里，缺哪个生成哪个。
var OnboardFiles = []struct{ Template, Path string }{
	{"templates/BACKLOG.md", filepath.Join("docs", "BACKLOG.md")},
	{"templates/AGENTS.md", "AGENTS.md"},
	{"templates/PROGRESS.md", "PROGRESS.md"},
	{"templates/DECISIONS.md", "DECISIONS.md"},
	{"templates/LESSONS.md", "LESSONS.md"},
}

// VerifyRel 是测试门跑的检查命令的位置，和 gate.VerifyScript 同一个文件。onboard 不依赖 gate，TestInitWritesVerifyScript 对得上。
const VerifyRel = ".agentq/verify"

// CmdInit：args 是可选的目录，默认当前目录。不满足条件时报具体哪条和怎么改，不替用户动仓库。
func (a *Service) CmdInit(args []string) error {
	target := "."
	if len(args) == 1 {
		if strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("agentq init 不认识 %s；用法：agentq init [目录]", args[0])
		}
		target = args[0]
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("找不到目录 %s：%w", target, err)
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("找不到目录 %s：%w", target, err)
	}
	p, err := a.checkOnboardable(dir)
	if err != nil {
		return err
	}
	unlock, err := a.Backlog.LockMain(p.Name+"-main", backlog.MainLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	created, kept, err := a.scaffold(p)
	if err != nil {
		return err
	}
	if len(created) == 0 {
		fmt.Fprintf(a.Out, "%s 已经接入，没有要生成的文件。\n", p.Name)
	} else {
		fmt.Fprintf(a.Out, "已接入 %s：生成并提交了 %s\n", p.Name, strings.Join(created, "、"))
		if len(kept) > 0 {
			fmt.Fprintf(a.Out, "已有的没动：%s\n", strings.Join(kept, "、"))
		}
	}
	a.warnBacklog(p)
	if a.Backlog.Excluded(p.Name) {
		fmt.Fprintf(a.Out, "注意：%s 在 exclude_projects 里，tick 不会接它的活。\n", p.Name)
	}
	fmt.Fprintln(a.Out, `下一步：agentq add "要做的事"`)
	return nil
}

// checkOnboardable 按顺序查：是 git 主目录、在默认分支上、在 projects_root 下一层。错误里写清楚怎么改。
func (a *Service) checkOnboardable(dir string) (backlog.Project, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return backlog.Project{}, fmt.Errorf("找不到目录 %s：%w", dir, err)
	}
	if !fi.IsDir() {
		return backlog.Project{}, fmt.Errorf("%s 不是目录", dir)
	}
	root := a.ProjectsRoot()
	realRoot, rootErr := filepath.EvalSymlinks(root)
	if rootErr == nil && dir == realRoot {
		return backlog.Project{}, a.projectIsRootError(root)
	}
	if err := a.checkGitMain(dir); err != nil {
		return backlog.Project{}, err
	}
	if err := a.checkDefaultBranch(dir); err != nil {
		return backlog.Project{}, err
	}
	if (rootErr != nil || filepath.Dir(dir) != realRoot) && a.Cfg.ExtraProjectDir(filepath.Base(dir)) != dir {
		return backlog.Project{}, a.notInRootError(root, dir)
	}
	name := filepath.Base(dir)
	if strings.HasPrefix(name, ".") {
		return backlog.Project{}, fmt.Errorf("目录名 %s 以 . 开头，调度器会跳过它；改个名字再运行 agentq init", name)
	}
	return backlog.Project{Name: name, Dir: dir}, nil
}

// projectIsRootError：目录就是 projects_root 本身。是仓库的话，多半是把项目 mv 成了这个名字，说清怎么恢复。
func (a *Service) projectIsRootError(root string) error {
	if !gitx.IsGitMain(root) {
		return fmt.Errorf("%s 是放项目的目录，不是项目。到它下面的项目目录里运行 agentq init", root)
	}
	q, tmp := fsx.ShQuote(root), fsx.ShQuote(root+".agentq-tmp")
	return fmt.Errorf("%s 就是 projects_root，不能同时是项目。多半是把项目 mv 成了这个名字。恢复（my-project 换成项目原来的名字）：\n  mv %s %s && mkdir -p %s && mv %s %s/my-project\n然后到 %s/my-project 运行 agentq init（agentq 不替你移动目录）",
		root, q, tmp, q, tmp, q, root)
}

// notInRootError：项目不在 projects_root 下一层。移动命令要先建好目录、写明目标名字：
// 目录不存在时 mv 会把项目改名成 projects_root，目标已有同名目录时 mv 会把它移进去变成子目录，这两种都不给。
func (a *Service) notInRootError(root, dir string) error {
	name := filepath.Base(dir)
	target := filepath.Join(root, name)
	useParent := fmt.Sprintf("把 %s 里的 projects_root 改成 %s", a.ConfigPath(), fsx.ShQuote(filepath.Dir(dir)))
	const orAdd = `不想动目录：在项目里直接运行 agentq add "要做的事"，它会登记到 extra_projects。`
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("agentq init 只接 %s 下一层的项目，%s 不在那里，%s 下已有同名的 %s。先给项目换个名字再移进去，或者%s。改好后重新运行 agentq init（agentq 不替你移动目录）",
			root, dir, root, name, useParent)
	}
	return fmt.Errorf("agentq init 只接 %s 下一层的项目，%s 不在那里。二选一：\n  mkdir -p %s && mv %s %s\n  或%s\n改好后到新位置重新运行 agentq init（agentq 不替你移动目录）。%s",
		root, dir, fsx.ShQuote(root), fsx.ShQuote(dir), fsx.ShQuote(target), useParent, orAdd)
}

func (a *Service) checkGitMain(dir string) error {
	if gitx.IsGitMain(dir) {
		return nil
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s 是 worktree 或子模块（.git 是文件），到仓库主目录运行 agentq init", dir)
	}
	if top, err := a.Gitx.Git(dir, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		return fmt.Errorf("%s 在 git 仓库 %s 里面，不是它的主目录；到 %s 运行 agentq init", dir, top, top)
	}
	return fmt.Errorf("%s 还不是 git 仓库。agentq 不替你建，先执行：\n  git -C %s init -b main\n然后重新运行 agentq init", dir, fsx.ShQuote(dir))
}

// checkDefaultBranch：调度器只在默认分支上提交和合并（commitBacklog 同样要求），当前不在就说清楚该执行哪条。
func (a *Service) checkDefaultBranch(dir string) error {
	q := fsx.ShQuote(dir)
	def, err := a.Gitx.DefaultBranch(dir)
	br, headErr := a.Gitx.Git(dir, "symbolic-ref", "--short", "HEAD")
	switch {
	case errors.Is(err, gitx.ErrNoDefaultBranch):
		return a.noDefaultBranchError(dir, br, headErr)
	case err != nil:
		return err
	case headErr != nil:
		return fmt.Errorf("%s 的 HEAD 不在任何分支上；先执行：\n  git -C %s switch %s\n然后重新运行 agentq init", dir, q, def)
	case br == def:
		return nil
	}
	return fmt.Errorf("%s 当前在 %s 分支，默认分支是 %s。先执行：\n  git -C %s switch %s\n然后重新运行 agentq init（agentq 不替你改分支）", dir, br, def, q, def)
}

// noDefaultBranchError：origin/HEAD、main、master 都没有。agentq 不猜默认分支是哪个，给两种改法。
func (a *Service) noDefaultBranchError(dir, br string, headErr error) error {
	q := fsx.ShQuote(dir)
	if headErr != nil {
		return fmt.Errorf("%s 的 HEAD 不在任何分支上，也认不出默认分支（origin/HEAD、本地 main、master 都没有）。先执行：\n  git -C %s switch -c main\n然后重新运行 agentq init（agentq 不替你改分支）", dir, q)
	}
	fix := fmt.Sprintf("git -C %s branch -m %s main", q, fsx.ShQuote(br))
	if _, err := a.Gitx.Git(dir, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		fix = fmt.Sprintf("git -C %s checkout -b main", q) // 还没有提交的仓库
	} else if _, err := a.Gitx.Git(dir, "remote", "get-url", "origin"); err == nil {
		fix += fmt.Sprintf("\n  或者有远程时让它说了算：git -C %s remote set-head origin -a", q)
	}
	return fmt.Errorf("%s 当前在 %s 分支，认不出默认分支（origin/HEAD、本地 main、master 都没有）。先执行：\n  %s\n然后重新运行 agentq init（agentq 不替你改分支）", dir, br, fix)
}

// scaffold 生成缺的文件并只提交它们；已有的不动，主目录里别的改动不碰。失败时把这次生成的都撤掉。调用方拿着 <项目>-main 锁。
func (a *Service) scaffold(p backlog.Project) (created, kept []string, err error) {
	docsDir := filepath.Join(p.Dir, "docs")
	_, docsErr := os.Stat(docsDir)
	madeDocs := errors.Is(docsErr, os.ErrNotExist)
	verifyDir := filepath.Dir(filepath.Join(p.Dir, VerifyRel))
	_, verifyDirErr := os.Stat(verifyDir)
	madeVerifyDir := errors.Is(verifyDirErr, os.ErrNotExist)
	undo := func() {
		if len(created) > 0 {
			if _, e := a.Gitx.Git(p.Dir, append([]string{"rm", "--cached", "-q", "--ignore-unmatch", "--"}, created...)...); e != nil {
				fmt.Fprintf(a.Out, "撤销暂存失败，留意 git status：%v\n", e)
			}
		}
		for _, rel := range created {
			if e := os.Remove(filepath.Join(p.Dir, rel)); e != nil {
				fmt.Fprintf(a.Out, "撤销 %s 失败：%v\n", rel, e)
			}
		}
		if madeDocs {
			os.Remove(docsDir) // 只有空目录才删得掉；别的进程放了文件就留着
		}
		if madeVerifyDir {
			os.Remove(verifyDir)
		}
	}
	for _, f := range OnboardFiles {
		path := filepath.Join(p.Dir, f.Path)
		if _, statErr := os.Lstat(path); statErr == nil {
			kept = append(kept, f.Path)
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			undo()
			return nil, nil, fmt.Errorf("查看 %s 失败：%w", path, statErr)
		}
		data, readErr := fs.ReadFile(a.Assets, f.Template)
		if readErr != nil {
			undo()
			return nil, nil, fmt.Errorf("读模板 %s 失败：%w", f.Template, readErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			undo()
			return nil, nil, fmt.Errorf("建目录失败：%w", mkErr)
		}
		if wErr := os.WriteFile(path, data, 0o644); wErr != nil {
			undo()
			return nil, nil, fmt.Errorf("写 %s 失败：%w", path, wErr)
		}
		created = append(created, f.Path)
	}
	wroteVerify, verifySteps, verifyErr := a.scaffoldVerify(p)
	if verifyErr != nil {
		undo()
		return nil, nil, verifyErr
	}
	if wroteVerify {
		created = append(created, VerifyRel)
	} else if _, err := os.Lstat(filepath.Join(p.Dir, VerifyRel)); err == nil {
		kept = append(kept, VerifyRel)
	}
	if len(created) == 0 {
		return nil, kept, nil
	}
	if out, addErr := a.Gitx.Git(p.Dir, append([]string{"add", "--"}, created...)...); addErr != nil {
		undo()
		return nil, nil, fmt.Errorf("git add 失败，生成的文件已撤掉（要接入的文件不能被 .gitignore 忽略）：%v %s", addErr, textfmt.OneLine(out, 300))
	}
	if out, commitErr := a.Gitx.Git(p.Dir, append([]string{"commit", "-q", "-m", "接入 agentq：生成待办和交接文件", "--"}, created...)...); commitErr != nil {
		undo()
		return nil, nil, fmt.Errorf("提交失败，生成的文件已撤掉（没配 git 用户名时先执行 git config --global user.name 和 user.email）：%v %s", commitErr, textfmt.OneLine(out, 300))
	}
	switch {
	case wroteVerify:
		fmt.Fprintf(a.Out, "合并前过门跑的检查命令：%s（写在 %s，可以改）\n", verifySummary(verifySteps), VerifyRel)
	case !slices.Contains(kept, VerifyRel):
		fmt.Fprintf(a.Out, "没判断出检查命令。要加就提交一个可执行的 %s，不然第一个任务要自己补，补不上门判不了、不合并。\n", VerifyRel)
	}
	return created, kept, nil
}

// scaffoldVerify：还没有 .agentq/verify 时按项目文件判断检查命令写一份。判断不了、路径被 .gitignore 忽略都不写：测试门只认提交进去的脚本。
func (a *Service) scaffoldVerify(p backlog.Project) (bool, []verifyStep, error) {
	path := filepath.Join(p.Dir, VerifyRel)
	if _, err := os.Lstat(path); err == nil {
		return false, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, nil, fmt.Errorf("查看 %s 失败：%w", path, err)
	}
	steps := detectVerify(p.Dir)
	if len(steps) == 0 {
		return false, nil, nil
	}
	if _, err := a.Gitx.Git(p.Dir, "check-ignore", "-q", "--", VerifyRel); err == nil {
		fmt.Fprintf(a.Out, "%s 被 .gitignore 忽略，没写检查命令；测试门只跑提交进去的脚本。\n", VerifyRel)
		return false, nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, nil, fmt.Errorf("建目录失败：%w", err)
	}
	if err := os.WriteFile(path, []byte(verifyScript(steps)), 0o755); err != nil {
		return false, nil, fmt.Errorf("写 %s 失败：%w", path, err)
	}
	return true, steps, nil
}

// warnBacklog：已有的 BACKLOG 不覆盖，但用不了的情况要现在说，别等 add 时才发现。
func (a *Service) warnBacklog(p backlog.Project) {
	b, err := os.ReadFile(p.BacklogPath())
	if err != nil {
		fmt.Fprintf(a.Out, "注意：读不了 %s：%v\n", p.BacklogPath(), err)
		return
	}
	if text, ok := backlog.InsertTodo(string(b), "- [ ] x"); !ok || text == string(b) {
		fmt.Fprintf(a.Out, "注意：%s 里没有可用的「## 待做」一节，手动加上后 agentq 才能接活。\n", p.BacklogPath())
	}
	rel, err := backlog.BacklogRepoPath(p)
	if err != nil {
		fmt.Fprintf(a.Out, "注意：%v\n", err)
		return
	}
	if out, err := a.Gitx.Git(p.Dir, "status", "--porcelain", "--", rel); err == nil && out != "" {
		fmt.Fprintf(a.Out, "注意：%s 有没提交的改动，提交后 agentq add 才能写。\n", rel)
	}
}

// parseAddArgs 解析 add 的参数：一段需求加可选的 --project，先后都行；需求以 - 开头时写在 -- 后面。
func parseAddArgs(args []string) (text, project string, err error) {
	seenText := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			if i != len(args)-2 || seenText {
				return "", "", errors.New(`-- 后面只跟一段需求；用法：agentq add "<需求>" [--project 名字]`)
			}
			return args[i+1], project, nil
		case arg == "--project":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", "", errors.New("--project 后面要写项目名")
			}
			i++
			project = args[i]
		case strings.HasPrefix(arg, "--project="):
			project = strings.TrimPrefix(arg, "--project=")
			if project == "" {
				return "", "", errors.New("--project 后面要写项目名")
			}
		case strings.HasPrefix(arg, "-"):
			return "", "", fmt.Errorf(`agentq add 不认识 %s；用法：agentq add "<需求>" [--project 名字]（需求以 - 开头时写在 -- 后面）`, textfmt.OneLine(arg, 40))
		case seenText:
			return "", "", errors.New(`需求要写成一段，用引号包起来：agentq add "<需求>"`)
		default:
			text, seenText = arg, true
		}
	}
	if !seenText {
		return "", "", errors.New(`没写需求；用法：agentq add "<需求>" [--project 名字]`)
	}
	return text, project, nil
}

// cleanAddText 校验并压成一行，走和其他写待做入口相同的 cleanTodoText。多留一个字，才分得出超长。
func cleanAddText(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", errors.New("需求不是合法的 UTF-8 文字")
	}
	for _, r := range raw {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return "", errors.New("需求里有控制字符")
		}
	}
	text := backlog.CleanTodoText(raw, AddTextMax+1)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return "", errors.New("需求是空的")
	case n > AddTextMax:
		return "", fmt.Errorf("需求超过 %d 字；细节多就写进文件，这里写一句话和文件路径", AddTextMax)
	}
	return text, nil
}

// addTarget 找要写的项目：--project 优先，否则看当前目录属于哪个项目（子目录、worktree 都算）。不在 projects_root 下一层的仓库也算，register 为真表示要登记进 extra_projects。
func (a *Service) addTarget(project string) (p backlog.Project, register bool, err error) {
	if project != "" {
		// 明确点名的项目，exclude_projects 里的也能加（只是 tick 不接），所以不走 routerProject。
		dir := a.ProjectDir(project)
		if project != filepath.Base(project) || strings.HasPrefix(project, ".") || !gitx.IsGitMain(dir) {
			return p, false, fmt.Errorf("%s 下没有 git 项目 %q，extra_projects 里也没有。cd 进项目目录或换个 --project 名字；新项目到它的目录运行 agentq add 或 agentq init", a.ProjectsRoot(), textfmt.OneLine(project, 40))
		}
		return backlog.Project{Name: project, Dir: dir}, false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return p, false, fmt.Errorf("读不了当前目录：%w", err)
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return p, false, fmt.Errorf("读不了当前目录：%w", err)
	}
	if realRoot, err := filepath.EvalSymlinks(a.ProjectsRoot()); err == nil {
		rel, relErr := filepath.Rel(realRoot, realCwd)
		if relErr == nil && rel == "." {
			return p, false, fmt.Errorf("当前目录是放项目的 %s，不是项目。cd 进项目目录或加 --project 名字", a.ProjectsRoot())
		}
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			name, _, _ := strings.Cut(rel, string(filepath.Separator))
			if dir := filepath.Join(a.ProjectsRoot(), name); !strings.HasPrefix(name, ".") && gitx.IsGitMain(dir) {
				return backlog.Project{Name: name, Dir: dir}, false, nil
			}
		}
	}
	return a.repoOutsideRoot(realCwd)
}

// repoOutsideRoot：当前目录所在的仓库不在 projects_root 下一层。不是 Git 仓库的不接管，不替用户建。
func (a *Service) repoOutsideRoot(cwd string) (p backlog.Project, register bool, err error) {
	common, gitErr := a.Gitx.Git(cwd, "rev-parse", "--git-common-dir")
	if gitErr != nil {
		return p, false, fmt.Errorf("%s 不是 Git 仓库，agentq 不接管。要接管先执行：\n  git -C %s init -b main\n然后重新运行 agentq add 或 agentq init；也可以 cd 进项目目录或加 --project 名字", cwd, fsx.ShQuote(cwd))
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(cwd, common)
	}
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real
	}
	dir := filepath.Dir(common)
	if filepath.Base(common) != ".git" || !gitx.IsGitMain(dir) {
		return p, false, fmt.Errorf("%s 在裸仓库或子模块里，agentq 只接普通仓库的主目录", cwd)
	}
	// 家目录和 projects_root 是仓库（dotfiles 常见）时，往上找找到的是它们，不能把它们当项目。
	if realHome, err := filepath.EvalSymlinks(a.Home); err == nil && dir == realHome {
		return p, false, fmt.Errorf("%s 只在家目录这个仓库里，不算项目。cd 进项目目录或加 --project 名字", cwd)
	}
	if realRoot, err := filepath.EvalSymlinks(a.ProjectsRoot()); err == nil && dir == realRoot {
		return p, false, a.projectIsRootError(a.ProjectsRoot())
	}
	name := filepath.Base(dir)
	if strings.HasPrefix(name, ".") {
		return p, false, fmt.Errorf("目录名 %s 以 . 开头，调度器会跳过它；改个名字再运行 agentq add", name)
	}
	switch extra := a.Cfg.ExtraProjectDir(name); {
	case extra == dir:
		if taken := filepath.Join(a.ProjectsRoot(), name); a.ProjectDir(name) != dir {
			return p, false, sameNameError(name, taken, dir)
		}
		return backlog.Project{Name: name, Dir: dir}, false, nil
	case extra != "":
		return p, false, sameNameError(name, extra, dir)
	}
	if taken := filepath.Join(a.ProjectsRoot(), name); exists(taken) {
		return p, false, sameNameError(name, taken, dir)
	}
	return backlog.Project{Name: name, Dir: dir}, true, nil
}

// sameNameError：agentq 按目录名认项目（任务记录、并发限制、Telegram 都是），两个同名的分不开。
func sameNameError(name, taken, dir string) error {
	return fmt.Errorf("已经有同名的项目 %s（%s）。agentq 按目录名认项目，先给 %s 改个名字再运行 agentq add", name, taken, dir)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// ensureScaffold：第一次 add 时把项目接入。BACKLOG 已有就不碰别的文件。
func (a *Service) ensureScaffold(p backlog.Project) error {
	if _, err := os.Lstat(p.BacklogPath()); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := a.checkDefaultBranch(p.Dir); err != nil {
		return err
	}
	unlock, err := a.Backlog.LockMain(p.Name+"-main", backlog.MainLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	created, _, err := a.scaffold(p)
	if err != nil {
		return err
	}
	if len(created) > 0 {
		fmt.Fprintf(a.Out, "已接入 %s：生成并提交了 %s\n", p.Name, strings.Join(created, "、"))
	}
	return nil
}

// registerExtra 把项目记进 extra_projects，这个进程接下来也按新配置走。
func (a *Service) registerExtra(p backlog.Project) error {
	if err := config.AddExtraProject(a.ConfigPath(), p.Dir); err != nil {
		return fmt.Errorf("登记 extra_projects 失败：%w", err)
	}
	a.Cfg.ExtraProjects = append(a.Cfg.ExtraProjects, p.Dir)
	fmt.Fprintf(a.Out, "%s 不在 %s 下，已登记到 extra_projects，后台会看它。\n", p.Dir, a.ProjectsRoot())
	return nil
}

// CmdAdd 往「待做」末尾加一条并只提交 BACKLOG。同样的一条已在「待做」里时不重复加。第一次 add 时自动接入项目，不在 projects_root 下的自动登记。
func (a *Service) CmdAdd(args []string) error {
	raw, project, err := parseAddArgs(args)
	if err != nil {
		return err
	}
	text, err := cleanAddText(raw)
	if err != nil {
		return err
	}
	p, register, err := a.addTarget(project)
	if err != nil {
		return err
	}
	if err := a.ensureScaffold(p); err != nil {
		return err
	}
	if register {
		if err := a.registerExtra(p); err != nil {
			return err
		}
	}
	dup := false
	err = a.Backlog.CommitBacklogNamed(p, "待办："+textfmt.TruncRunes(backlog.ItemTitle(text), 40)+"（agentq add）", func(old string) (string, error) {
		for _, it := range backlog.ParseBacklog(old) {
			if strings.TrimSpace(backlog.InProgressRe.ReplaceAllString(it.Text, "")) == text {
				dup = true
				return old, nil
			}
		}
		edited, ok := backlog.InsertTodo(old, "- [ ] "+text)
		if !ok {
			return "", errors.New("BACKLOG 里没有「## 待做」一节")
		}
		return edited, nil
	})
	if err != nil {
		return err
	}
	if dup {
		fmt.Fprintf(a.Out, "%s 的「待做」里已经有这一条了，没重复加：%s\n", p.Name, textfmt.TruncRunes(text, 60))
		return nil
	}
	fmt.Fprintf(a.Out, "已加到 %s 的「待做」：%s\n", p.Name, textfmt.TruncRunes(text, 60))
	if a.Backlog.Excluded(p.Name) {
		fmt.Fprintf(a.Out, "%s 在 exclude_projects 里，tick 不会接；要做就运行 agentq run %s\n", p.Name, p.Name)
	} else if a.Cfg.Paused {
		fmt.Fprintln(a.Out, "后台还没开：运行 agentq start")
	}
	return nil
}

// StartOptions 是 start 除命令行参数外要的东西：是不是终端、从哪读回答。Installer、Getenv 只有测试注入，默认用真的。
type StartOptions struct {
	In          io.Reader
	Interactive bool
	Installer   *setup.Installer
	Getenv      func(string) string
}

// IsTerminal：标准输入是不是能回答问题的终端。/dev/null 也是字符设备，要单独排除。
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && !os.SameFile(fi, null)
}

// CmdStart 一步打开后台：检查 → 权限 → 服务 → 取消暂停 → 立刻 tick。可重复运行；前一步不过就不做后一步。
func (a *Service) CmdStart(args []string, o StartOptions) error {
	var acceptBypass, welcome, assumeYes bool
	for _, arg := range args {
		switch arg {
		case "--accept-bypass":
			acceptBypass = true
		case "--welcome":
			welcome = true
		case "--assume-yes":
			assumeYes = true
		default:
			return fmt.Errorf("agentq start 不认识 %s；用法：agentq start [--accept-bypass] 或 agentq start --welcome [--assume-yes]", textfmt.OneLine(arg, 40))
		}
	}
	if assumeYes && !welcome || acceptBypass && welcome {
		return errors.New("用法：agentq start [--accept-bypass] 或 agentq start --welcome [--assume-yes]")
	}
	i := o.Installer
	if i == nil {
		i = &setup.Installer{Assets: a.Assets, Home: a.Home, Launchctl: a.Service.Launchctl, Systemctl: a.Service.Systemctl, Loginctl: a.Service.Loginctl}
	}
	if welcome {
		return a.cmdWelcome(assumeYes, o, i)
	}
	if err := a.startChecks(i); err != nil {
		return err
	}
	method, err := a.ensureBypass(acceptBypass, o)
	if err != nil {
		return err
	}
	if method != "" {
		first := a.envFirstTime()
		if err := a.recordConsent(method); err != nil {
			return err
		}
		if first {
			a.autoEnableEnv()
		}
	}
	if err := a.EnsureServices(i); err != nil {
		return err
	}
	if err := a.Sched.CmdSetPaused(false); err != nil {
		return fmt.Errorf("服务已装上，但取消暂停失败：%w", err)
	}
	if err := a.startFirstTick(); err != nil {
		return err
	}
	a.StartReport()
	return nil
}

// startChecks 跑和 doctor 相同的检查；只读。输出留到失败时才给看。
func (a *Service) startChecks(i *setup.Installer) error {
	if err := setup.CheckClaudeConfigDir(a.Home); err != nil {
		return fmt.Errorf("检查没过，什么都没改：%w", err)
	}
	// 用户照提示自己装好 CLI 后再 start：配置里的 claude 找不到，但官方位置有，就记下真实路径。
	if a.usesDefaultCLIPath() {
		if _, err := setup.ProbeBackendEnv(a.Cfg.Backend, a.Home, a.Cfg.ClaudePath, setup.InstallEnv(a.Home)); errors.Is(err, backend.ErrNoCLI) {
			if _, path := a.probeEngine(); path != "" {
				if err := a.adoptCLIPath(path); err != nil {
					return err
				}
			}
		}
	}
	var report bytes.Buffer
	i.Out = &report
	if err := i.Doctor(); err != nil {
		help, canSetUp := a.cliHelp(), false
		if _, ok := a.cliSetup(); ok {
			canSetUp = true
		}
		if canSetUp && errors.Is(err, backend.ErrNoCLI) {
			return fmt.Errorf("检查没过，什么都没改：还没有 %s。先在终端运行下面两条，再运行 agentq start：\n  %s\n  %s", help.Name, help.Install, help.Login)
		}
		if canSetUp && errors.Is(err, backend.ErrNotLoggedIn) {
			return fmt.Errorf("检查没过，什么都没改：%s 还没登录。先在终端运行 %s，再运行 agentq start", help.Name, help.Login)
		}
		if s := strings.TrimSpace(report.String()); s != "" {
			fmt.Fprintln(a.Out, s)
		}
		return fmt.Errorf("检查没过，什么都没改：%w\n重新运行安装脚本可以修复或升级；agentq doctor 可以再查一遍", err)
	}
	r, err := i.Receipt()
	if err != nil {
		return fmt.Errorf("读安装记录失败，什么都没改：%w", err)
	}
	if r.Scope != "full" {
		return errors.New("现在只装了技能（--scope skills），没有后台程序；先 agentq remove --yes，再不带 --scope skills 重新安装")
	}
	return nil
}

// ensureBypass：配置里显式写了别的模式才问。没写这个键的是老配置，默认就是完全权限，不问也不改。返回这次同意的方式，没问就是空。
func (a *Service) ensureBypass(accept bool, o StartOptions) (string, error) {
	mode := a.Cfg.PermissionMode
	full := a.fullPermission()
	if mode == full {
		return "", nil
	}
	method := "accept-bypass"
	fmt.Fprintf(a.Out, "现在 permission_mode=%s。任务没人看着跑，要自己改文件、跑命令，所以必须用完全权限（%s）。\n", strconv.Quote(mode), full)
	fmt.Fprintln(a.Out, "完全权限下，任务能读写这个用户能碰到的所有文件，包括本地密钥（SSH、云账号、API key）。")
	fmt.Fprintln(a.Out, "建议用单独的系统用户或容器来跑。")
	if p, ok := a.DefaultBackend().(backend.Permissions); ok && p.PermissionNote() != "" {
		fmt.Fprintln(a.Out, p.PermissionNote())
	}
	if line := a.envConsentLine(); line != "" {
		fmt.Fprintln(a.Out, line)
	}
	manual := fmt.Sprintf("想手动设：把 %s 里的 permission_mode 改成 %s，再运行 agentq start。", a.ConfigPath(), full)
	switch {
	case accept:
		fmt.Fprintln(a.Out, "你带了 --accept-bypass，按同意处理。")
	case !o.Interactive:
		fmt.Fprintln(a.Out, manual)
		return "", errors.New("这里不是终端，没法问你，没改权限也没开后台。看过上面的说明后，带 --accept-bypass 重新运行")
	default:
		fmt.Fprint(a.Out, "改成完全权限吗？输入 y 同意 [y/N] ")
		answer, err := bufio.NewReader(o.In).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("读回答失败，没改权限也没开后台：%w", err)
		}
		fmt.Fprintln(a.Out)
		if s := strings.ToLower(strings.TrimSpace(answer)); s != "y" && s != "yes" {
			fmt.Fprintln(a.Out, "没改权限，后台也没开。"+manual)
			return "", env.ErrLogged
		}
		method = "prompt"
	}
	return method, a.setFullPermission()
}

// fullPermission 是任务无人值守要用的权限模式，每个后端自己声明。不声明的后端按 Claude 的写法存（后端自己决定怎么理解）。
func (a *Service) fullPermission() string {
	if p, ok := a.DefaultBackend().(backend.Permissions); ok {
		return p.FullPermission()
	}
	return "bypassPermissions"
}

func (a *Service) setFullPermission() error {
	full := a.fullPermission()
	if err := config.SetKey(a.ConfigPath(), "permission_mode", strconv.Quote(full), func() error {
		return fmt.Errorf("%s 不见了", a.ConfigPath())
	}); err != nil {
		return fmt.Errorf("改权限失败，后台没开：%w", err)
	}
	a.Cfg.PermissionMode = full
	return nil
}

// wantListen：配 Telegram 本身就是用户的选择，凭据配好且推送开着才装常驻监听；没配不装。
func (a *Service) wantListen() bool {
	c, err := a.Telegram.TelegramCreds()
	if err != nil {
		fmt.Fprintf(a.Out, "Telegram 配置读不了，先不装消息监听：%v\n", err)
		return false
	}
	if c == nil {
		return false
	}
	if !a.Cfg.NotifyEnabled {
		fmt.Fprintf(a.Out, "Telegram 配好了，但 notify_enabled=false，没装消息监听。要用就把 %s 里这一项改成 true，再运行 agentq start。\n", a.ConfigPath())
		return false
	}
	return true
}

// EnsureServices：要的服务都在跑就不动（也就不受运行中任务影响）；否则走 setup 同一个引擎，范围和已选的备份、看门狗、监听照旧。
// setup 不碰配置，夜班、复盘、巡逻的开关都不受影响。
func (a *Service) EnsureServices(i *setup.Installer) error {
	installed, err := a.ensureServices(i)
	if installed {
		fmt.Fprintln(a.Out, "调度服务已装上：每 5 分钟看一次待做。")
	}
	return err
}

// ensureServices 返回这次是不是真的装了服务，怎么说由调用方定。
func (a *Service) ensureServices(i *setup.Installer) (bool, error) {
	r, err := i.Receipt()
	if err != nil {
		return false, fmt.Errorf("读安装记录失败，后台没开：%w", err)
	}
	running := func(label string) bool { return slices.Contains(r.Services, label) && i.Loaded(label) }
	listen := a.wantListen()
	if running(a.Service.ServiceLabel()) && running(a.Service.RoutineLabel(config.KindNight)) && running(a.Service.RoutineLabel(config.KindReview)) && (!listen || running(a.Service.ListenLabel())) {
		return false, nil
	}
	var plan bytes.Buffer
	i.Out = &plan
	if err := i.Setup(setup.SetupOptions{Scope: r.Scope, Yes: true, Services: true, Listen: listen}); err != nil {
		if s := strings.TrimSpace(plan.String()); s != "" {
			fmt.Fprintln(a.Out, s)
		}
		return false, fmt.Errorf("调度服务没装上，后台没开：%w", err)
	}
	if r, err = i.Receipt(); err != nil {
		return false, fmt.Errorf("读安装记录失败：%w", err)
	}
	notes, err := i.Svc(a.Cfg).Svc().Doctor(r.Services)
	if err != nil {
		return false, fmt.Errorf("调度服务装上后检查没过：%w", err)
	}
	for _, n := range notes {
		fmt.Fprintln(a.Out, n)
	}
	if listen {
		fmt.Fprintln(a.Out, "Telegram 消息监听也装上了：发消息随到随回。")
	}
	return true, nil
}

// startFirstTick 立刻接一次活。服务刚装上时系统会马上跑一次 tick，等它放锁，不然这次会跳过，看不到接了什么。
func (a *Service) startFirstTick() error {
	lock, err := fsx.AcquireLock(a.LockPath(), runner.CmdLockWait)
	if err != nil {
		return fmt.Errorf("后台已打开，但这次没能立刻接活：%w", err)
	}
	if lock == nil {
		return fmt.Errorf("后台已打开，但等了 %s 还拿不到 %s，这次没立刻接活，下次定时 tick 会接", runner.CmdLockWait, a.LockPath())
	}
	lock.Close()
	if err := a.Sched.Tick(""); err != nil {
		return err
	}
	a.Sense.AfterTick()
	return nil
}

// StartReport 告诉用户接了哪几条、还排着哪几条、怎么看、怎么停。
func (a *Service) StartReport() {
	st, err := state.Load(a.StatePath())
	if err != nil {
		fmt.Fprintf(a.Out, "读不了状态，用 agentq status 看：%v\n", err)
		st = &state.State{}
	}
	tasks := st.Tasks()
	if len(tasks) > 0 {
		fmt.Fprintln(a.Out, "\n正在做：")
		for _, j := range tasks {
			fmt.Fprintf(a.Out, "  %s「%s」（任务 %s）\n", j.Project, textfmt.TruncRunes(j.Title, 60), j.ID)
		}
	}
	var queued []string
	if projects, err := a.Backlog.ActiveProjects(); err == nil {
		for _, p := range projects {
			items, err := p.Items()
			if err != nil {
				fmt.Fprintf(a.Out, "%s 的 BACKLOG 读不了：%v\n", p.Name, err)
				continue
			}
			for _, it := range items {
				if it.Eligible() && st.RunningFor(p.Name, it.Fingerprint) == nil {
					queued = append(queued, fmt.Sprintf("  %s「%s」", p.Name, textfmt.TruncRunes(it.Title, 60)))
				}
			}
		}
	}
	if len(queued) > 0 {
		fmt.Fprintln(a.Out, "\n接下来会接：")
		fmt.Fprintln(a.Out, strings.Join(queued, "\n"))
	}
	if len(tasks)+len(queued) == 0 {
		fmt.Fprintln(a.Out, "\n现在没有待做。到项目目录运行 agentq add \"要做的事\"，5 分钟内会接上。")
	}
	if limits := backend.Limits(a.DefaultBackend()); len(limits) > 0 {
		fmt.Fprintln(a.Out, "\n后端 "+a.DefaultBackend().Name()+" 缺这些能力（agentq doctor 看全表）：\n  "+strings.Join(limits, "\n  "))
	}
	for _, why := range a.Cfg.PermissionProblems() {
		fmt.Fprintln(a.Out, "\n"+why)
	}
	fmt.Fprintln(a.Out, "\n后台已打开。看进度：agentq status；暂停：agentq pause")
}
