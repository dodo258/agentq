package notify

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// 决策卡片（票拟批红）：「等用户决定」的条目按固定格式写了选项和推荐时，推送带 Telegram 按钮，点一下就定，消息改成结论；不起模型。
// 只认一种写法（行内，或条目下一行缩进）：（选项：A 做法一｜B 做法二；推荐 A）。解析不了就照旧推文字。
// 接口见 core.telegram.org/bots/api：callback_data 1-64 字节；按下按钮后「Telegram clients will display a progress bar until you call answerCallbackQuery」；
// answerCallbackQuery 的 text 0-200 字。文档没写 editMessageText 不带 reply_markup 时键盘怎样，所以明确传空键盘。
// 提交审核绝不做成按钮：发版提示不带键盘，这条路径也调不到提交代码（TestSubmitPathIsSingle 看着）。
const (
	CardKeep       = 14 * 24 * time.Hour
	cardMax        = 300
	cardMaxOptions = 6
	cardsPerFlush  = 5 // 一次最多发这么多张，多的留在队列里下次发
	CardButtonText = 30

	CardExpired = "按钮过期了，请回文字"
	CardChanged = "条目改过了，请回文字"
)

type DecideOption struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// DecideCard 是一张卡片：callback_data 只放短 id，其余存在 notify.json 的 cards。
type DecideCard struct {
	ID          string         `json:"id,omitempty"`
	Project     string         `json:"project"`
	Fingerprint string         `json:"fingerprint"`
	TextFP      string         `json:"text_fp"` // 条目全文的指纹：标题指纹只防换条目，这个防改了选项或正文
	Title       string         `json:"title"`
	Options     []DecideOption `json:"options"`
	Recommend   string         `json:"recommend"`
	At          time.Time      `json:"at,omitzero"`
	Decided     string         `json:"decided,omitempty"`
	// GateJob 非空：门卡住的任务的卡片，按钮的决定交给门，不走「等用户决定」的条目。
	GateJob string `json:"gate_job,omitempty"`
	// DiscussKey 非空：开工前商量大方向的卡片，按钮的回答交给 discuss。
	DiscussKey string `json:"discuss_key,omitempty"`
}

var (
	decideOptionsRe     = regexp.MustCompile(`（选项：(.+?)；推荐 ([A-Z])）`)
	DecideOptionsLineRe = regexp.MustCompile(`^（选项：.+?；推荐 [A-Z]）$`)
	CardDataRe          = regexp.MustCompile(`^dc:([0-9a-f]{12}):([A-Z])$`)
)

// ParseDecideOptions 只认固定格式：恰好一处、2 到 6 个选项，每个是「大写字母 空格 做法」，字母不重复，推荐的那个字母在选项里。
func ParseDecideOptions(text string) ([]DecideOption, string, bool) {
	if strings.Count(text, "（选项：") != 1 {
		return nil, "", false
	}
	m := decideOptionsRe.FindStringSubmatch(text)
	if m == nil {
		return nil, "", false
	}
	parts := strings.Split(m[1], "｜")
	if len(parts) < 2 || len(parts) > cardMaxOptions {
		return nil, "", false
	}
	seen := map[string]bool{}
	var opts []DecideOption
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) < 3 || p[0] < 'A' || p[0] > 'Z' || p[1] != ' ' {
			return nil, "", false
		}
		label, body := p[:1], strings.TrimSpace(p[2:])
		if body == "" || seen[label] {
			return nil, "", false
		}
		seen[label] = true
		opts = append(opts, DecideOption{Label: label, Text: body})
	}
	if !seen[m[2]] {
		return nil, "", false
	}
	return opts, m[2], true
}

func StripDecideOptions(text string) string {
	return strings.TrimRight(decideOptionsRe.ReplaceAllString(text, ""), " ")
}

// DecisionTitle：选项写在标题里时不算标题（选项改了是另一回事，由全文指纹管）。
func DecisionTitle(text string) string { return backlog.ItemTitle(StripDecideOptions(text)) }

func decisionTextFP(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// decideNotice 排好「要你定」这条消息。带选项的多一张卡片；不带的和原来逐字一样。
func decideNotice(d Decision) Notice {
	n := Notice{Cat: "decide", Key: d.Fingerprint, Project: d.Project, Text: d.Project + "　" + textfmt.TruncRunes(d.Title, 60)}
	opts, rec, ok := ParseDecideOptions(d.Text)
	body := d.Text
	if ok {
		body = StripDecideOptions(d.Text)
	}
	if _, rest, cut := strings.Cut(body, "——"); cut && strings.TrimSpace(rest) != "" {
		n.Lines = append(n.Lines, textfmt.OneLine(rest, 150))
	}
	if !ok {
		n.Lines = append(n.Lines, "回复这条消息说选哪个，就按你的决定接着做")
		return n
	}
	for _, o := range opts {
		n.Lines = append(n.Lines, textfmt.OneLine(optionLine(o, rec), 150))
	}
	n.Lines = append(n.Lines, "点按钮就定；想说别的就回复这条消息")
	n.Card = &DecideCard{Project: d.Project, Fingerprint: d.Fingerprint, TextFP: decisionTextFP(d.Text), Title: d.Title, Options: opts, Recommend: rec}
	return n
}

func optionLine(o DecideOption, rec string) string {
	s := o.Label + " " + o.Text
	if o.Label == rec {
		s += "（推荐）"
	}
	return s
}

// CardMarkup 拼 inline_keyboard，一行一个按钮（做法可能很长）。
func CardMarkup(card *DecideCard) (string, error) {
	type button struct {
		Text string `json:"text"`
		Data string `json:"callback_data"`
	}
	rows := make([][]button, 0, len(card.Options))
	for _, o := range card.Options {
		text := o.Label + " " + textfmt.TruncRunes(o.Text, CardButtonText)
		if o.Label == card.Recommend {
			text += "（推荐）"
		}
		rows = append(rows, []button{{Text: text, Data: "dc:" + card.ID + ":" + o.Label}})
	}
	b, err := json.Marshal(map[string]any{"inline_keyboard": rows})
	return string(b), err
}

func newCardID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *State) PruneCards(now time.Time) {
	for id, c := range s.Cards {
		if c == nil || now.Sub(c.At) > CardKeep { // notify.json 写坏时可能是 null
			delete(s.Cards, id)
		}
	}
	for len(s.Cards) > cardMax {
		oldest := ""
		for id, c := range s.Cards {
			if oldest == "" || c.At.Before(s.Cards[oldest].At) || c.At.Equal(s.Cards[oldest].At) && id < oldest {
				oldest = id
			}
		}
		delete(s.Cards, oldest)
	}
}

// sendDecideCards 把这批里带卡片的逐张发出去（卡片不合并：合起来就说不清按钮是哪件事），返回剩下要照旧发的。调用方拿着 notify.lock。
func (a *Service) sendDecideCards(s *State, st *state.State, c *telegram.TgCreds, batch []Notice, now time.Time) []Notice {
	var rest []Notice
	sent, failed := 0, false
	for _, n := range batch {
		if n.Card == nil {
			rest = append(rest, n)
			continue
		}
		if failed || sent >= cardsPerFlush {
			continue // 留在队列里
		}
		card := *n.Card
		id, err := newCardID()
		var markup string
		if err == nil {
			card.ID, card.At = id, now
			markup, err = CardMarkup(&card)
		}
		msg := a.RenderMessage("decide", now, []MsgSection{{Emoji: NotifyCats["decide"].emoji, Head: n.Text, Lines: n.Lines}}, "agentq")
		var sentID int64
		if err == nil {
			sentID, err = a.Telegram.TgSendMarkup(c, msg, 0, markup)
		}
		if err != nil {
			failed = true
			a.NotifyErr(s, fmt.Sprintf("Telegram 发送失败，要你定的卡片留在队列里下次再试：%v", err))
			a.macFallback(s, []Notice{n}, msg)
			continue
		}
		if s.Cards == nil {
			s.Cards = map[string]*DecideCard{}
		}
		s.Cards[card.ID] = &card
		s.PruneCards(now)
		if th, ok := a.noticeThread(st, n); ok {
			s.AddThread(sentID, th, now)
		}
		s.removePending([]Notice{n})
		s.LastSent[n.Cat] = now
		s.Count++
		s.LastError = ""
		sent++
		a.Logf("推送：发出「要你定」卡片 %s（%d 个选项）", n.Project, len(card.Options))
	}
	return rest
}

// cardNotices：队列里没发的卡片。早报只列标题，按钮还得单独发。
func cardNotices(list []Notice) []Notice {
	var out []Notice
	for _, n := range list {
		if n.Card != nil {
			out = append(out, n)
		}
	}
	return out
}

// CardCurrent：条目还在「等用户决定」、还能定、全文没变。
func (a *Service) CardCurrent(card *DecideCard) bool {
	all, _ := a.Decisions()
	for _, d := range Askable(all) {
		if d.Project == card.Project && d.Fingerprint == card.Fingerprint && decisionTextFP(d.Text) == card.TextFP {
			return true
		}
	}
	return false
}
