package backlog

import "strings"

// 引擎合并后写收尾（见 internal/gate）。

// MergedLine 是写进「已完成」的内容。
type MergedLine struct {
	Commit     string
	Acceptance string
}

// line 写一行「已完成」。条目原文留着：质量统计要看里面写的提交号，用户的决定也在正文里。
func (m MergedLine) line(text string) string {
	sep := " —— "
	if strings.Contains(text, "——") {
		sep = "；"
	}
	s := "- [x] " + text + sep + "合并提交 " + m.Commit
	if m.Acceptance != "" {
		s += "；验收：" + m.Acceptance
	}
	return s
}

// MarkMerged 把这个任务合并掉的条目从「待做」移到「已完成」最前面：指纹对得上的那一条，以及进行中或卡住标记写着同一分支的（巡逻写的接手待做和原条目共用分支）。
// 都没有时才按标题找，而且标题只对上一条才算：同标题的另一条可能还没做。条目已经不在「待做」（用户先移走了）也写一行记下合并提交。
// 这个提交已经记过就什么都不做，可以重复调用。
func MarkMerged(text, fp, title, branch string, m MergedLine) string {
	if strings.Contains(text, "合并提交 "+m.Commit) {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	targets := mergedTargets(lines, fp, title, branch)
	var out, moved []string
	for i := 0; i < len(lines); i++ {
		if !targets[i] {
			out = append(out, lines[i])
			continue
		}
		line := strings.TrimRight(lines[i], "\r\n")
		body := strings.TrimSpace(stuckMarkRe.ReplaceAllString(InProgressRe.ReplaceAllString(strings.TrimPrefix(line, "- [ ]"), ""), ""))
		moved = append(moved, m.line(body)+"\n")
		for i+1 < len(lines) && isContinuation(lines[i+1]) {
			i++
			moved = append(moved, lines[i])
		}
	}
	if len(moved) == 0 {
		moved = []string{m.line(title) + "\n"}
	}
	return insertDone(strings.Join(out, ""), strings.Join(moved, ""))
}

// mergedTargets 返回「待做」里要移走的行号。
func mergedTargets(lines []string, fp, title, branch string) map[int]bool {
	targets := map[int]bool{}
	var titled []int
	fpSeen := false
	section := ""
	var cm CommentTracker
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if cm.Skip(line) {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line[3:])
		}
		if !strings.HasPrefix(section, "待做") || !strings.HasPrefix(line, "- [ ]") {
			continue
		}
		bare := stuckMarkRe.ReplaceAllString(line, "")
		text := strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
		switch {
		case !fpSeen && Fingerprint(bare) == fp:
			fpSeen = true
			targets[i] = true
		case branch != "" && (MarkerBranch(text) == branch || stuckMarkHasBranch(text, branch)):
			targets[i] = true
		case ItemTitle(stuckMarkRe.ReplaceAllString(text, "")) == title:
			titled = append(titled, i)
		}
	}
	if len(targets) == 0 && len(titled) == 1 {
		targets[titled[0]] = true
	}
	return targets
}

// stuckMarkHasBranch：卡住标记写着这个分支（门和 agentq 自动放下时写的「，分支 x）」）。
func stuckMarkHasBranch(text, branch string) bool {
	return strings.Contains(stuckMarkRe.FindString(text), "分支 "+branch+"）")
}

// insertDone 插到「## 已完成」下面第一条的位置（归档按「前面的是新的」留条目）；没有这一节就加在最后。
// 文件尾巴卡在没关的注释里时加在注释前面：写进注释等于没写，下次还找不到这一节。
func insertDone(text, add string) string {
	lines := strings.SplitAfter(text, "\n")
	var cm CommentTracker
	openAt := -1
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		skip := cm.Skip(line)
		switch {
		case !cm.in:
			openAt = -1
		case openAt < 0:
			openAt = i
		}
		if skip || !strings.HasPrefix(line, "## ") || !strings.HasPrefix(strings.TrimSpace(line[3:]), "已完成") {
			continue
		}
		at := i + 1
		for at < len(lines) && strings.TrimSpace(lines[at]) == "" {
			at++
		}
		head := strings.Join(lines[:i+1], "")
		if !strings.HasSuffix(head, "\n") {
			head += "\n"
		}
		return head + "\n" + add + "\n" + strings.Join(lines[at:], "")
	}
	if openAt >= 0 {
		return strings.Join(lines[:openAt], "") + "\n## 已完成\n\n" + add + "\n" + strings.Join(lines[openAt:], "")
	}
	return strings.TrimRight(text, "\n") + "\n\n## 已完成\n\n" + add
}

// Reclaim 把条目上门写的卡住标记（任务 id 对得上才算）换回进行中。别的卡住标记不动：可能是用户写的。
func Reclaim(text, title, jobID, claim string) string {
	lines := strings.SplitAfter(text, "\n")
	section := ""
	var cm CommentTracker
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if cm.Skip(line) {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line[3:])
		}
		if !strings.HasPrefix(section, "待做") || !strings.HasPrefix(line, "- [ ]") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
		m := stuckMarkRe.FindString(body)
		if m == "" || !strings.Contains(m, "任务 "+jobID) || ItemTitle(stuckMarkRe.ReplaceAllString(body, "")) != title {
			continue
		}
		lines[i] = strings.TrimRight(strings.Replace(line, m, "", 1), " \t") + claim + raw[len(line):]
		return strings.Join(lines, "")
	}
	return text
}
