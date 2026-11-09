package chat

import (
	"fmt"
	"regexp"
	"strings"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

type fastCmd int

const (
	fastNone fastCmd = iota
	fastPause
	fastResume
	fastUsage
	fastRunning
	fastStop
)

// fastStopIDRe：「停 」后面只认任务 id 或它的前缀（任务 id 是「月日-时分秒-项目」，st.find 认 4 位以上前缀）。
// 「停 下来」「停 一下」不是 id，交给模型。
var fastStopIDRe = regexp.MustCompile(`^\d{4}(-\d{0,6}(-[^\s？?]*)?)?$`)

// parseFastCmd 认这几个固定说法。问句（「暂停？」「恢复？」）不当命令：它们会改状态，交给模型理解；只读的两个问号无所谓。
func parseFastCmd(text string) (fastCmd, string) {
	t := strings.TrimSpace(text)
	switch strings.TrimRight(t, "。.？?") {
	case "额度":
		return fastUsage, ""
	case "在跑什么":
		return fastRunning, ""
	}
	t = strings.TrimRight(t, "。.")
	switch t {
	case "暂停":
		return fastPause, ""
	case "恢复":
		return fastResume, ""
	}
	if f := strings.Fields(t); len(f) == 2 && f[0] == "停" && fastStopIDRe.MatchString(f[1]) {
		return fastStop, f[1]
	}
	return fastNone, ""
}

// fastAnswer 算好要回的消息。停任务不在这里（要 tick.lock，见 serveStops）。
func (a *Service) fastAnswer(cmd fastCmd) string {
	switch cmd {
	case fastPause:
		return a.fastPaused(true)
	case fastResume:
		return a.fastPaused(false)
	case fastUsage:
		return a.usageDigest()
	case fastRunning:
		return a.RunningDigest()
	}
	return ""
}

func (a *Service) fastStatus(head string, lines ...string) string {
	return a.Notify.RenderMessage("status", a.Now(), []notify.MsgSection{{Emoji: "⚙️", Head: head, Lines: lines}}, "agentq 对话")
}

// fastPaused 改 paused 并说明做了什么。只改这一个键（见 setConfigPaused）：listen 手上的是长轮询之前读的，直接存会盖掉这期间用户改的配置。
func (a *Service) fastPaused(paused bool) string {
	cfg, err := config.Load(a.ConfigPath())
	if err != nil {
		return a.fastStatus("配置读不出，没改", textfmt.OneLine(err.Error(), 200))
	}
	st, stErr := state.Load(a.StatePath())
	if cfg.Paused == paused {
		if paused {
			return a.fastStatus("本来就暂停着，没改", "发「恢复」恢复")
		}
		return a.fastStatus("本来就没暂停，没改")
	}
	if err := config.SetPaused(a.ConfigPath(), paused); err != nil {
		return a.fastStatus("没写进配置，没改", textfmt.OneLine(err.Error(), 200))
	}
	a.Cfg.Paused = paused
	if paused {
		head := "已暂停：不再接新活（paused=true）"
		running := "在跑的任务继续跑到结束，收尾照常"
		if stErr == nil {
			running = fmt.Sprintf("在跑的 %d 个任务继续跑到结束，收尾照常", len(st.Tasks()))
		}
		return a.fastStatus(head, running, "夜班、复盘、这里的对话和 agentq run 不受影响；发「恢复」恢复")
	}
	lines := []string{"下次 tick 起接活，最多等 5 分钟"}
	if stErr == nil {
		now := a.Now()
		lvl, until, why := backend.UsageLevel(st.Usage, now, a.Cfg.UsagePolicy())
		if note := a.Runner.SchedNote(st, nil, now, lvl, until, why); note != "正常" {
			lines = append(lines, "不过现在："+note)
		}
	}
	return a.fastStatus("已恢复（paused=false）", lines...)
}

// usageDigest：「额度」的回答。缓存过期就现读一次（同 agentq status：claude -p /usage 是本地命令，不问模型；只显示，不写回 state）。
func (a *Service) usageDigest() string {
	now := a.Now()
	var u *backend.UsageInfo
	if st, err := state.Load(a.StatePath()); err == nil {
		u = st.Usage
	}
	if u.Stale(now) {
		u = a.Runner.ReadUsage(now)
	}
	lines := []string{textfmt.WhenCN(u.CheckedAt) + " 读取"}
	if lvl, until, why := backend.UsageLevel(u, now, a.Cfg.UsagePolicy()); lvl >= backend.UsageStop {
		lines = append(lines, backend.UsageGateNote(lvl, until, why))
	}
	return a.fastStatus("额度　"+backend.UsageDescribe(u, now, a.Cfg.UsagePolicy()), lines...)
}

// RunningDigest：「在跑什么」的回答，占名额的任务在前，id 写全，用户可以直接复制去停。
func (a *Service) RunningDigest() string {
	st, err := state.Load(a.StatePath())
	if err != nil {
		return a.fastStatus("状态文件读不出", textfmt.OneLine(err.Error(), 200))
	}
	now := a.Now()
	running := st.Tasks()
	for _, j := range st.Running() {
		if !state.TakesSlot(j.Kind) {
			running = append(running, j)
		}
	}
	if len(running) == 0 {
		return a.fastStatus("现在没有任务在跑")
	}
	var lines []string
	for _, j := range running {
		lines = append(lines, fmt.Sprintf("%s　%s「%s」%s %s", j.ID, j.Project, textfmt.TruncRunes(j.Title, 24), state.JobLabel(j), textfmt.FormatIdle(now.Sub(j.StartedAt))))
	}
	lines = append(lines, "要停就发「停 <任务id>」")
	return a.fastStatus(fmt.Sprintf("在跑　%d 个（占名额 %d/%d）", len(running), len(st.Tasks()), a.Cfg.MaxParallel), lines...)
}

// serveStops 执行收件箱里的「停 <任务id>」（调用方拿着 tick.lock 和 notify.lock，见 serveInbox）。不看对话任务在不在跑、额度够不够。
// 和 agentq stop 一样算用户亲手停的。
func (a *Service) serveStops(s *notify.State, st *state.State, c *telegram.TgCreds) {
	var stops, rest []notify.InMsg
	for _, m := range s.Inbox {
		if m.StopJob != "" {
			stops = append(stops, m)
		} else {
			rest = append(rest, m)
		}
	}
	if len(stops) == 0 {
		return
	}
	s.Inbox = rest // 先摘出来：收尾时可能往收件箱加东西
	for _, m := range stops {
		msg, err := a.Runner.StopByID(st, m.StopJob)
		if err != nil {
			msg = "没停：" + err.Error()
		}
		a.Logf("Telegram：用户要停 %s：%s", m.StopJob, textfmt.OneLine(msg, 200))
		a.replyPlain(c, m.ID, msg)
	}
}
