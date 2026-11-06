package gate

import (
	"context"
	"path/filepath"
	"strings"
)

// 门脚本（.agentq/verify、ui、smoke）的共同做法：跑默认分支上的版本，任务改不了门。
// 默认分支上没有、这次任务新增的，用任务的版本，并标给审查门看。

// locateScript 定用哪一份。first 为真：默认分支上没有能跑的，这次任务新增了，用任务的。两个都不是时 first 和 note 都是零值。
// 软链接可能指到项目外，没有可执行权限的不算有：都不跑。
func locateScript(ctx context.Context, d Deps, c Candidate, rel string) (first bool, note string, err error) {
	modeOf := func(rev string) (string, error) {
		out, err := d.Git(ctx, c.Dir, "ls-tree", rev, "--", rel)
		if err != nil {
			return "", err
		}
		mode, _, _ := strings.Cut(out, " ")
		return mode, nil
	}
	mode, err := modeOf(c.Base)
	if err != nil {
		return false, "", err
	}
	if mode == "100755" {
		return false, "用 " + c.DefaultBranch + " 上的 " + rel, nil
	}
	mode, err = modeOf(c.Merge)
	if err != nil {
		return false, "", err
	}
	if mode == "100755" {
		return true, "首次由任务新增 " + rel + "，用任务的版本：审查时看它是不是真的在查东西", nil
	}
	return false, "", nil
}

// prepareCandidate 把 worktree 还原成候选，再把 .agentq 换回默认分支上的（首次新增的留任务的）。
func prepareCandidate(ctx context.Context, d Deps, c Candidate, first bool) error {
	if _, err := d.Git(ctx, c.Dir, "reset", "-q", "--hard", c.Merge); err != nil {
		return err
	}
	if _, err := d.Git(ctx, c.Dir, "clean", "-q", "-f", "-d"); err != nil {
		return err
	}
	if first {
		return nil
	}
	if _, err := d.Git(ctx, c.Dir, "rm", "-rqf", "--ignore-unmatch", agentqDir); err != nil {
		return err
	}
	_, err := d.Git(ctx, c.Dir, "checkout", "-q", c.Base, "--", agentqDir)
	return err
}

// runProjectScript 在候选 worktree 里跑一个门脚本，输出写到过门目录的 log。
func runProjectScript(ctx context.Context, d Deps, c Candidate, rel, log string, env []string) ScriptResult {
	return d.Script(ctx, ScriptOpts{Script: filepath.Join(c.Dir, rel), Dir: c.Dir, Log: filepath.Join(c.WorkDir, log), Timeout: c.Timeout, WorkDir: c.WorkDir, Mark: gateMark(c.JobID), Env: env})
}

// restoreFromBase 把 path（文件或目录）换回基点的版本，基点没有的删掉。
func restoreFromBase(ctx context.Context, d Deps, c Candidate, path string) error {
	out, err := d.Git(ctx, c.Dir, "ls-tree", c.Base, "--", path)
	if err != nil {
		return err
	}
	if _, err := d.Git(ctx, c.Dir, "rm", "-rqf", "--ignore-unmatch", "--", path); err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	_, err = d.Git(ctx, c.Dir, "checkout", "-q", c.Base, "--", path)
	return err
}
