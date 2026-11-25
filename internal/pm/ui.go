package pm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/gate"
	"agentq/internal/textfmt"
)

const (
	// uiSkill 是界面这一路对照的技能，审查表从装好的技能里取，不在这里另抄一份。
	uiSkill = "no-ai-look"
	// checklistMax：审查表进提示词的上限。
	checklistMax = 3000
	// ShotsDir 是起调研前截的图在任务目录里的位置，跟任务目录按保留期一起清；uiWorkDir 放截图脚本的输出和一次性 worktree。
	ShotsDir  = "screenshots"
	uiWorkDir = "ui"
)

// shotExts 是算截图的图片后缀。
var shotExts = []string{".png", ".jpg", ".jpeg", ".webp"}

// uiOf：项目有界面就给界面这一路的材料，没有是 nil。有界面 = 有截图命令 .agentq/ui，或 git 管的文件里有界面文件（和界面门同一份后缀）。
func (a *Service) uiOf(p backlog.Project) *UI {
	files := a.tracked(p)
	_, err := os.Stat(filepath.Join(p.Dir, gate.UIScript))
	if err != nil && !slices.ContainsFunc(files, gate.IsUIFile) {
		return nil
	}
	return &UI{Checklist: a.checklist(p), Shots: shots(files)}
}

// captureUI：项目默认分支上有 .agentq/ui 就先跑一次，截现在的界面给调研看。截图放进这次调研的任务目录，所以先分好任务号，返回它；没跑是空的。
// 超时和失败照界面门的规矩（gate.CaptureUI，超时用 verify_timeout）。没截到不挡调研：原因进 ui.Missed，提示词里写明，日志和当天汇总各记一句。
// tick 拿着锁等它跑完：调研只在又做完一版、待做清空时起，截图脚本一般一两分钟。
func (a *Service) captureUI(p backlog.Project, ui *UI) string {
	branch, err := a.Gitx.DefaultBranch(p.Dir)
	if err != nil {
		if _, serr := os.Stat(filepath.Join(p.Dir, gate.UIScript)); serr == nil {
			a.missed(p, ui, "", "认不出默认分支："+textfmt.OneLine(err.Error(), 160))
		}
		return ""
	}
	id, err := a.Runner.NewJobID(p.Name)
	if err != nil {
		a.missed(p, ui, "", "分不了任务目录："+textfmt.OneLine(err.Error(), 160))
		return ""
	}
	dir := a.JobDir(id)
	c, ran := gate.CaptureUI(context.Background(), gate.CaptureOpts{Home: a.Home, Project: p.Dir, Branch: branch,
		WorkDir: filepath.Join(dir, uiWorkDir), Shots: filepath.Join(dir, ShotsDir), Mark: "pm-ui-" + id, Timeout: a.Cfg.VerifyTimeout.Duration, Tmps: a.Tmps()})
	if !ran {
		os.RemoveAll(dir)
		return ""
	}
	ui.Captured = c.Shots
	if c.Problem != "" {
		why := c.Problem
		if c.Log != "" {
			why += "（日志 " + a.Tilde(c.Log) + "）"
		}
		a.missed(p, ui, id, why)
	}
	if len(c.Shots) > 0 {
		a.Logf("产品经理：%s 起调研前在 %s 上截了 %d 张界面图，在 %s", p.Name, branch, len(c.Shots), a.Tilde(filepath.Join(dir, ShotsDir)))
	}
	return id
}

// missed 记下这次截图的问题：一张没截到的写「这次没截到图」，截到了但有不合格的写明哪几张。
func (a *Service) missed(p backlog.Project, ui *UI, id, why string) {
	ui.Missed = why
	msg := "产品经理：" + p.Name + " 这次没截到图：" + why
	if len(ui.Captured) > 0 {
		msg = "产品经理：" + p.Name + " 起调研前截的图里" + why
	}
	if id != "" {
		msg += "（调研任务 " + id + "）"
	}
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
}

// tracked：项目里 git 管的文件（相对项目目录）。读不了是空的，只少了按文件判断这一条。
func (a *Service) tracked(p backlog.Project) []string {
	out, err := a.Gitx.Git(p.Dir, "ls-files", "-z")
	if err != nil {
		a.Logf("产品经理：%s 列不出文件，只按 %s 判断有没有界面：%s", p.Name, gate.UIScript, textfmt.OneLine(out, 160))
		return nil
	}
	return strings.FieldsFunc(out, func(r rune) bool { return r == 0 })
}

// shots：项目里现成的界面截图。用户确认过的基线（docs/design/approved/）排前面，其次 docs/ 下和路径里带 screenshot、截图的图片。
func shots(files []string) []string {
	var base, other []string
	for _, f := range files {
		low := strings.ToLower(f)
		if !slices.ContainsFunc(shotExts, func(e string) bool { return strings.HasSuffix(low, e) }) {
			continue
		}
		switch {
		case strings.HasPrefix(f, gate.BaselineDir+"/"):
			base = append(base, f)
		case strings.HasPrefix(f, "docs/") || strings.Contains(low, "screenshot") || strings.Contains(f, "截图"):
			other = append(other, f)
		}
	}
	out := append(base, other...)
	return out[:min(len(out), shotsMax)]
}

// checklist 从这个项目用的后端装好的 no-ai-look 技能里取截图审查表那一节。调研进程只读工作目录，读不到技能文件，所以这里读好放进提示词。
func (a *Service) checklist(p backlog.Project) string {
	h, ok := a.BackendFor(a.Cfg.ProjectBackend(p.Name)).(backend.SkillHome)
	if !ok {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(h.SkillsDir(a.Home), uiSkill, "SKILL.md"))
	if err != nil {
		return ""
	}
	return ChecklistSection(string(b))
}

// ChecklistSection 取技能正文里标题带「审查表」的那一节（到下一个二级标题为止），没有是空的。
func ChecklistSection(skill string) string {
	var out []string
	in := false
	for _, l := range strings.Split(skill, "\n") {
		if strings.HasPrefix(l, "## ") {
			if in {
				break
			}
			in = strings.Contains(l, "审查表")
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return textfmt.TruncRunes(strings.TrimSpace(strings.Join(out, "\n")), checklistMax)
}

// noteUI：有界面的项目，界面这一路的结论记进当天汇总；没交的也记一句，用户看得到这一路漏了。
func (a *Service) noteUI(p backlog.Project, jobID, ui string, day time.Time) {
	if ui == "" && a.uiOf(p) == nil {
		return
	}
	msg := "产品经理：" + p.Name + " 界面这一路：" + ui
	if ui == "" {
		msg = "产品经理：" + p.Name + " 的调研（任务 " + jobID + "）没交界面这一路的结论"
	}
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(day, msg)
}
