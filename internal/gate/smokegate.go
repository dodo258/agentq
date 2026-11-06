package gate

import (
	"context"

	"agentq/internal/textfmt"
)

// 成品门：agentq new 的第一版和标了（成品）的条目才跑。项目脚本退出码 0 算过：构建产物在、能启动。
// 过了才有交付单（做了什么、怎么打开看，见 deliver）。

// SmokeScript 是项目的烟测命令。
const SmokeScript = ".agentq/smoke"

type smokeGate struct{ d Deps }

func (smokeGate) Name() string { return "smoke" }

func (smokeGate) AppliesTo(c Candidate) bool { return c.Product }

func (g smokeGate) Run(ctx context.Context, c Candidate) Result {
	first, note, err := locateScript(ctx, g.d, c, SmokeScript)
	if err != nil {
		return Result{Outcome: Unknown, Summary: "查不了 " + SmokeScript + "：" + textfmt.OneLine(err.Error(), 200)}
	}
	if note == "" {
		return Result{Outcome: Unknown, Summary: "没有能跑的 " + SmokeScript + "：成品要有一条命令检查构建产物在、能启动，退出码 0 算过"}
	}
	if err := prepareCandidate(ctx, g.d, c, first); err != nil {
		return Result{Outcome: Unknown, Summary: "还原 worktree 失败：" + textfmt.OneLine(err.Error(), 200)}
	}
	r := runProjectScript(ctx, g.d, c, SmokeScript, "smoke.log", nil)
	log := "日志 " + g.d.Tilde(c.WorkDir+"/smoke.log")
	switch r.Status {
	case ScriptPassed:
		return Result{Outcome: Pass, Summary: SmokeScript + " 过了", Evidence: []string{note, log}}
	case ScriptFailed:
		return Result{Outcome: Fail, Summary: SmokeScript + " 没过：" + r.Note, Evidence: []string{note, log}}
	}
	return Result{Outcome: Unknown, Summary: SmokeScript + " 判不了：" + textfmt.OneLine(r.Note, 200), Evidence: []string{note, log}}
}
