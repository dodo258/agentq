package gate

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// 轻量流程：改动小、局部、好撤回时，调模型的两道门（审查、界面审查）换便宜一档的模型，强度不降（审查至少 medium），确定性的门照跑。
// 11-08 官网试用：一条只改一段 CSS 的小改动，门调模型用掉的和任务本身一样多。判断只看 git，不调模型。

const (
	lightMaxFiles = 3  // 不算测试和记录文件，最多改这么多个文件
	lightMaxLines = 60 // 不算测试和记录文件，新增加删除最多这么多行
)

// lightFlow 返回走不走轻量流程和理由。diff 是 --unified=0 的基点到候选的改动。
func lightFlow(c Candidate, risk Risk, diff string) (bool, string) {
	if risk != RiskLocal {
		return false, "改错的代价是「" + riskLabel(risk) + "」"
	}
	if c.Type == "rewrite" {
		return false, "类型是 rewrite"
	}
	if list := instructionFiles(c.Files); len(list) > 0 {
		return false, "改了给模型看的指令文件：" + listFiles(list)
	}
	var code []string
	for _, f := range c.Files {
		if strings.HasPrefix(f, ".agentq/") && f != ReadyFile {
			return false, "改了门脚本或门配置：" + f
		}
		if counted(f) {
			code = append(code, f)
		}
	}
	lines := changedLines(diff)
	switch {
	case len(code) > lightMaxFiles:
		return false, fmt.Sprintf("改了 %d 个文件（轻量最多 %d 个，不算测试和记录文件）", len(code), lightMaxFiles)
	case lines > lightMaxLines:
		return false, fmt.Sprintf("改了 %d 行（轻量最多 %d 行，不算测试和记录文件）", lines, lightMaxLines)
	}
	return true, fmt.Sprintf("局部可撤回，改了 %d 个文件、%d 行（不算测试和记录文件）", len(code), lines)
}

// counted：算进改动量的文件。测试、记录文件和 ready.json 不算。
func counted(f string) bool {
	return f != ReadyFile && !isTestFile(f) && !slices.Contains(recordFiles, path.Base(f))
}

// changedLines 数 --unified=0 diff 里算进改动量的文件的增删行。文件头（diff --git 到第一个 @@）里的 ---、+++ 是路径，不是改动。
func changedLines(diff string) int {
	n, count, header := 0, false, false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			count, header = false, true
		case strings.HasPrefix(l, "@@"):
			header = false
		case header && strings.HasPrefix(l, "--- "):
			f := strings.TrimPrefix(strings.TrimPrefix(l, "--- "), "a/")
			count = f != "/dev/null" && counted(f)
		case header && strings.HasPrefix(l, "+++ "):
			if f := strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/"); f != "/dev/null" { // 删掉的文件按「---」那行算
				count = counted(f)
			}
		case !header && count && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")):
			n++
		}
	}
	return n
}

// reviewModel：审查、界面审查用的模型。轻量时换 light_review_model（空就照 model）。强度不跟着降，一律 reviewEffort。
func reviewModel(model, light string, c Candidate) string {
	if c.Light && light != "" {
		return light
	}
	return model
}
