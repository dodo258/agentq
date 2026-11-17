package release

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CHANGELOG.md 的固定格式：「## 未发布」（或「## Unreleased」）一节里每条以「- 」开头，续行缩进；
// 只修了问题的条目以「修复：」（或「Fix:」）开头。发版时这一节的条目挪到「## <版本>」下，「## 未发布」留空。

var (
	unreleasedHead = regexp.MustCompile(`^##\s+(未发布|Unreleased)\s*$`)
	versionRe      = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	fixEntry       = regexp.MustCompile(`^(修复[：:]|(?i:fix)[：:])`)
)

// section 找「## 未发布」一节：标题行号和下一个二级标题（或文件末尾）的行号。没有这一节 head 是 -1。
func section(lines []string) (head, end int) {
	head, end = -1, len(lines)
	for i, l := range lines {
		if head < 0 {
			if unreleasedHead.MatchString(strings.TrimRight(l, "\r")) {
				head = i
			}
			continue
		}
		if strings.HasPrefix(l, "## ") {
			return head, i
		}
	}
	return head, end
}

// Unreleased：「## 未发布」一节里的条目（续行并进上一条，去掉开头的「- 」）。没有这一节或没有条目返回空。
func Unreleased(text string) []string {
	lines := strings.Split(text, "\n")
	head, end := section(lines)
	if head < 0 {
		return nil
	}
	var out []string
	for _, l := range lines[head+1 : end] {
		switch t := strings.TrimSpace(l); {
		case strings.HasPrefix(l, "- "):
			out = append(out, strings.TrimSpace(l[2:]))
		case t != "" && len(out) > 0 && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")):
			out[len(out)-1] += " " + t
		}
	}
	return out
}

// NextVersion：last 是上一次发布的 tag（vX.Y.Z，空表示还没发过）。条目全是「修复：」开头的升补丁号，否则升小版本号。
func NextVersion(last string, entries []string) (string, error) {
	if len(entries) == 0 {
		return "", errors.New("「未发布」里没有条目")
	}
	var v [3]int
	if last != "" {
		m := versionRe.FindStringSubmatch(last)
		if m == nil {
			return "", fmt.Errorf("上一次发布的 tag %q 不是 vX.Y.Z，认不出下一个版本号", last)
		}
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
	}
	allFix := true
	for _, e := range entries {
		allFix = allFix && fixEntry.MatchString(e)
	}
	if allFix && last != "" {
		v[2]++
	} else {
		v[1], v[2] = v[1]+1, 0
	}
	return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]), nil
}

// Stamp：把「## 未发布」下的内容挪到新的「## <版本>」标题下，「## 未发布」留空。没有这一节或没有条目时报错。
func Stamp(text, version string) (string, error) {
	if len(Unreleased(text)) == 0 {
		return "", errors.New("CHANGELOG.md 没有「## 未发布」一节或里面没有条目")
	}
	lines := strings.Split(text, "\n")
	head, _ := section(lines)
	out := append([]string{}, lines[:head+1]...)
	out = append(out, "", "## "+version)
	return strings.Join(append(out, lines[head+1:]...), "\n"), nil
}
