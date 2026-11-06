// Package textfmt 把数值、时间、长文本格式化成给人看的短文字。
package textfmt

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// localPathRe：绝对路径或 ~/ 开头、至少两段的路径。路径可能带空格，不知道到哪结束，一直连到下一个标点或行尾：宁可遮多。一段的（/settings 这种页面路径）不算；前面要是开头、空白或标点，agent/x 这种分支名不算。
var localPathRe = regexp.MustCompile(`(^|[\s（(：:="'「，,；;])~?/[^/\n，。；：:）)（("'」,;]+(?:/[^\n，。；：:）)（("'」,;]*)+`)

// RedactPaths 把文字里的本机路径换成「<本机路径>」，路径后面到下一个标点的文字也算进去。提交进仓库的文字用，日志和给本人看的不用。
func RedactPaths(s string) string {
	return localPathRe.ReplaceAllString(s, "$1<本机路径>")
}

func FormatDuration(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// FmtTokens：1234 → 1.2K，6388024 → 6.39M。
func FmtTokens(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func DateCN(t time.Time) string { return fmt.Sprintf("%d月%d日", t.Month(), t.Day()) }

var weekdayCN = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// WhenCN：给人看的时间一律写成「10-13 周二 21:00」，星期由程序算好（推送、汇总里的时间模型只照抄，不自己换算）。
func WhenCN(t time.Time) string {
	return t.Format("01-02") + " " + weekdayCN[t.Weekday()] + " " + t.Format("15:04")
}

func HasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func FormatAge(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

func TruncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func ErrText(err error) string {
	if err == nil {
		return "没有凭据文件"
	}
	return OneLine(err.Error(), 150)
}

func HumanSize(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

var tagRe = regexp.MustCompile(`<[^>]*>`)

// HTMLToPlain 去掉 HTML 标签、还原实体，用于纯文本重发和本机通知。
func HTMLToPlain(s string) string { return html.UnescapeString(tagRe.ReplaceAllString(s, "")) }

// Esc 转义 HTML 模式里的 & < >（Telegram 只认这几个，加上 &quot;）。
func Esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func FormatIdle(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

func OneLine(s string, n int) string {
	return TruncRunes(strings.Join(strings.Fields(s), " "), n)
}

func FmtPct(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }
