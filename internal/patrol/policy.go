package patrol

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/state"
)

// 巡逻问题怎么处置，只在这个文件里定：推不推「巡逻发现」、进不进早报「出问题」、要不要起分析、要不要写汇总、
// 是不是看门狗那边的事、是不是另一个问题的后果、消失时算不算恢复。检查函数只查事实、写证据和事实标记
// （excluded、handled、CausedBy），不自己定这些。
//
// 为什么这样分：以前每个检查函数一边查一边自己定 Anomaly、quiet，推送、早报、看门狗分工又各按检查名写一份判断，
// 一件事是不是问题、归谁、是不是别的问题的后果没有统一的地方定。加一种情况就要改好几处，漏一处就误报或重复报：
// 当天才打开开关也报缺报告（7064d94）、门正常退回报成任务中断（8d8b712）、夜班任务失败了又报一遍缺夜班报告，
// 还被自我进化当成 agentq 自己的问题攒成待做（这次）。现在加一类检查就在 policies 加一行，后果关系用 CausedBy 写清，
// 推送、早报、分析都按同一套判断走。

// policy 是一类检查（PatrolIssue.Check）的处置。表里没有的检查什么都不做：不推、不进早报、不分析。
type policy struct {
	push    bool                 // 新发现、没自动处理时推「巡逻发现」。拍板、额度各有自己的推送
	morning bool                 // 进早报「出问题」
	anomaly bool                 // 新发现时起一次只读分析（任务所在项目被排除的除外）
	quiet   bool                 // tick 发现时已经写过汇总（疑似卡死、额度档位变化），巡逻不再写
	infra   func(fp string) bool // 看门狗也查得到：照常记、写汇总，但不推、不进早报（看门狗推）
}

func always(string) bool { return true }

var policies = map[string]policy{
	// 服务退出码、常驻服务没在跑由看门狗推；服务没装上它查不到（它只看列表里有的服务），巡逻推
	"launchd":  {push: true, morning: true, anomaly: true, infra: func(fp string) bool { return !strings.HasSuffix(fp, ":missing") }},
	"autopush": {anomaly: true, infra: always},
	"backup":   {anomaly: true, infra: always},
	"disk":     {anomaly: true, infra: always},
	"job":      {push: true, morning: true, anomaly: true},
	"stall":    {push: true, morning: true, anomaly: true, quiet: true},
	"usage":    {morning: true, quiet: true},
	// 缺早报看门狗也查，缺夜班报告它查不到
	"report":  {push: true, morning: true, anomaly: true, infra: func(fp string) bool { return strings.HasPrefix(fp, "report:"+config.KindReview+":") }},
	"decide":  {},
	"backlog": {push: true, morning: true},
	vulnCheck: {push: true, morning: true},
}

func (is PatrolIssue) policy() policy { return policies[is.Check] }

// infra：基础设施类问题，看门狗也查得到。
func (is PatrolIssue) infra() bool {
	p := is.policy()
	return p.infra != nil && p.infra(is.Fingerprint)
}

// echoOf：这个问题是另一个现有问题的后果（open 是现有问题的指纹）。原因已经报过、推过、分析过，这一条只记着、写汇总。
// 原因不在现有问题里（过了窗口、本来就不报）时，这一条自己算数。
func (is PatrolIssue) echoOf(open map[string]bool) bool {
	return is.CausedBy != "" && open[is.CausedBy]
}

// settle 按表定 Anomaly（落盘，分析和 status 看它）。
func (is *PatrolIssue) settle(open map[string]bool) {
	is.Anomaly = is.policy().anomaly && !is.excluded && !is.echoOf(open)
}

func settleAll(list []PatrolIssue, open map[string]bool) []PatrolIssue {
	for i := range list {
		list[i].settle(open)
	}
	return list
}

func openSet(lists ...[]PatrolIssue) map[string]bool {
	open := map[string]bool{}
	for _, l := range lists {
		for _, is := range l {
			open[is.Fingerprint] = true
		}
	}
	return open
}

// pushed：新发现时推给用户。
func (is PatrolIssue) pushed(open map[string]bool) bool {
	return is.policy().push && !is.infra() && !is.handled && !is.echoOf(open)
}

// inMorning：列进早报「出问题」。
func (is PatrolIssue) inMorning(open map[string]bool) bool {
	return is.policy().morning && !is.infra() && !is.echoOf(open)
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
