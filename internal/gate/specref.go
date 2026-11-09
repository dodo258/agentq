package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// 条目依据的方案：审查进程要读到方案里定下的设计决定，照方案做的不当问题提。
// 条目写了 .md 路径就用写的（在候选目录里真有的普通文件）；只写「按方案第几节」没写路径的，退回 docs/specs/ 下最新的几份（文件名带日期，按名字倒排）。
// 这次改动里的文件（changed，含新加的）都不算方案：被审的分支改出来的东西不能拿来引导审查。没改的文件在候选目录里就是基点上的那份。

const (
	maxSpecRefs  = 5
	fallbackSpec = 3
	specDir      = "docs/specs"
)

var mdPathRe = regexp.MustCompile("[A-Za-z0-9_./\\-]+\\.md")

func specRefs(dir, item string, changed []string) []string {
	var refs []string
	for _, m := range mdPathRe.FindAllString(item, -1) {
		p := filepath.ToSlash(filepath.Clean(m))
		if filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.HasSuffix(p, "BACKLOG.md") || slices.Contains(refs, p) || slices.Contains(changed, p) {
			continue
		}
		if regularFile(filepath.Join(dir, p)) {
			refs = append(refs, p)
		}
		if len(refs) == maxSpecRefs {
			return refs
		}
	}
	if len(refs) > 0 || !strings.Contains(item, "方案") {
		return refs
	}
	entries, err := os.ReadDir(filepath.Join(dir, specDir))
	if err != nil {
		return nil
	}
	for i := len(entries) - 1; i >= 0 && len(refs) < fallbackSpec; i-- { // ReadDir 按名字升序
		e := entries[i]
		if p := specDir + "/" + e.Name(); strings.HasSuffix(e.Name(), ".md") && !slices.Contains(changed, p) && regularFile(filepath.Join(dir, p)) {
			refs = append(refs, p)
		}
	}
	return refs
}

func regularFile(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// specLine 是提示词里的一行；没有方案就是空的。ui 为真时连提示也不给照方案做的设计。
func specLine(c Candidate, ui bool) string {
	refs := specRefs(c.Dir, c.Item, c.Files)
	if len(refs) == 0 {
		return ""
	}
	s := fmt.Sprintf("条目依据的方案（当前目录里的文件）：%s。先读和这次改动有关的那几节：照方案定下的做法做的不算问题；实现背离方案的才报，写明背离了方案哪一节。", strings.Join(refs, "、"))
	if ui {
		s += "方案定下的颜色、元素、版式和用词，照着做了就不提意见，hint 也不写。"
	}
	return s + "\n"
}
