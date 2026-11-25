package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// CaptureOpts 是在默认分支上截一次现在的界面（产品经理调研前用，不是过门）。
type CaptureOpts struct {
	// Home 是 git 用的 HOME，Project 是项目主目录，Branch 是默认分支。
	Home, Project, Branch string
	// WorkDir 放一次性 worktree（跑完删掉）、脚本输出 ui.log 和 pgid。
	WorkDir string
	// Shots 是截图目录，$AGENTQ_ARTIFACTS 指这里，跑完留着。
	Shots string
	// Mark 是脚本环境里 AGENTQ_JOB 的值。
	Mark    string
	Timeout time.Duration
	// Tmps 登记脚本的临时目录，同 ScriptOpts。
	Tmps proc.TmpStore
}

// UICapture 是截图的结果。
type UICapture struct {
	// Shots 是截出来的 PNG（绝对路径，排好序，最多 maxShotFiles 张），脚本没过时是空的。
	Shots []string
	// Problem：没截到的原因，或有截图不合格（解不开、空白）；都好是空的。
	Problem string
	// Log 是脚本输出的位置。
	Log string
}

// CaptureUI 在默认分支上跑一次 .agentq/ui。规矩和界面门一样：只跑默认分支上带可执行权限的那份，在一次性 worktree 里跑，
// 独立进程组、超时就终止，退出码 0 算过，PNG 要解得开、不是空白。默认分支上没有 .agentq/ui 时返回 false，什么都不做。
func CaptureUI(ctx context.Context, o CaptureOpts) (res UICapture, ran bool) {
	git := func(dir string, args ...string) (string, error) { return runGit(ctx, o.Home, dir, nil, args...) }
	out, err := git(o.Project, "ls-tree", o.Branch, "--", UIScript)
	if err != nil {
		return UICapture{Problem: "查不了 " + o.Branch + " 上的 " + UIScript + "：" + textfmt.OneLine(err.Error(), 200)}, true
	}
	if out == "" {
		return UICapture{}, false
	}
	if mode, _, _ := strings.Cut(out, " "); mode != "100755" {
		return UICapture{Problem: o.Branch + " 上的 " + UIScript + " 没有可执行权限，没跑"}, true
	}
	res.Log = filepath.Join(o.WorkDir, "ui.log")
	for _, d := range []string{o.WorkDir, o.Shots} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			res.Problem = "建不了目录：" + textfmt.OneLine(err.Error(), 200)
			return res, true
		}
	}
	wt := filepath.Join(o.WorkDir, "wt")
	if _, err := git(o.Project, "worktree", "add", "--detach", "--quiet", wt, o.Branch); err != nil {
		res.Problem = "建 worktree 失败：" + textfmt.OneLine(err.Error(), 200)
		return res, true
	}
	defer func() {
		if err := RemoveWorktree(o.Project, wt); err != nil { // 截图不受影响，留下的检出在 git worktree list 里看得到
			res.Problem = strings.TrimPrefix(res.Problem+"；", "；") + "一次性 worktree " + wt + " 没清掉：" + textfmt.OneLine(err.Error(), 200)
		}
	}()
	defer dropTmps(o.Tmps, o.Mark)
	r := RunScript(ctx, ScriptOpts{Script: filepath.Join(wt, UIScript), Dir: wt, Log: res.Log, Timeout: o.Timeout, WorkDir: o.WorkDir, Mark: o.Mark, Env: []string{ArtifactsEnv + "=" + o.Shots}, Tmps: o.Tmps})
	switch r.Status {
	case ScriptPassed:
	case ScriptFailed:
		res.Problem = UIScript + " 没过：" + r.Note
		return res, true
	default:
		res.Problem = UIScript + " 判不了：" + textfmt.OneLine(r.Note, 200)
		return res, true
	}
	shots, problems, err := checkShots(o.Shots)
	if err != nil {
		res.Problem = "读不了截图：" + textfmt.OneLine(err.Error(), 200)
		return res, true
	}
	if len(shots) == 0 {
		res.Problem = UIScript + " 退出码 0，但 $" + ArtifactsEnv + " 里没有 PNG 截图"
		return res, true
	}
	if len(problems) > 0 {
		res.Problem = fmt.Sprintf("%d 张截图有问题：%s", len(problems), textfmt.OneLine(strings.Join(problems, "；"), 300))
	}
	res.Shots = absShots(o.Shots, shots)
	return res, true
}

// CapturedShots 是截图目录里的 PNG（绝对路径，排好序，最多 maxShotFiles 张）；目录不在或读不了是空的。
func CapturedShots(dir string) []string {
	shots, err := pngFiles(dir)
	if err != nil {
		return nil
	}
	return absShots(dir, shots)
}

func absShots(dir string, rel []string) []string {
	out := make([]string, 0, min(len(rel), maxShotFiles))
	for _, s := range rel[:min(len(rel), maxShotFiles)] {
		out = append(out, filepath.Join(dir, s))
	}
	return out
}
