// Package patrol 是每小时一次的巡逻：纯脚本查服务、备份、任务、额度、报告、拍板条目和磁盘，新异常才起模型只读分析。
package patrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/runner"
	"agentq/internal/service"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Service 是巡逻的模块实例。
type Service struct {
	*env.Env
	Backlog  *backlog.Service
	Gitx     *gitx.Service
	Ledger   *ledger.Service
	Notify   *notify.Service
	Runner   *runner.Service
	Service  *service.Service
	Telegram *telegram.Service
	// DiskFree 为空时用 statfs 读磁盘剩余；AnalyzePatrol 为空时经任务外壳启动巡逻分析。测试里替换掉。
	DiskFree      func(path string) (free, total uint64, err error)
	AnalyzePatrol func(st *state.State, prompt string) (*state.Job, error)
}

// 巡逻：tick 每小时调用一次（agentq patrol 立刻执行一次），纯脚本把下面几类检查走一圈，不调用 claude：
// launchd 服务退出码、自动备份推送 FAIL、当天备份报告失败、agentq 任务失败/超时/中断/疑似卡住、订阅额度、
// 08:00 后缺早报和夜班报告、各项目「等用户决定」条目、磁盘剩余。每个问题带证据写进 ~/.agentq/patrol.json 和当天汇总。
// 按指纹记住报过的问题：问题还在就不重报，消失后再出现才重报。中断的任务写成待做放进对应项目 BACKLOG。
// 推给用户的只有 agentq 和项目自己的事（任务失败/卡住等）；基础设施类（infra）只写汇总，推送归看门狗
// （~/.claude/scripts/watchdog.py，不依赖 agentq，agentq 挂了也能报），免得同一件事推两遍。
// 有新异常时可选地启动一次 claude -p 只读分析原因（每天最多 patrolAnalyzeMax 次），结果在它结束时写进汇总。
const (
	patrolEvery      = time.Hour
	patrolWindow     = 24 * time.Hour // 自动推送 FAIL、结束的任务只看最近这么久
	patrolDiskMin    = 20 << 30       // 磁盘剩余低于这个就报
	patrolReportHour = 8              // 这个钟点之后还没有当天早报、夜班报告就报
	patrolAnalyzeMax = 3              // 每天最多启动几次 claude -p 分析
	PatrolJobName    = "巡逻分析"
)

// LaunchdPrefix：只查和 agentq 自己的服务同一前缀的 launchd 服务（服务名去掉最后一段），系统和别的软件的不管。
var LaunchdPrefix = config.LaunchdLabel[:strings.LastIndex(config.LaunchdLabel, ".")+1]

type PatrolIssue struct {
	Fingerprint string   `json:"fingerprint"`
	Check       string   `json:"check"` // launchd、autopush、backup、job、stall、usage、report、decide、disk
	Title       string   `json:"title"`
	Evidence    []string `json:"evidence"`
	// Action：已经怎么处理、用户要不要做什么。
	Action string `json:"action,omitempty"`
	// Anomaly：是不是异常（要不要启动分析）。「等用户决定」的新条目、额度快用完只是要用户知道，不分析。
	Anomaly   bool       `json:"anomaly,omitempty"`
	FirstSeen time.Time  `json:"first_seen"`
	LastSeen  time.Time  `json:"last_seen"`
	job       *state.Job // 中断/失败/超时的待做任务：新发现时看要不要写接手的待做
	jobID     string     // 这个问题只点名一个任务时的任务 id，推送带上，回复能接上那个任务
	// quiet：tick 发现时已经在汇总里写过一行（疑似卡死、额度档位变化），巡逻不再重复写。
	quiet bool
	// handled：已经自动处理（写了接手的待做、或已有任务在接手），不推给用户。
	handled bool
	gone    string // 消失时：已恢复 / 已处理
}

// infra：基础设施类问题，看门狗也查得到。照常检查、写进 patrol.json 和当天汇总，但不推给用户（巡逻推送和早报的「出问题」都不带），
// 由看门狗推。看门狗查不到的照常推：launchd 服务没装上（它只看列表里有的服务的退出码和常驻服务的 pid）、夜班报告缺失（它只查早报）。
func (is PatrolIssue) infra() bool {
	switch is.Check {
	case "autopush", "backup", "disk":
		return true
	case "launchd":
		return !strings.HasSuffix(is.Fingerprint, ":missing")
	case "report":
		return strings.HasPrefix(is.Fingerprint, "report:"+config.KindReview+":")
	}
	return false
}

type PatrolState struct {
	LastRun time.Time `json:"last_run"`
	// Issues 是上次巡逻时还在的问题；这次还在的不重报，不在了的算已恢复。
	Issues        []PatrolIssue `json:"issues"`
	AnalysisDay   string        `json:"analysis_day,omitempty"`
	AnalysisCount int           `json:"analysis_count,omitempty"`
	// Vuln：每周一次的漏洞检查（vuln.go）。
	Vuln *VulnState `json:"vuln,omitempty"`
}

type PatrolReport struct {
	At            time.Time
	launchd       map[string]bool // 这次 launchctl list 里有的服务（判断消失的 launchd 问题是不是真恢复）
	pushOK        map[string]bool // 之后同仓库又推成功的那几段推送失败的指纹（判断消失的 autopush 问题是不是真恢复）
	New, Old      []PatrolIssue
	Gone          []PatrolIssue
	Notes         []string // 检查本身出的问题（读不了 launchd、BACKLOG 等），不算发现
	Analysis      string   // 分析启动了没有、为什么
	AnalysisJobID string
}

func (a *Service) PatrolPath() string { return filepath.Join(a.DataDir(), "patrol.json") }

// MorningIssues 是早报「出问题」一节要列的标题（接到 notify 的 MorningIssues 上）。读不出 patrol.json 时没有。
func (a *Service) MorningIssues() []string {
	ps, err := a.LoadPatrol()
	if err != nil {
		return nil
	}
	var titles []string
	for _, is := range ps.Issues {
		if is.Check != "decide" && !is.infra() { // 基础设施类由看门狗推
			titles = append(titles, textfmt.TruncRunes(is.Title, 40))
		}
	}
	return titles
}

// LoadPatrol：文件不存在是第一次巡逻；读不出返回错误（当作未知，不巡逻）；内容坏了就备份成 patrol.json.bad-<时间>、从空的开始
// （之前报过的问题会再报一次）。
func (a *Service) LoadPatrol() (*PatrolState, error) {
	b, err := os.ReadFile(a.PatrolPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &PatrolState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ps PatrolState
	if err := json.Unmarshal(b, &ps); err != nil {
		bak := a.PatrolPath() + ".bad-" + a.Now().Format("20060102-150405")
		if werr := fsx.WriteFileAtomic(bak, b); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），备份到 %s 也失败：%w", a.PatrolPath(), err, bak, werr)
		}
		a.Logf("巡逻：%s 格式不对（%v），已备份为 %s，从空的开始（报过的问题会再报一次）", a.PatrolPath(), err, bak)
		// 坏文件留着的话，status 每读一次就再备份一份
		if werr := a.SavePatrol(&PatrolState{}); werr != nil {
			a.Logf("巡逻：重置 %s 失败：%v", a.PatrolPath(), werr)
		}
		return &PatrolState{}, nil
	}
	return &ps, nil
}

func (a *Service) SavePatrol(ps *PatrolState) error {
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(a.PatrolPath(), append(b, '\n'))
}

// PatrolIfDue：tick 调用。巡逻开着、离上次满 patrolEvery（或时钟往回拨过）才巡逻一次，结果只记日志和汇总，出错不影响 tick。
func (a *Service) PatrolIfDue(st *state.State) {
	if !a.Cfg.PatrolEnabled {
		return
	}
	defer func() { // 巡逻出了没想到的错也不能让 tick 非 0 退出（连续 2 次会触发自动回滚）
		if p := recover(); p != nil {
			a.Logf("巡逻：出错，本次巡逻作废，不影响 tick：%v", p)
		}
	}()
	ps, err := a.LoadPatrol()
	if err != nil {
		a.Logf("巡逻：读不了 %s，本次不巡逻：%v", a.PatrolPath(), err)
		return
	}
	if now := a.Now(); !ps.LastRun.IsZero() && !now.Before(ps.LastRun) && now.Sub(ps.LastRun) < patrolEvery {
		return
	}
	r, err := a.RunPatrol(st, ps)
	if err != nil {
		a.Logf("巡逻：%v", err)
		return
	}
	a.Logf("%s", r.logLine())
}

// CmdPatrol：立刻巡逻一次（不看 patrol_enabled 和上次时间），打印全部现有问题。
func (a *Service) CmdPatrol() error {
	lock, err := a.Runner.LockForCommand()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := state.Load(a.StatePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出，当作未知，不巡逻（下次 tick 会处理）：%w", err)
	}
	a.Runner.Reap(st)
	a.Runner.EnforceTimeouts(st)
	if err := state.Save(a.StatePath(), st); err != nil {
		return err
	}
	ps, err := a.LoadPatrol()
	if err != nil {
		return fmt.Errorf("读不了 %s，不巡逻：%w", a.PatrolPath(), err)
	}
	r, err := a.RunPatrol(st, ps)
	if err != nil {
		return err
	}
	r.print(a.Out, a.PatrolPath(), a.NightlyPath(r.At))
	return nil
}

// RunPatrol 跑一圈检查，和上次的问题按指纹比对：新问题写接手待做、存 patrol.json、写当天汇总，有新异常时看要不要启动分析。
func (a *Service) RunPatrol(st *state.State, ps *PatrolState) (*PatrolReport, error) {
	now := a.Now()
	r := &PatrolReport{At: now}
	found, failed := a.patrolChecks(st, ps, now, r)
	a.takeOverReturned(st, now)
	prev := map[string]PatrolIssue{}
	for _, is := range ps.Issues {
		prev[is.Fingerprint] = is
	}
	seen := map[string]bool{}
	var cur []PatrolIssue
	for _, f := range found {
		if seen[f.Fingerprint] {
			continue
		}
		seen[f.Fingerprint] = true
		f.LastSeen = now
		if old, ok := prev[f.Fingerprint]; ok {
			f.FirstSeen, f.Action = old.FirstSeen, old.Action
			r.Old = append(r.Old, f)
		} else {
			f.FirstSeen = now
			if f.job != nil {
				f.Action, f.handled = a.TakeOverTodo(st, f.job)
			}
			r.New = append(r.New, f)
		}
		cur = append(cur, f)
	}
	for _, old := range ps.Issues {
		if seen[old.Fingerprint] {
			continue
		}
		// 这次检查本身没做成的，不知道问题还在不在：照旧记着，不算恢复
		if textfmt.HasAnyPrefix(old.Fingerprint, failed) {
			cur = append(cur, old)
			continue
		}
		switch old.gone = a.goneHow(st, r, old, now); old.gone {
		case goneKeep:
			old.gone = ""
			cur = append(cur, old)
		case "":
		default:
			r.Gone = append(r.Gone, old)
		}
	}
	ps.Issues, ps.LastRun = cur, now
	if err := a.SavePatrol(ps); err != nil {
		return nil, fmt.Errorf("写 %s 失败，本次发现不写汇总（下次巡逻会再报）：%w", a.PatrolPath(), err)
	}
	a.notifyPatrol(r)
	a.patrolAnalyze(st, ps, r)
	if r.AnalysisJobID != "" {
		if err := a.SavePatrol(ps); err != nil {
			a.Logf("巡逻：写 %s 失败（今天的分析次数没存下）：%v", a.PatrolPath(), err)
		}
	}
	for _, is := range r.New {
		if !is.quiet {
			a.Ledger.AppendNightlyLine(now, "巡逻发现："+is.nightlyText())
		}
	}
	var fixed, handled []string
	for _, is := range r.Gone {
		if is.gone == goneHandled {
			handled = append(handled, textfmt.TruncRunes(is.Title, 60))
		} else {
			fixed = append(fixed, textfmt.TruncRunes(is.Title, 60))
		}
	}
	if len(fixed) > 0 {
		a.Ledger.AppendNightlyLine(now, fmt.Sprintf("巡逻：已恢复 %d 个之前报过的问题：%s", len(fixed), strings.Join(fixed, "；")))
	}
	if len(handled) > 0 {
		a.Ledger.AppendNightlyLine(now, fmt.Sprintf("巡逻：%d 条等用户决定的已处理（不在那一节了）：%s", len(handled), strings.Join(handled, "；")))
	}
	if r.AnalysisJobID != "" {
		a.Ledger.AppendNightlyLine(now, "巡逻："+r.Analysis)
	}
	return r, nil
}

const (
	goneFixed   = "已恢复"
	goneHandled = "已处理"
	goneKeep    = "keep" // 还说不准好没好：照旧记着
)

// goneHow：上次报过的问题这次没再出现，是真的好了（已恢复）、拍板条目被处理了（已处理）、还说不准（goneKeep，照旧记着），
// 还是只是过了时间窗口、换了一天、服务卸掉了（返回空：悄悄去掉，不写「已恢复」）。
func (a *Service) goneHow(st *state.State, r *PatrolReport, is PatrolIssue, now time.Time) string {
	switch is.Check {
	case "job":
		return ""
	case "autopush": // 之后同仓库推成功了才算恢复；过了 24 小时窗口的悄悄去掉
		if !r.pushOK[is.Fingerprint] {
			return ""
		}
	case "decide":
		if decs, fails := a.Notify.Decisions(); len(fails) == 0 {
			for _, d := range decs {
				if d.Fingerprint == is.Fingerprint && d.Waiting {
					return "" // 条目还在，只是加上了等条件标记：悄悄去掉，不写「已处理」
				}
			}
		}
		return goneHandled
	case "stall":
		id := strings.TrimPrefix(is.Fingerprint, "stall:")
		for _, j := range st.Running() {
			if j.ID == id {
				return goneFixed // 会话记录又在长了
			}
		}
		return "" // 任务结束了：失败、超时由任务那一类报
	case "backup":
		if !strings.HasSuffix(is.Fingerprint, ":"+now.Format("2006-01-02")) {
			return "" // 前一天的，换天了
		}
	case "report": // report:<kind>:<日期>：报告文件真的有了才算恢复；补跑还没写出来（或开关刚关）就照旧记着
		f := strings.Split(is.Fingerprint, ":")
		if len(f) != 3 || f[2] != now.Format("2006-01-02") {
			return ""
		}
		if _, err := os.Stat(filepath.Join(a.ReportDir(), f[2]+config.Routines[f[1]].Report+".md")); err != nil {
			return goneKeep
		}
	case "launchd": // launchd:<服务>[:missing|:down]：服务还在列表里（退出码回到 0、缺的装上了、又在跑了）才算恢复，从列表里卸掉了不算
		label := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(is.Fingerprint, "launchd:"), ":missing"), ":down")
		if !r.launchd[label] {
			return ""
		}
	case "usage": // 一个窗口重置了，别的窗口仍 ≥90% 时不算恢复（那个窗口会作为新指纹报出来）
		if lvl, _, _ := backend.UsageLevel(st.UsageOf(a.Cfg.Backend), now, a.Cfg.UsagePolicy()); lvl >= backend.UsageStop {
			return ""
		}
	}
	return goneFixed
}

// patrolChecks 依次跑各项检查。检查本身没做成时，在 r.Notes 记原因、返回它的指纹前缀（那一类上次的问题照旧记着）。
func (a *Service) patrolChecks(st *state.State, ps *PatrolState, now time.Time, r *PatrolReport) ([]PatrolIssue, []string) {
	var out []PatrolIssue
	var failed []string
	fail := func(prefix, note string) {
		failed = append(failed, prefix)
		r.Notes = append(r.Notes, note)
	}
	out = append(out, a.checkLaunchd(r, fail)...)
	out = append(out, a.checkAutopush(now, r, fail)...)
	out = append(out, a.checkBackupReport(now, fail)...)
	out = append(out, a.CheckJobs(st, now)...)
	out = append(out, a.checkUsage(st, now)...)
	out = append(out, a.CheckReports(st, now)...)
	out = append(out, a.checkDecisions(fail)...)
	out = append(out, a.CheckDirtyBacklogs(now, fail)...)
	out = append(out, a.checkDisk(fail)...)
	out = append(out, a.checkVuln(ps, now)...)
	return out, failed
}

// PatrolStatus：status 里的一行，上次巡逻时间和现有问题数（读 patrol.json，不拿锁）。
func (a *Service) PatrolStatus() string {
	off := ""
	if !a.Cfg.PatrolEnabled {
		off = "tick 不自动巡逻（patrol_enabled=false），"
	}
	ps, err := a.LoadPatrol()
	switch {
	case err != nil:
		return off + "读不了 " + a.PatrolPath() + "：" + err.Error()
	case ps.LastRun.IsZero():
		return off + "还没巡逻过（agentq patrol 立刻查一遍）"
	}
	vuln := ps.VulnStatus()
	if vuln != "" {
		vuln = "；" + vuln
	}
	if len(ps.Issues) == 0 {
		return off + "上次 " + textfmt.WhenCN(ps.LastRun) + "，没有问题" + vuln
	}
	return fmt.Sprintf("%s上次 %s，现有 %d 个问题（agentq patrol 看证据）%s", off, textfmt.WhenCN(ps.LastRun), len(ps.Issues), vuln)
}
