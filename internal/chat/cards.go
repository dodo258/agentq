package chat

import (
	"errors"
	"fmt"

	"agentq/internal/discuss"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// handleCallback 处理按钮回调：核对用户身份、按钮没过期、条目没变，然后走和 Router 的 decide 同一条路（routerDecide），不起模型。
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
	if !a.Notify.CardCurrent(&card) {
		a.Logf("Telegram：%s「%s」的条目改过了，按钮不生效", card.Project, textfmt.TruncRunes(card.Title, 30))
		answer(notify.CardChanged)
		return
	}
	if err := a.routerDecide(RouterAction{Type: "decide", Project: card.Project, Fingerprint: card.Fingerprint, Decision: choice}); err != nil {
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

// gateCallback：门卡住的任务的按钮。决定写给门，下次 tick 落。
func (a *Service) gateCallback(c *telegram.TgCreds, q *telegram.TgCallback, id string, card notify.DecideCard, label, choice string, answer func(string)) {
	action, reason := "", ""
	switch label {
	case "A":
		action, reason = gate.DecidePass, "Telegram 按钮放行"
	case "B":
		action = gate.DecideReturn
	case "C":
		action = gate.DecideAbandon
	case "D":
		action = gate.DecideRerun
	}
	if action == "" {
		answer(notify.CardExpired)
		return
	}
	if err := a.Gate.Decide(card.GateJob, action, reason); err != nil {
		a.Logf("Telegram：门的决定没记下（任务 %s）：%v", card.GateJob, err)
		answer("没记下，过一会儿再点")
		return
	}
	a.markCardDecided(id, choice)
	answer("已定：" + textfmt.TruncRunes(choice, 100))
	a.editDecided(c, q, card, choice, "下次 tick 生效，最多等 5 分钟")
}
