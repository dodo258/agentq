package chat

import (
	"errors"
	"fmt"
	"strings"

	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/textfmt"
)

// ActiveProject：项目必须是 projects_root 下或 extra_projects 登记的项目。名字只和扫描结果比较，路径穿越进不来。
func (a *Service) ActiveProject(name string) (backlog.Project, error) {
	if a.Backlog.Excluded(name) {
		return backlog.Project{}, fmt.Errorf("项目 %q 在 exclude_projects 里，自动流程不碰；要做就在终端运行 agentq run", textfmt.OneLine(name, 40))
	}
	projects, err := a.Backlog.ActiveProjects()
	for _, p := range projects {
		if p.Name == name {
			return p, nil
		}
	}
	if err != nil {
		return backlog.Project{}, err
	}
	return backlog.Project{}, fmt.Errorf("没有项目 %q", textfmt.OneLine(name, 40))
}

// decide：必须精确匹配一条「等用户决定」，还在等条件的不算。把决定写进条目、移到「待做」顶部并提交，tick 接着做。
func (a *Service) decide(project, fingerprint, decision string) error {
	p, err := a.ActiveProject(project)
	if err != nil {
		return err
	}
	choice := backlog.CleanTodoText(decision, 300)
	if choice == "" {
		return errors.New("decision 是空的")
	}
	all, _ := a.Notify.Decisions()
	var hit []notify.Decision
	for _, d := range notify.Askable(all) {
		if d.Project == project && d.Fingerprint == fingerprint {
			hit = append(hit, d)
		}
	}
	if len(hit) != 1 {
		return fmt.Errorf("「等用户决定」里匹配到 %d 条，要正好 1 条", len(hit))
	}
	return a.Backlog.CommitBacklog(p, "待办："+textfmt.TruncRunes(hit[0].Title, 40)+"（用户拍板）", func(old string) (string, error) {
		text, ok := MoveDecision(old, hit[0].Title, choice)
		if !ok {
			return "", errors.New("BACKLOG 里找不到这一条或没有「## 待做」")
		}
		return text, nil
	})
}

// MoveDecision 把「等用户决定」里标题为 title 的条目移到「待做」第一条之前，末尾写上用户的决定。
func MoveDecision(text, title, decision string) (string, bool) {
	lines := strings.Split(text, "\n")
	section, from, todo := "", -1, -1
	var cm backlog.CommentTracker
	first := -1
	for i, line := range lines {
		l := strings.TrimRight(line, "\r")
		if cm.Skip(l) {
			continue
		}
		if strings.HasPrefix(l, "## ") {
			section = strings.TrimSpace(l[3:])
			if strings.HasPrefix(section, "待做") && todo < 0 {
				todo = i
			}
			continue
		}
		if strings.HasPrefix(section, "待做") && first < 0 && strings.HasPrefix(l, "- [") {
			first = i
		}
		if backlog.IsDecisionSection(section) && from < 0 && strings.HasPrefix(l, "- [ ]") &&
			notify.DecisionTitle(strings.TrimSpace(strings.TrimPrefix(l, "- [ ]"))) == title {
			from = i
		}
	}
	if from < 0 || todo < 0 {
		return text, false
	}
	at := first
	if at < 0 {
		at = todo + 1
	}
	// 选项用不着了（决定写在后面）：行内的去掉，下一行缩进写的连行一起去掉
	moved := notify.StripDecideOptions(strings.TrimRight(lines[from], "\r")) + "（用户决定：" + decision + "）"
	optionLines := map[int]bool{}
	for i := from + 1; i < len(lines) && (strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t")); i++ {
		if notify.DecideOptionsLineRe.MatchString(strings.TrimSpace(lines[i])) {
			optionLines[i] = true
		}
	}
	var out []string
	for i, line := range lines {
		if i == at {
			out = append(out, moved)
		}
		if i != from && !optionLines[i] {
			out = append(out, line)
		}
	}
	if at >= len(lines) {
		out = append(out, moved)
	}
	return strings.Join(out, "\n"), true
}
