package runner

import (
	"errors"
	"fmt"
	"os/exec"

	"agentq/internal/backlog"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// BackupRemote：用户层备份钩子每次提交后 push --all 的远程。项目没有这个远程就不查。
const BackupRemote = "backup"

// BackupFork：分支已经推到 BackupRemote、本地分支却不是推上去那个提交的后代（amend、rebase、reset 改写了推过的提交）时，
// 返回一句给日志的话；没推过、本地没有这个分支、查不了时返回空。只看本地的远程跟踪引用（push 时 git 顺手更新），不联网。
func (a *Service) BackupFork(dir, branch string) string {
	if branch == "" {
		return ""
	}
	remote, err := a.Gitx.Git(dir, "rev-parse", "--verify", "--quiet", "refs/remotes/"+BackupRemote+"/"+branch+"^{commit}")
	if err != nil {
		return ""
	}
	local, err := a.Gitx.Git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil || local == remote {
		return ""
	}
	_, err = a.Gitx.Git(dir, "merge-base", "--is-ancestor", remote, local)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 { // 0 是后代；别的退出码是查不了
		return ""
	}
	return fmt.Sprintf("任务分支 %s 已推到 %s（%s），本地（%s）不是它的后代：推过的提交被 amend、rebase 或 reset 改写了，备份的 push --all 会被拒、整个仓库都推不上去。"+
		"要人看：确认本地是要的，再 git -C %s push --force-with-lease %s %s", branch, BackupRemote, textfmt.TruncRunes(remote, 7), textfmt.TruncRunes(local, 7), dir, BackupRemote, branch)
}

// TaskBranchFork：待做任务的分支（BACKLOG 进行中标记里写的，没有就是任务记下的卡住分支）和备份分叉时返回 BackupFork 那句话。
func (a *Service) TaskBranchFork(j *state.Job) string {
	if j.Kind != "" {
		return ""
	}
	branch := j.StuckBranch
	if items, err := a.jobItems(j); err == nil {
		if it, ok := stuckTarget(j, backlog.Project{Name: j.Project, Dir: j.ProjectDir}, items); ok && backlog.MarkerBranch(it.Text) != "" {
			branch = backlog.MarkerBranch(it.Text)
		}
	}
	return a.BackupFork(j.ProjectDir, branch)
}
