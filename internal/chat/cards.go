package chat

import (
	"context"
	"errors"
	"fmt"

	"agentq/internal/discuss"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// handleCallback 处理按钮回调：核对用户身份、按钮没过期、条目没变，然后用 decide 写进 BACKLOG，不起模型。
// 重复点只生效一次。接口失败不回滚已生效的决定，消息没改成只记日志。
func (a *Service) handleCallback(c *telegram.TgCreds, q *telegram.TgCallback) {
	answer := func(text string) {
		if err := a.Telegram.TgAnswer(c, q.ID, text); err != nil {
			a.Logf("Telegram：回应按钮失败：%v", c.Redact(err.Error()))
		}
	}
	if q.From.IsBot || q.From.ID != c.ChatID || (q.Message != nil && q.Message.Chat.ID != c.ChatID) {
		a.Logf("Telegram：忽略用户 %d 点的按钮（只认配好的账号）", q.From.ID)
		answer("只有配好的账号能点")
		return
	}
	m := notify.CardDataRe.FindStringSubmatch(q.Data)
	if m == nil {
		answer(notify.CardExpired)
		return
	}
	var card notify.DecideCard
	found := false
	if err := a.Notify.WithNotify(func(s *notify.State) error {
		s.PruneCards(a.Now())
		if x := s.Cards[m[1]]; x != nil {
			card, found = *x, true
		}
		return nil
	}); err != nil {
		a.Logf("Telegram：读不了按钮记录：%v", err)
		answer("现在读不了记录，请回文字")
		return
	}
	if !found {
		answer(notify.CardExpired)
		return
	}
	if card.Decided != "" && card.GateJob != "" {
		answer(textfmt.TruncRunes("已经定过了："+card.Decided, 190))
		return
	}
	if card.Decided != "" {
		answer("已经定了：" + card.Decided)
		return
	}
	choice := ""
	for _, o := range card.Options {
		if o.Label == m[2] {
			choice = o.Label + " " + o.Text
		}
	}
	if choice == "" {
		answer(notify.CardExpired)
		return
	}
	if card.GateJob != "" {
		a.gateCallback(c, q, m[1], card, m[2], choice, answer)
		return
	}
	if card.DiscussKey != "" {
		a.discussCallback(c, q, m[1], card, m[2], answer)
		return
	}
	if card.IssueURL != "" {
		a.issueCallback(c, q, m[1], card, m[2], choice, answer)
		return
	}
	if !a.Notify.CardCurrent(&card) {
		a.Logf("Telegram：%s「%s」的条目改过了，按钮不生效", card.Project, textfmt.TruncRunes(card.Title, 30))
		answer(notify.CardChanged)
		return
	}
	if err := a.decide(card.Project, card.Fingerprint, choice); err != nil {
		a.Logf("Telegram：按钮没定成（%s「%s」）：%v", card.Project, textfmt.TruncRunes(card.Title, 30), err)
		answer(textfmt.TruncRunes("没定成："+textfmt.OneLine(err.Error(), 100)+"。过一会儿再点，或回文字", 190))
		return
	}
	if err := a.Notify.WithNotify(func(s *notify.State) error {
		if x := s.Cards[m[1]]; x != nil {
			x.Decided = choice
		}
		return nil
	}); err != nil { // 决定已经写进 BACKLOG：再点时条目已不在「等用户决定」，同样不会再生效
		a.Logf("Telegram：决定已生效，但按钮记录没存下：%v", err)
	}
	msg := fmt.Sprintf("用户点按钮定了：%s「%s」选 %s", card.Project, textfmt.TruncRunes(card.Title, 40), textfmt.OneLine(choice, 60))
	a.Logf("Telegram：%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	answer("已定：" + textfmt.TruncRunes(choice, 100))
	if q.Message == nil {
		return
	}
	next := "下次 tick 接着做，最多等 5 分钟"
	if a.Cfg.Paused {
		next = "现在暂停着，恢复后才开工"
	}
	text := a.Notify.RenderMessage("done", a.Now(), []notify.MsgSection{{Emoji: "🟢", Head: "已定　" + card.Project + "　" + textfmt.TruncRunes(card.Title, 60),
		Lines: []string{"选了 " + textfmt.OneLine(choice, 150), next}}}, "agentq")
	if err := a.Telegram.TgEdit(c, q.Message.MessageID, text); err != nil {
		a.Logf("Telegram：决定已生效，消息没改成结论（不重试）：%v", c.Redact(err.Error()))
	}
}

// discussCallback：商量大方向的按钮。回答写给 discuss，下次 tick 写进条目。过期了（已经按默认开工）告诉用户。
func (a *Service) discussCallback(c *telegram.TgCreds, q *telegram.TgCallback, id string, card notify.DecideCard, label string, answer func(string)) {
	text := ""
	for _, o := range card.Options {
		if o.Label == label {
			text = o.Text
		}
	}
	if err := a.Discuss.Answer(card.DiscussKey, text); err != nil {
		a.Logf("Telegram：大方向的回答没记下（编号 %s）：%v", card.DiscussKey, err)
		if errors.Is(err, discuss.ErrClosed) {
			answer("已经过了，这一条按默认开工了")
			return
		}
		answer("没记下，过一会儿再点")
		return
	}
	choice := label + " " + text
	a.markCardDecided(id, choice)
	answer("已定：" + textfmt.TruncRunes(choice, 100))
	a.editDecided(c, q, card, choice, "下次 tick 写进条目开工，最多等 5 分钟")
}

// markCardDecided：按钮记录记下选了什么，再点就说已经定了。
func (a *Service) markCardDecided(id, choice string) {
	if err := a.Notify.WithNotify(func(s *notify.State) error {
		if x := s.Cards[id]; x != nil {
			x.Decided = choice
		}
		return nil
	}); err != nil {
		a.Logf("Telegram：决定已记下，按钮记录没存下：%v", err)
	}
}

// editDecided：消息改成结论，去掉按钮。
func (a *Service) editDecided(c *telegram.TgCreds, q *telegram.TgCallback, card notify.DecideCard, choice, next string) {
	if q.Message == nil {
		return
	}
	text := a.Notify.RenderMessage("done", a.Now(), []notify.MsgSection{{Emoji: "🟢", Head: "已定　" + card.Project + "　" + textfmt.TruncRunes(card.Title, 60),
		Lines: []string{"选了 " + textfmt.OneLine(choice, 150), next}}}, "agentq")
	if err := a.Telegram.TgEdit(c, q.Message.MessageID, text); err != nil {
		a.Logf("Telegram：决定已记下，消息没改成结论（不重试）：%v", c.Redact(err.Error()))
	}
}

// gateButtons：门卡片的按钮（字母见 gate 发卡片时的选项）。
var gateButtons = map[string]struct{ action, name string }{
	"A": {gate.DecidePass, "放行"}, "B": {gate.DecideReturn, "退回"}, "C": {gate.DecideAbandon, "放弃"}, "D": {gate.DecideRerun, "再审一次"},
}

// gateCallback：门卡住的任务的按钮。决定写给门，下次 tick 落。门已经有了决定（命令行、会话里定过）或任务不再停在门上时不再写，回已经定过、消息改成结论。
func (a *Service) gateCallback(c *telegram.TgCreds, q *telegram.TgCallback, id string, card notify.DecideCard, label, choice string, answer func(string)) {
	b, ok := gateButtons[label]
	if !ok {
		answer(notify.CardExpired)
		return
	}
	settled, done, err := a.gateSettled(card.GateJob)
	if err != nil {
		a.Logf("Telegram：读不了任务 %s 的状态，按钮不生效：%v", card.GateJob, err)
		answer("现在读不了任务状态，过一会儿再点")
		return
	}
	if done {
		a.markCardDecided(id, settled)
		answer(textfmt.TruncRunes("已经定过了："+settled, 190))
		if q.Message != nil {
			if err := a.Telegram.TgEdit(c, q.Message.MessageID, a.Notify.SettledCardText(&card, settled)); err != nil {
				a.Logf("Telegram：门已经定过，消息没改成结论（不重试）：%v", c.Redact(err.Error()))
			}
		}
		return
	}
	reason := ""
	if b.action == gate.DecidePass {
		reason = notify.GatePassByCard
	}
	if err := a.Gate.Decide(card.GateJob, b.action, reason); err != nil {
		a.Logf("Telegram：门的决定没记下（任务 %s）：%v", card.GateJob, err)
		answer("没记下，过一会儿再点")
		return
	}
	a.markCardDecided(id, choice)
	answer("已定：" + textfmt.TruncRunes(choice, 100))
	a.editDecided(c, q, card, choice, "下次 tick 生效，最多等 5 分钟")
}

// gateSettled：任务的门已经有了决定（记下了、下次 tick 落），或已经不停在门上时，返回怎么定的。状态文件读不了返回错误：当未知，不写决定。
func (a *Service) gateSettled(jobID string) (string, bool, error) {
	if action, why, ok := a.Gate.PendingDecision(jobID); ok {
		o := notify.GateOutcome{Choice: action, Reason: why, Note: "下次 tick 生效"}
		for _, b := range gateButtons {
			if b.action == action {
				o.Choice = b.name
			}
		}
		return o.Text(), true, nil
	}
	st, err := state.Load(a.StatePath())
	if err != nil {
		return "", false, err
	}
	o, done := notify.GateSettled(st, jobID)
	return o.Text(), done, nil
}

// issueCallback：发版后 issue 回复草稿的按钮。A 发、B 发并关闭、C 不发；只有 A、B 才调 gh 贴回复。没贴成可以再点。
func (a *Service) issueCallback(c *telegram.TgCreds, q *telegram.TgCallback, id string, card notify.DecideCard, label, choice string, answer func(string)) {
	act := map[string]string{"A": notify.IssueActSend, "B": notify.IssueActSendClose, "C": notify.IssueActSkip}[label]
	if act == "" {
		answer(notify.CardExpired)
		return
	}
	note, err := a.Notify.AnswerIssue(context.Background(), card.IssueURL, act)
	if errors.Is(err, notify.ErrIssueAnswered) {
		answer("已经定了")
		return
	}
	if err != nil {
		answer(textfmt.TruncRunes("没贴成："+textfmt.OneLine(err.Error(), 120)+"。过一会儿再点", 190))
		return
	}
	a.markCardDecided(id, choice)
	a.Ledger.AppendNightlyLine(a.Now(), fmt.Sprintf("用户点按钮定了 issue 回复：%s %s：%s", card.Project, card.IssueURL, note))
	answer(textfmt.TruncRunes(note, 190))
	a.editDecided(c, q, card, choice, note)
}
