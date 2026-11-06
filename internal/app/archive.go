package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// BACKLOG 瘦身（agentq backlog archive）：每个任务都要读 BACKLOG，「已完成」的旧条目移到 docs/BACKLOG-archive.md。
const (
	archiveGitWait = 30 * time.Second
)

// archiveBlocked 返回不能归档的原因（BACKLOG 有未提交改动、有任务占用），可以归档返回空。stale 是过期的进行中标记条数。
func (a *App) archiveBlocked(ctx context.Context, st *state.State, p bl.Project, backlog string) (why string, stale int) {
	for _, j := range st.Tasks() {
		if j.Project == p.Name {
			return fmt.Sprintf("任务 %s 正在跑", j.ID), 0
		}
	}
	now := a.Now()
	for _, e := range bl.BacklogEntries(backlog) {
		if e.Done || !strings.Contains(e.Text, "（进行中") {
			continue
		}
		if !bl.StaleMark(e.Text, now) {
			return fmt.Sprintf("「%s」标着进行中", textfmt.TruncRunes(e.Title, 40)), 0
		}
		stale++
	}
	ctx, cancel := context.WithTimeout(ctx, archiveGitWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", p.Dir, "status", "--porcelain", "--", "docs/BACKLOG.md", "docs/BACKLOG-archive.md")
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("git status 失败，当作未知：%v", err), stale
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "BACKLOG 有未提交的改动", stale
	}
	return "", stale
}

// archiveProject 处理一个项目；dryRun 只打印，不拒绝也不写文件。返回拒绝或失败的原因。
func (a *App) archiveProject(ctx context.Context, st *state.State, p bl.Project, dryRun bool) error {
	raw, err := os.ReadFile(p.BacklogPath())
	if err != nil {
		return fmt.Errorf("读 BACKLOG：%w", err)
	}
	old, err := os.ReadFile(p.ArchivePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读归档文件：%w", err)
	}
	g := &gitx.GitRepo{Dir: p.Dir, Commits: map[string]gitx.GitInfo{}}
	plan := bl.PlanArchive(string(raw), string(old), func(h string) (time.Time, bool) {
		c := g.Commit(h)
		return c.Time, c.OK
	}, a.Now())
	why, stale := a.archiveBlocked(ctx, st, p, string(raw))
	text := fmt.Sprintf("%s：「已完成」保留 %d 条，移走 %d 条，BACKLOG %d → %d 字节", p.Name, plan.Kept, plan.Moved, len(raw), len(plan.Backlog))
	if stale > 0 {
		text += fmt.Sprintf("；%d 个进行中标记超过 %s，当作过期", stale, textfmt.FormatDuration(bl.ArchiveMarkFresh))
	}
	if dryRun {
		if why != "" {
			text += "；实际执行会被拒绝：" + why
		}
		fmt.Fprintln(a.Out, text+"（--dry-run，没有改文件）")
		return nil
	}
	if why != "" {
		return fmt.Errorf("%s：拒绝归档：%s", p.Name, why)
	}
	if plan.Moved > 0 {
		// 先写归档再写 BACKLOG：中途失败时条目不会丢，归档文件的未提交改动也会挡住下一次运行
		if err := fsx.WriteFileAtomic(p.ArchivePath(), []byte(plan.Archive)); err != nil {
			return fmt.Errorf("%s：写归档文件：%w", p.Name, err)
		}
		if err := fsx.WriteKeepMode(p.BacklogPath(), []byte(plan.Backlog)); err != nil {
			return fmt.Errorf("%s：写 BACKLOG：%w", p.Name, err)
		}
	}
	fmt.Fprintln(a.Out, text+"（没有自动提交）")
	return nil
}

// CmdBacklog：agentq backlog archive <项目名|--all> [--dry-run]。
func (a *App) CmdBacklog(ctx context.Context, args []string) error {
	var name string
	var all, dryRun bool
	for _, x := range args[1:] {
		switch {
		case x == "--dry-run":
			dryRun = true
		case x == "--all":
			all = true
		case strings.HasPrefix(x, "-") || name != "":
			return fmt.Errorf("backlog archive：不认识的参数 %q", x)
		default:
			name = x
		}
	}
	if args[0] != "archive" || all == (name != "") {
		return errors.New("用法：agentq backlog archive <项目名|--all> [--dry-run]")
	}
	if !dryRun {
		lock, err := a.Runner.LockForCommand()
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	st, err := state.Load(a.StatePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出，当作未知，不归档：%w", err)
	}
	projects, err := a.Backlog.AllProjects()
	if err != nil {
		return fmt.Errorf("列项目：%w", err)
	}
	var failed []string
	found := false
	for _, p := range projects {
		if all {
			if _, err := os.Stat(p.BacklogPath()); errors.Is(err, os.ErrNotExist) || a.Backlog.Excluded(p.Name) {
				continue
			}
		} else if p.Name != name {
			continue
		}
		found = true
		if err := a.archiveProject(ctx, st, p, dryRun); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if !all && !found {
		return fmt.Errorf("%s 下没有项目 %s", a.ProjectsRoot(), name)
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "\n"))
	}
	return nil
}
