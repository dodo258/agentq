package patrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// 漏洞检查：每周一次用 govulncheck 的二进制模式查正在运行的 agentq（编进去的 Go 版本和依赖），不用源码，谁装的都能查。
// 没装 govulncheck 就跳过，说明怎么装；跑不成（没网、超时）记原因，上次的结果照旧算，下周再跑。
// 只算真用到了有漏洞的函数的（和 govulncheck 默认输出一样），按模块归成一个问题。
// 自我进化把它当 agentq 自己的问题：自己长大开着时写进 self_project 的待做去升级（internal/evolve）。
const (
	vulnEvery   = 7 * 24 * time.Hour
	vulnTimeout = 5 * time.Minute
	vulnCheck   = "vuln"
	vulnInstall = "go install golang.org/x/vuln/cmd/govulncheck@latest"
)

// VulnState 是上次漏洞检查的结果，存在 patrol.json：一周才跑一次，中间每小时巡逻照这个报。
type VulnState struct {
	Run      time.Time     `json:"run"`
	Note     string        `json:"note,omitempty"` // 没跑成的原因（没装、没网），跑成了为空
	Findings []VulnFinding `json:"findings,omitempty"`
}

// VulnFinding 是一个模块的已知漏洞。
type VulnFinding struct {
	Module  string   `json:"module"`
	Version string   `json:"version"`
	Fixed   string   `json:"fixed,omitempty"`
	IDs     []string `json:"ids"`
	Summary []string `json:"summary,omitempty"`
}

// vulnMsg 是 govulncheck -format json 输出里的一条消息（只取用得到的字段）。
type vulnMsg struct {
	OSV *struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"osv"`
	Finding *struct {
		OSV   string `json:"osv"`
		Fixed string `json:"fixed_version"`
		Trace []struct {
			Module   string `json:"module"`
			Version  string `json:"version"`
			Function string `json:"function"`
		} `json:"trace"`
	} `json:"finding"`
}

// parseVulns 按模块归并真用到了有漏洞函数的发现。
func parseVulns(r io.Reader) ([]VulnFinding, error) {
	dec := json.NewDecoder(r)
	summary := map[string]string{}
	byMod := map[string]*VulnFinding{}
	for {
		var m vulnMsg
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("govulncheck 的输出读不懂：%w", err)
		}
		if m.OSV != nil {
			summary[m.OSV.ID] = m.OSV.Summary
		}
		f := m.Finding
		if f == nil || len(f.Trace) == 0 || f.Trace[0].Function == "" {
			continue
		}
		t := f.Trace[0] // 第一帧是有漏洞的那个函数，往后是一路调用到它的
		v := byMod[t.Module]
		if v == nil {
			v = &VulnFinding{Module: t.Module, Version: t.Version}
			byMod[t.Module] = v
		}
		if f.Fixed != "" && (v.Fixed == "" || f.Fixed > v.Fixed) {
			v.Fixed = f.Fixed
		}
		if !slices.Contains(v.IDs, f.OSV) {
			v.IDs = append(v.IDs, f.OSV)
		}
	}
	var out []VulnFinding
	for _, v := range byMod {
		sort.Strings(v.IDs)
		for _, id := range v.IDs {
			if s := summary[id]; s != "" {
				v.Summary = append(v.Summary, id+"："+textfmt.OneLine(s, 120))
			}
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Module < out[b].Module })
	return out, nil
}

// runVulncheck 跑一次。返回的 note 非空是没跑成（没装、出错），findings 这时不可信。
func runVulncheck(ctx context.Context) ([]VulnFinding, string) {
	tool, err := exec.LookPath("govulncheck")
	if err != nil {
		return nil, "没装 govulncheck，跳过漏洞检查（装上：" + vulnInstall + "）"
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return nil, "找不到正在运行的 agentq，漏洞检查没跑：" + err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, vulnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, "-mode=binary", "-format", "json", self)
	cmd.Env = proc.ChildEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	if err := cmd.Run(); err != nil {
		return nil, "govulncheck 没跑成（" + err.Error() + "）：" + textfmt.OneLine(errb.String(), 200)
	}
	found, err := parseVulns(&out)
	if err != nil {
		return nil, err.Error()
	}
	return found, ""
}

// checkVuln：离上次满一周（或时钟往回拨过）就跑一次 govulncheck；按存下的结果每次都报。
func (a *Service) checkVuln(ps *PatrolState, now time.Time) []PatrolIssue {
	v := ps.Vuln
	if v == nil || now.Before(v.Run) || now.Sub(v.Run) >= vulnEvery {
		found, note := runVulncheck(context.Background())
		next := &VulnState{Run: now, Note: note, Findings: found}
		if note != "" { // 一周说一次，status 的巡逻那一行也带着
			a.Logf("每周漏洞检查：%s", note)
			if v != nil {
				next.Findings = v.Findings // 这次没查成：上次的结果照旧算，下周再跑
			}
		}
		ps.Vuln, v = next, next
	}
	var out []PatrolIssue
	for _, f := range v.Findings {
		who := f.Module
		if who == "stdlib" {
			who = "Go"
		}
		title := fmt.Sprintf("agentq 用的 %s %s 有已知漏洞 %s", who, f.Version, strings.Join(f.IDs, "、"))
		if f.Fixed != "" {
			title += "，" + f.Fixed + " 修了"
		}
		out = append(out, PatrolIssue{
			Fingerprint: vulnCheck + ":" + f.Module, Check: vulnCheck,
			Title:    title,
			Evidence: append([]string{"govulncheck -mode=binary（" + v.Run.Format("01-02 15:04") + " 查）"}, f.Summary...),
			Action:   "配了 self_project 且自己长大开着时写进它的待做去升级；发行版用 agentq update",
		})
	}
	return out
}

// VulnStatus：status 里巡逻那一行后面带的漏洞检查情况。没跑过是空的。
func (ps *PatrolState) VulnStatus() string {
	if ps.Vuln == nil {
		return ""
	}
	switch {
	case ps.Vuln.Note != "":
		return "漏洞检查（" + textfmt.WhenCN(ps.Vuln.Run) + "）：" + ps.Vuln.Note
	case len(ps.Vuln.Findings) == 0:
		return "漏洞检查（" + textfmt.WhenCN(ps.Vuln.Run) + "）：没有已知漏洞"
	}
	return fmt.Sprintf("漏洞检查（%s）：%d 个模块有已知漏洞", textfmt.WhenCN(ps.Vuln.Run), len(ps.Vuln.Findings))
}
