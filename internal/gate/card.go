package gate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 门停下等用户定时，决定卡片上给人看的话：这次改了什么、为什么停下、建议选哪个和为什么。
// 先分清是哪种停下：改动本身有问题、检查自己出了故障判不了、检查都过了但按规矩要人点头、分支被别人合了没过检查。
// 门的名字、原始报错、日志路径、分支名都只在 agentq gate report 里，卡片上不写。

type stopKind int

const (
	stopChange stopKind = iota // 检查查出改动本身有问题
	stopBroken                 // 检查自己出了故障，判不了改动好坏
	stopHold                   // 检查都过了，按规矩要人点头
	stopBypass                 // 改动已经在默认分支上，不是引擎合的
)

type gateCard struct {
	Head      string // 标题末尾的说法
	Lines     []string
	Recommend string
}

// stopKindOf 看这次过门的结果是哪种停下。挡住合并的是没过的那道 error 档门（runGates 在它那里停下；warn 档没过只记录），它判不了就算检查出故障。
func stopKindOf(res RunResult) (stopKind, *Result) {
	switch res.Outcome {
	case outBypass:
		return stopBypass, nil
	case outHold:
		return stopHold, nil
	}
	var block *Result
	for i := range res.Results {
		if r := &res.Results[i]; r.Outcome != Pass && r.Level != LevelWarn {
			block = r
		}
	}
	switch {
	case block != nil && (block.Outcome == Unknown || block.Unjudged):
		return stopBroken, block
	case block != nil:
		return stopChange, block
	case res.Outcome == outFail:
		return stopChange, nil
	}
	return stopBroken, nil
}

// plainCard 排卡片正文。options 是卡片上真有的按钮，只推荐其中的。
func plainCard(j *state.Job, res RunResult, options []notify.DecideOption) gateCard {
	kind, block := stopKindOf(res)
	var c gateCard
	if changed := plainText(j.Gate.Ready.Progress); changed != "" {
		c.Lines = append(c.Lines, "这次改的："+changed)
	}
	passed := passedGates(res.Results)
	has := func(label string) bool {
		for _, o := range options {
			if o.Label == label {
				return true
			}
		}
		return false
	}
	var why, advice string
	switch kind {
	case stopHold:
		c.Head = "等你拍板"
		why = "检查都过了"
		if passed != "" {
			why += "（" + passed + "）"
		}
		why += "，但这次改动按规矩要你点头才合并：" + holdWhy(res)
		c.Recommend, advice = "A", "停下是规矩，不是查出了问题；确认上面说的那几处改得对，就放行"
		if agent, ok := resultNamed(res.Results, HoldReviewGate); ok {
			c.Recommend, advice = agentAdvice(agent, has("D"))
			why += "。独立审查代先替你看过：" + agentWords(agent)
		}
	case stopBypass:
		c.Head = "门没过，等你定"
		why = "这条的改动已经在主分支上了，但不是 agentq 合进去的，没经过检查"
		c.Recommend, advice = "A", "改动已经在主分支上，放行只是把这条记成做完；不放心就先自己看看改动"
	case stopBroken:
		c.Head = "检查出故障，等你定"
		why = "不是改动查出了问题，是检查自己出了故障：" + brokenPoint(block) + "，所以判不了这次改动好不好"
		if passed != "" {
			why += "。已经过了的：" + passed
		}
		if has("D") {
			c.Recommend, advice = "D", "可能只是偶发故障，代码不动重新检查一遍就知道，不算退回次数；再检查还是这样，就选退回让它查是不是改动把检查弄坏了"
		} else {
			c.Recommend, advice = "B", "已经重新检查过几次还是跑不起来，可能是改动把检查要用的东西弄坏了，退回让它查"
		}
	default:
		c.Head = "门没过，等你定"
		if res.Conflict { // 只是交付后主分支又有了新提交，检查还没跑：不是改动或拆法的问题
			why = "不是改动查出了问题，是交付后主分支上又有了别的任务的新提交：" + changePoint(block)
			if n := j.GateConflicts + 1; n >= MaxConflicts {
				why += fmt.Sprintf("。主分支一直在动，已经连着 %d 次这样", n)
			}
			c.Recommend, advice = "B", "退回让它再合一次最新的主分支，不用改拆法；并行的任务多时可以等一等再退回，改动留在分支上不会丢"
			break
		}
		why = "检查发现改动本身有问题：" + changePoint(block)
		if g := j.Gate; g.Round >= MaxRounds && g.Override == "" && !g.Rerun {
			why += fmt.Sprintf("。它已经自己改了 %d 轮还没过", g.Round)
			c.Recommend, advice = "C", "再退回多半还是一样，更可能是这条要求太大或验收说得不清；放弃后把条目拆小、写清验收再派，改动留在分支上不会丢"
		} else {
			c.Recommend, advice = "B", "问题出在改动上，退回让它照检查发现的问题再改一轮"
		}
	}
	c.Lines = append(c.Lines, "为什么停："+why)
	for _, o := range options {
		if o.Label == c.Recommend {
			c.Lines = append(c.Lines, "建议选 "+o.Label+" "+o.Text+"："+advice)
		}
	}
	var how []string
	for _, o := range options {
		if m := optionMeaning[o.Text]; m != "" {
			how = append(how, o.Text+"＝"+m)
		}
	}
	c.Lines = append(c.Lines, strings.Join(how, "；"), "细节和原始报错：agentq gate report "+j.ID)
	return c
}

var optionMeaning = map[string]string{
	"放行":   "合并这次改动",
	"退回":   "让它再改一轮",
	"放弃":   "先放着不管，改动留在分支上",
	"再审一次": "代码不动，重新检查一遍，不算退回次数",
}

// gateWords 是每道门对用户的叫法，只列用户关心过没过的。
var gateWords = map[string]string{
	"test": "测试", "eval": "自检考题", "review": "代码审查", "smoke": "成品启动", "ui": "界面截图", "capability": "新能力检查",
}

func passedGates(results []Result) string {
	var names []string
	for _, r := range results {
		if w := gateWords[r.Gate]; w != "" && r.Outcome == Pass {
			names = append(names, w)
		}
	}
	return strings.Join(names, "、")
}

func changePoint(r *Result) string {
	if r == nil {
		return "有一项检查没过"
	}
	switch r.Gate {
	case "test":
		return "项目自带的测试没过"
	case "review":
		return "代码审查找出了要改的问题"
	case "eval":
		return "agentq 给自己出的考题，有的比上一版做得差了"
	case "scope":
		return "改到了退回时说好不动的文件"
	case "patch":
		return "同一块代码最近已经修过好几次，这次还是打补丁，要整块重写"
	case "smoke":
		return "做出来的成品启动不起来"
	case "ui":
		return "界面截图检查没过"
	case "capability":
		return "新能力的检查没过"
	case "merge":
		s := "和主分支上新提交的改动撞在一起了"
		if names := baseNames(r.Files, 3); names != "" {
			s += "（撞在 " + names + "）"
		}
		return s
	}
	return "有一项检查没过"
}

func brokenPoint(r *Result) string {
	if r == nil {
		return "检查这次没跑完，没给出结论"
	}
	switch {
	case r.Gate == "eval" && r.Unjudged:
		return "agentq 给自己出的考题有的这次没跑起来（布置考场那一步就失败了）"
	case r.Gate == "eval":
		return "agentq 的自检这次没跑完"
	case r.Gate == "test":
		return "项目的测试这次没跑完（超时，或者跑测试的环境不对）"
	case r.Gate == "review":
		return "代码审查这次没给出结论"
	case r.Gate == "merge":
		return "没法把这次改动和主分支合到一起来检查"
	case r.Gate == "smoke":
		return "成品启动检查这次没跑完"
	case r.Gate == "ui":
		return "界面截图这次没截成"
	}
	return "有一项检查这次没跑成"
}

// holdWhy：为什么要人点头，一句话。
func holdWhy(res RunResult) string { return strings.Join(holdPoints(res), "；") }

// agentWords：独立审查代的结论，给人看的那句话。
func agentWords(r Result) string {
	_, words, ok := strings.Cut(r.Summary, "：")
	if !ok {
		words = r.Summary
	}
	words = plainText(strings.ReplaceAll(words, "；", "，"))
	switch r.Outcome {
	case Fail:
		return "有疑问，" + words
	case Unknown:
		return "没能判断，" + words
	}
	return "放行，" + words
}

// agentAdvice：独立审查代有疑问就建议退回；判不了建议再审一次（会再请它看），再审用完了只能你自己看。
func agentAdvice(r Result, canRerun bool) (string, string) {
	switch {
	case r.Outcome == Fail:
		return "B", "独立审查代看了改动觉得有地方不对，退回让它照这个疑问改；你自己看过觉得没问题，也可以放行"
	case canRerun:
		return "D", "独立审查代这次没给出结论，可能只是偶发，代码不动再审一次会重新请它看；还是判不了，就自己看过上面说的那几处再放行"
	}
	return "A", "独立审查代判不了，只能你自己看：上面说的那几处改得对就放行，不对就退回"
}

// holdPoints：为什么要人点头，和 holdReason 认的几种一一对应。
func holdPoints(res RunResult) []string {
	var out []string
	if v := res.Risk; v != nil {
		switch v.Level {
		case RiskIrreversible:
			out = append(out, "改动里有删数据、改数据库或部署这类改错了难撤回的地方")
		case RiskBroad:
			if _, ran := resultNamed(res.Results, "test"); !ran {
				out = append(out, "改动影响面广，又没有能自动跑的测试兜底")
			}
		}
	}
	for _, r := range res.Results {
		switch {
		case r.Hold == "":
		case r.Gate == "eval":
			out = append(out, "它改了 agentq 检查自己用的规则，这种改动不让它自己说了算")
		default:
			out = append(out, "有一项检查要求人看过再合")
		}
	}
	if len(out) == 0 {
		out = append(out, "这类改动要人看过再合")
	}
	return out
}

func baseNames(files []string, n int) string {
	var names []string
	for _, f := range files {
		if len(names) == n {
			names = append(names, "等")
			break
		}
		names = append(names, filepath.Base(f))
	}
	return strings.Join(names, "、")
}

var (
	codeSpan    = regexp.MustCompile("`[^`]*`")
	pathLike    = regexp.MustCompile(`(?:进|到|在)?\s*[A-Za-z0-9_.~*-]*(?:/[A-Za-z0-9_.*-]+)+/?`)
	fileLike    = regexp.MustCompile(`(?:进|到|在)?\s*\b[A-Za-z0-9_-]+\.(?:go|md|json|sh|ya?ml|toml|txt|log|tsx?|jsx?|py|swift|sql|sq|html|css)\b`)
	emptyParen  = regexp.MustCompile(`[（(]\s*[，、,\s]*[）)]`)
	spaceBefore = regexp.MustCompile(`\s+([，。；、：）,;:)])`)
	jargon      = regexp.MustCompile(`(?i)\b(?:eval|baseline|patch|scope)\b`)
	jargonWord  = map[string]string{"eval": "评测", "baseline": "上次的成绩", "patch": "补丁", "scope": "范围"}
)

// plainText：任务交付单里那句「改了什么」去掉代码、路径和文件名，只留第一句。
func plainText(s string) string {
	s = textfmt.OneLine(s, 1000)
	if first, _, ok := strings.Cut(s, "；"); ok {
		s = first
	}
	s = codeSpan.ReplaceAllString(s, "")
	s = pathLike.ReplaceAllString(s, "")
	s = fileLike.ReplaceAllString(s, "")
	s = jargon.ReplaceAllStringFunc(s, func(w string) string { return jargonWord[strings.ToLower(w)] })
	s = emptyParen.ReplaceAllString(s, "")
	s = spaceBefore.ReplaceAllString(s, "$1")
	s = strings.Join(strings.Fields(s), " ")
	return textfmt.OneLine(strings.Trim(s, " ，、：；"), 90)
}
