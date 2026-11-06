package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agentq/internal/asc"
	"agentq/internal/notify"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// 提交 App Store 审核。整个项目里对外风险最高的动作，只有一条路能到：
// Telegram 收消息（takeUpdates）→ releaseIntercept → submitFromReply → submitOffer。
// Router、对话、任务 prompt 碰不到它，也没有 CLI 子命令；源码里的调用关系有用例断言（TestSubmitPathIsSingle）。
// 写接口（POST、PATCH）只在这个文件里。
const (
	releaseWord       = "发"
	releaseSubmitWait = 3 * time.Minute
	releaseCheckStale = 10 * time.Minute // 校验中（还没调写接口）崩溃了，这么久之后可以重新回复

	phaseCreate = "create"
	PhaseItem   = "item"
	phaseSubmit = "submit"
)

func phaseCN(phase string) string {
	switch phase {
	case phaseCreate:
		return "第 1 步，建审核提交"
	case PhaseItem:
		return "第 2 步，加版本"
	case phaseSubmit:
		return "第 3 步，标记提交"
	}
	return "步骤不明"
}

// send 发一次写请求，绝不重试。
func send(c *asc.Client, ctx context.Context, method, path string, body any) (*asc.Doc, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	status, data, err := c.RoundTrip(ctx, method, c.Endpoint(path, nil), b)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, asc.HTTPError(method, path, status, data)
	}
	return c.Decode(method, path, data)
}

// ReleaseIntercept 在把消息交给模型之前拦截。返回真表示这条处理完了（不进收件箱，模型看不到）：
//   - 回复了 📦 提示：整句就是「发」、没有附件才试着提交；别的话一律不提交，也不交给模型（「发吧」交给有权限的模型，可能被理解成同意）；
//   - 有等回复的提示时，整句是「发」却不是回复那条提示：不提交，告诉用户怎么做。
//
// 其余情况不管，走原来的流程。
func (a *Service) ReleaseIntercept(c *telegram.TgCreds, m *telegram.TgMsg) bool {
	exact := m.Text == releaseWord && !telegram.HasAttachment(m)
	var th notify.Thread
	isRelease, unknown := false, false
	if m.ReplyTo != nil {
		ns, err := a.Notify.LoadNotify()
		if err != nil {
			unknown = true
		} else if t, ok := ns.Threads[m.ReplyTo.MessageID]; ok && t.Kind == notify.ThreadRelease {
			th, isRelease = t, true
		}
	}
	say := func(text string) { a.releaseSay(c, m.MessageID, text) }
	switch {
	case isRelease && exact:
		say(a.submitFromReply(c, m, th))
	case isRelease:
		a.Logf("发版助手：用户回复了提示，但不是整句「发」，没提交")
		say("没提交：只有回复这条提示、整句就是「" + releaseWord + "」才提交。要问别的请发新消息。")
	case exact && unknown:
		a.Logf("发版助手：收到「发」，但回复链读不了，没提交")
		say("没提交：回复链读不了，不知道这条是在回复什么。")
	case exact && a.releaseHasLiveOffer():
		a.Logf("发版助手：收到「发」，但不是回复提示消息，没提交")
		say("没提交：要提交审核，请回复那条 📦 提示消息，整句写「" + releaseWord + "」。")
	default:
		return false
	}
	return true
}

// releaseHasLiveOffer：有等回复、没过期的提示。读不了记录当有：宁可多拦一句「发」。
func (a *Service) releaseHasLiveOffer() bool {
	if !a.Cfg.ReleaseAssistEnabled {
		return false
	}
	s, err := a.ReadRelease()
	if err != nil {
		return true
	}
	for _, o := range s.Offers {
		if o.Status == OfferOffered && a.Now().Sub(o.OfferedAt) <= ReleaseOfferTTL {
			return true
		}
	}
	return false
}

// releaseSay 把结果回给用户（挂在他那条消息下面）；发不出去记进推送队列，不丢。
func (a *Service) releaseSay(c *telegram.TgCreds, replyTo int64, text string) {
	if _, err := a.Telegram.TgSend(c, textfmt.Esc(text), replyTo); err != nil {
		a.Logf("发版助手：回复用户失败，记进推送队列：%v", c.Redact(err.Error()))
		a.Notify.Notify(notify.Notice{Cat: "alert", Text: textfmt.TruncRunes(text, 300)})
	}
}

// claimOffer 抢这条提示（拿着 release.lock）：只有等回复的、没过期的能抢到，抢到就落盘成「校验中」。
// 同一时间只有一个能抢到；已经提交、提交中、失败过的一律不再提交。返回记录的副本，或给用户看的拒绝理由。
func (a *Service) claimOffer(id string, replyTo int64, msgAt time.Time) (ReleaseOffer, string) {
	var got ReleaseOffer
	refuse := ""
	err := a.WithRelease(func(s *ReleaseStore) error {
		now := a.Now()
		o := s.Find(id)
		if o == nil || o.MessageID == 0 || o.MessageID != replyTo {
			refuse = "没提交：这条提示对不上记录。"
			return nil
		}
		if o.Status == OfferChecking && now.Sub(o.CheckingAt) >= releaseCheckStale {
			o.Status = OfferOffered // 上次校验到一半进程没了，还没调过写接口
		}
		switch o.Status {
		case OfferOffered:
		case OfferChecking:
			refuse = "没提交：上一次回复还在校验，等结果。"
			return nil
		case OfferSubmitted:
			refuse = fmt.Sprintf("没提交：%s %s 已经提交过了，不重复提交。", o.Project, o.Version)
			return nil
		case OfferSubmitting:
			refuse = fmt.Sprintf("没提交：%s %s 上次提交走到一半没有结果（%s），不重复提交。去 App Store Connect 看实际状态。", o.Project, o.Version, phaseCN(o.Phase))
			return nil
		case OfferFailed:
			refuse = fmt.Sprintf("没提交：%s %s 上次提交失败了，不自动重试：%s", o.Project, o.Version, textfmt.OneLine(o.Result, 200))
			return nil
		case OfferExpired:
			refuse = "没提交：这条提示已经过期了。"
			return nil
		default:
			refuse = "没提交：这条提示已经失效了（" + offerStatusCN(o) + "）。"
			return nil
		}
		if now.Sub(o.OfferedAt) > ReleaseOfferTTL || msgAt.Sub(o.OfferedAt) > ReleaseOfferTTL {
			o.Status, o.UpdatedAt = OfferExpired, now
			refuse = "没提交：这条提示发出超过 7 天，已经过期了。等下一个构建再提示。"
			return nil
		}
		o.Status, o.CheckingAt, o.UpdatedAt = OfferChecking, now, now
		got = *o
		return nil
	})
	if err != nil {
		return got, "没提交：发版记录读写不了：" + textfmt.OneLine(err.Error(), 200)
	}
	return got, refuse
}

// submitFromReply：用户回复了提示、整句是「发」。先抢记录，再重新校验全部条件，都对才提交。返回给用户看的话。
func (a *Service) submitFromReply(c *telegram.TgCreds, m *telegram.TgMsg, th notify.Thread) string {
	if m.From == nil || m.From.IsBot || m.From.ID != c.ChatID {
		a.Logf("发版助手：回复「发」的不是配好的账号本人，没提交")
		return "没提交：只有配好的账号本人回复才算。"
	}
	if !a.Cfg.ReleaseAssistEnabled {
		a.Logf("发版助手：release_assist_enabled 关着，回复「发」没提交")
		return "没提交：发版助手关着（release_assist_enabled=false）。"
	}
	o, refuse := a.claimOffer(th.Fingerprint, m.ReplyTo.MessageID, time.Unix(m.Date, 0))
	if refuse != "" {
		a.Logf("发版助手：回复「发」被拒绝：%s", textfmt.OneLine(refuse, 200))
		return refuse
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseSubmitWait)
	defer cancel()
	ver, client, why := a.recheckOffer(ctx, o)
	if why != "" {
		// 还没调过写接口：放回等回复，用户可以再回一次（每次都重新校验）
		if err := a.updateOffer(o.ID, func(r *ReleaseOffer) { r.Status = OfferOffered; r.LastErr = textfmt.OneLine(why, 300) }); err != nil {
			a.Logf("发版助手：%s 放回等回复失败（%s 分钟后可以再回）：%v", o.Project, textfmt.FormatDuration(releaseCheckStale), err)
		}
		msg := fmt.Sprintf("没提交 %s %s：%s", o.Project, o.Version, why)
		a.Logf("发版助手：%s", textfmt.OneLine(msg, 400))
		a.Ledger.AppendNightlyLine(a.Now(), "发版助手："+textfmt.OneLine(msg, 400))
		return msg
	}
	return a.SubmitOffer(ctx, client, o, ver)
}

// recheckOffer 提交前重新校验：App 对应、全部条件、构建没变、版本还在准备中且挂着这个构建。why 非空表示不能提交。只读。
func (a *Service) recheckOffer(ctx context.Context, o ReleaseOffer) (ReleaseVersion, *asc.Client, string) {
	c, err := a.ASC.NewASC()
	if err != nil {
		return ReleaseVersion{}, nil, textfmt.OneLine(err.Error(), 200)
	}
	mapped, _, err := a.releaseApps(ctx, c)
	if err != nil {
		return ReleaseVersion{}, nil, "查不出：" + textfmt.OneLine(err.Error(), 200)
	}
	var m *asc.MappedApp
	for i := range mapped {
		if mapped[i].ID == o.AppID && mapped[i].Project.Name == o.Project {
			m = &mapped[i]
		}
	}
	if m == nil {
		return ReleaseVersion{}, nil, "App 和项目对应不上了"
	}
	known, knownErr := a.ReadRelease()
	rep := a.releaseCheckApp(ctx, c, *m, known, knownErr)
	if !rep.ok() {
		return ReleaseVersion{}, nil, "条件不满足：" + textfmt.OneLine(rep.failures(), 400)
	}
	if rep.BuildID != o.BuildID {
		return ReleaseVersion{}, nil, fmt.Sprintf("构建变了（提示的是 %s，现在最新是 %s）", o.BuildNumber, rep.BuildNumber)
	}
	ver, problem, err := preparedVersion(c, ctx, o.AppID)
	switch {
	case err != nil:
		return ReleaseVersion{}, nil, "查不出 App Store 版本：" + textfmt.OneLine(err.Error(), 200)
	case problem != "":
		return ReleaseVersion{}, nil, problem
	case ver.BuildID != o.BuildID:
		return ReleaseVersion{}, nil, "App Store 版本挂的不是提示的那个构建"
	case ver.ID != o.VersionID || ver.Version != o.Version:
		return ReleaseVersion{}, nil, fmt.Sprintf("App Store 版本变了（现在是 %s）", textfmt.OneLine(ver.Version, 20))
	}
	return ver, c, ""
}

// SubmitOffer 走官方三步：建 reviewSubmissions、加 reviewSubmissionItems、标记 submitted=true。
// 每一步先落盘再调接口；落不了盘就停，崩溃后记录停在「提交中」，不会自动重来。失败如实说留下了什么，不重试、不清理。
func (a *Service) SubmitOffer(ctx context.Context, c *asc.Client, o ReleaseOffer, ver ReleaseVersion) string {
	name := o.Project + " " + o.Version
	if err := a.updateOffer(o.ID, func(r *ReleaseOffer) { r.Status, r.Phase = OfferSubmitting, phaseCreate }); err != nil {
		msg := "没提交 " + name + "：发版记录写不了，没调接口：" + textfmt.OneLine(err.Error(), 200)
		a.Logf("发版助手：%s", msg)
		return msg
	}
	finish := func(status, msg string) string {
		if err := a.updateOffer(o.ID, func(r *ReleaseOffer) { r.Status, r.Result = status, textfmt.OneLine(msg, 600) }); err != nil {
			msg += "（记录写不了：" + textfmt.OneLine(err.Error(), 100) + "）"
		}
		a.Logf("发版助手：%s", textfmt.OneLine(msg, 600))
		a.Ledger.AppendNightlyLine(a.Now(), "发版助手："+textfmt.OneLine(msg, 600))
		return msg
	}
	// 第 1 步
	d, err := send(c, ctx, http.MethodPost, "/v1/reviewSubmissions", map[string]any{"data": map[string]any{
		"type":          "reviewSubmissions",
		"attributes":    map[string]any{"platform": "IOS"},
		"relationships": map[string]any{"app": map[string]any{"data": map[string]any{"type": "apps", "id": o.AppID}}},
	}})
	var sub asc.Resource
	if err == nil {
		if sub, err = d.One(); err == nil && sub.ID == "" {
			err = errors.New("响应里没有提交 id")
		}
	}
	if err != nil {
		left := "App Store Connect 上没有留下东西"
		if ascAmbiguous(err) {
			left = "说不准有没有建出来（请求可能到了），去 App Store Connect 看"
		}
		return finish(OfferFailed, fmt.Sprintf("提交 %s 失败，第 1 步（建审核提交）：%s。%s。不自动重试。", name, textfmt.OneLine(err.Error(), 300), left))
	}
	if err := a.updateOffer(o.ID, func(r *ReleaseOffer) { r.SubmissionID, r.Phase = sub.ID, PhaseItem }); err != nil {
		return finish(OfferFailed, fmt.Sprintf("提交 %s 停在第 1 步之后：记录写不了（%s）。留下了一个空的审核提交 %s，没提交给 Apple。", name, textfmt.OneLine(err.Error(), 100), sub.ID))
	}
	// 第 2 步
	_, err = send(c, ctx, http.MethodPost, "/v1/reviewSubmissionItems", map[string]any{"data": map[string]any{
		"type": "reviewSubmissionItems",
		"relationships": map[string]any{
			"reviewSubmission": map[string]any{"data": map[string]any{"type": "reviewSubmissions", "id": sub.ID}},
			"appStoreVersion":  map[string]any{"data": map[string]any{"type": "appStoreVersions", "id": ver.ID}},
		},
	}})
	if err != nil {
		return finish(OfferFailed, fmt.Sprintf("提交 %s 失败，第 2 步（把版本加进审核提交）：%s。留下了审核提交 %s（还没提交给 Apple），要清理或手动继续去 App Store Connect。不自动重试。", name, textfmt.OneLine(err.Error(), 300), sub.ID))
	}
	// 第 3 步：调之前就算「提交过」，结果不明时 14 天规则也按提交过算
	now := a.Now()
	if err := a.WithRelease(func(s *ReleaseStore) error {
		r := s.Find(o.ID)
		if r == nil {
			return errors.New("记录不见了")
		}
		r.Phase, r.UpdatedAt = phaseSubmit, now
		if s.LastSubmit == nil {
			s.LastSubmit = map[string]time.Time{}
		}
		s.LastSubmit[o.AppID] = now
		return nil
	}); err != nil {
		return finish(OfferFailed, fmt.Sprintf("提交 %s 停在第 2 步之后：记录写不了（%s）。审核提交 %s 已经有版本，没提交给 Apple。", name, textfmt.OneLine(err.Error(), 100), sub.ID))
	}
	d, err = send(c, ctx, http.MethodPatch, "/v1/reviewSubmissions/"+sub.ID, map[string]any{"data": map[string]any{
		"type": "reviewSubmissions", "id": sub.ID, "attributes": map[string]any{"submitted": true},
	}})
	state := ""
	if err == nil {
		if r, oerr := d.One(); oerr == nil {
			state = submissionState(r)
		}
	} else {
		// 第 3 步的结果不一定：读一下实际状态再说话（只读）
		state = a.readSubmissionState(ctx, c, sub.ID)
	}
	switch state {
	case "WAITING_FOR_REVIEW", "IN_REVIEW", "COMPLETING", StateComplete:
		return finish(OfferSubmitted, fmt.Sprintf("%s 已提交审核（状态 %s，审核提交 %s）。", name, state, sub.ID))
	}
	shown := state
	if shown == "" {
		shown = "读不出"
	}
	if err != nil {
		return finish(OfferFailed, fmt.Sprintf("提交 %s 失败，第 3 步（标记提交）：%s。审核提交 %s 现在的状态：%s，去 App Store Connect 看。不自动重试。", name, textfmt.OneLine(err.Error(), 300), sub.ID, shown))
	}
	return finish(OfferFailed, fmt.Sprintf("提交 %s 结果不对：第 3 步返回成功，但审核提交 %s 的状态是 %s，去 App Store Connect 看。不自动重试。", name, sub.ID, shown))
}

func submissionState(r asc.Resource) string {
	var at struct {
		State string `json:"state"`
	}
	if json.Unmarshal(r.Attributes, &at) != nil {
		return ""
	}
	return at.State
}

func (a *Service) readSubmissionState(ctx context.Context, c *asc.Client, id string) string {
	d, err := c.GetURL(ctx, c.Endpoint("/v1/reviewSubmissions/"+id, nil))
	if err != nil {
		a.Logf("发版助手：读审核提交 %s 的状态失败：%s", id, textfmt.OneLine(err.Error(), 200))
		return ""
	}
	r, err := d.One()
	if err != nil {
		return ""
	}
	return submissionState(r)
}

// ascAmbiguous：写请求的结果说不准（超时、断线、5xx）。4xx 是确定没做。
func ascAmbiguous(err error) bool {
	var ae *asc.Error
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return true
}
