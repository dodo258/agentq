package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/release"
	"agentq/internal/sense"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// ---- 假 ASC 上的世界：一个满足全部条件的 App ----

const (
	relBuildID   = "build-1"
	relVersionID = "ver-1"
	relSubID     = "sub-1"
)

type relEnv struct {
	*senseEnv
	alphaP backlog.Project
	noon   time.Time
	subGET atomic.Value // GET /v1/reviewSubmissions/<id> 返回的状态
	patchS atomic.Value // PATCH 返回的状态
}

func newRelEnv(t *testing.T, cfg map[string]any) *relEnv {
	t.Helper()
	full := map[string]any{"release_assist_enabled": true, "paused": false}
	for k, v := range cfg {
		full[k] = v
	}
	s := newSenseEnv(t, full)
	y, m, d := time.Now().Date()
	r := &relEnv{senseEnv: s, noon: time.Date(y, m, d, 12, 0, 0, 0, time.Local)}
	r.alphaP = s.alpha()
	r.subGET.Store("READY_FOR_REVIEW")
	r.patchS.Store("WAITING_FOR_REVIEW")
	r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-4*24*time.Hour), "VALID", false, "APP_STORE_ELIGIBLE")
	r.setVersion(relVersionID, "1.2.0", relBuildID)
	s.asc.setHandle(r.handleWrites)
	return r
}

func (r *relEnv) handleWrites(w http.ResponseWriter, req *http.Request, _ []byte) bool {
	switch req.Method + " " + req.URL.Path {
	case "POST /v1/reviewSubmissions":
		ascJSON(w, 201, map[string]any{"data": ascRes("reviewSubmissions", relSubID, map[string]any{"state": "READY_FOR_REVIEW"})})
	case "POST /v1/reviewSubmissionItems":
		ascJSON(w, 201, map[string]any{"data": ascRes("reviewSubmissionItems", "item-1", map[string]any{"state": "READY_FOR_REVIEW"})})
	case "PATCH /v1/reviewSubmissions/" + relSubID:
		ascJSON(w, 200, map[string]any{"data": ascRes("reviewSubmissions", relSubID, map[string]any{"state": r.patchS.Load()})})
	case "GET /v1/reviewSubmissions/" + relSubID:
		ascJSON(w, 200, map[string]any{"data": ascRes("reviewSubmissions", relSubID, map[string]any{"state": r.subGET.Load()})})
	default:
		return false
	}
	return true
}

func (r *relEnv) setBuild(id, number, version string, uploaded time.Time, state string, expired bool, audience string) {
	b := ascRes("builds", id, map[string]any{"version": number, "uploadedDate": uploaded.Format(time.RFC3339), "processingState": state, "expired": expired, "buildAudienceType": audience})
	b["relationships"] = map[string]any{"preReleaseVersion": map[string]any{"data": map[string]any{"type": "preReleaseVersions", "id": "pv-" + id}}}
	r.asc.setList("/v1/builds", b)
	r.asc.mu.Lock()
	r.asc.included["/v1/builds"] = []map[string]any{ascRes("preReleaseVersions", "pv-"+id, map[string]any{"version": version, "platform": "IOS"})}
	r.asc.mu.Unlock()
}

func (r *relEnv) setVersion(id, version, build string) {
	v := ascRes("appStoreVersions", id, map[string]any{"versionString": version, "appVersionState": "PREPARE_FOR_SUBMISSION"})
	if build != "" {
		v["relationships"] = map[string]any{"build": map[string]any{"data": map[string]any{"type": "builds", "id": build}}}
	}
	r.asc.setList("/v1/apps/"+senseAppID+"/appStoreVersions", v)
}

func (r *relEnv) crash(at time.Time) {
	r.asc.setList("/v1/apps/"+senseAppID+"/betaFeedbackCrashSubmissions", ascRes("betaFeedbackCrashSubmissions", "c1", map[string]any{"createdDate": at.Format(time.RFC3339)}))
}

func relSubmission(id, state string, at time.Time) map[string]any {
	attrs := map[string]any{"state": state, "platform": "IOS"}
	if !at.IsZero() {
		attrs["submittedDate"] = at.Format(time.RFC3339)
	}
	return ascRes("reviewSubmissions", id, attrs)
}

func (r *relEnv) clockAt(t time.Time) {
	r.extra = append(r.extra, "AGENTQ_TEST_NOW="+t.Format(time.RFC3339))
}

func (r *relEnv) store() *release.ReleaseStore {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.home, ".agentq", "release.json"))
	if err != nil {
		return &release.ReleaseStore{}
	}
	s := &release.ReleaseStore{}
	if err := json.Unmarshal(b, s); err != nil {
		r.t.Fatalf("release.json：%v\n%s", err, b)
	}
	return s
}

func (r *relEnv) offer() *release.ReleaseOffer {
	r.t.Helper()
	o := r.store().Find(release.OfferID(senseAppID, relBuildID))
	if o == nil {
		r.t.Fatalf("没有记录：%+v", r.store())
	}
	return o
}

// seedOffered：记录和回复链都就位，等用户回复。
func (r *relEnv) seedOffered(msgID int64, mut func(o *release.ReleaseOffer, s *release.ReleaseStore)) {
	r.t.Helper()
	o := &release.ReleaseOffer{ID: release.OfferID(senseAppID, relBuildID), Project: "alpha", ProjectDir: r.alphaP.Dir, AppID: senseAppID, AppName: "Alpha", BundleID: "test.example.alpha",
		BuildID: relBuildID, BuildNumber: "42", Version: "1.2.0", VersionID: relVersionID, Status: release.OfferOffered, MessageID: msgID, OfferedAt: r.noon.Add(-time.Hour), UpdatedAt: r.noon}
	s := &release.ReleaseStore{Offers: []*release.ReleaseOffer{o}}
	if mut != nil {
		mut(o, s)
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(r.home, ".agentq", "release.json"), b, 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.seedThreads(map[int64]notify.Thread{msgID: {Kind: notify.ThreadRelease, Project: "alpha", Fingerprint: o.ID}})
	r.seedOffsetOne()
}

// seedOffsetOne：已经收过消息（不算第一次），新消息不按「第一次收消息」跳过。
func (r *relEnv) seedOffsetOne() {
	r.t.Helper()
	ns := r.readNotify()
	ns.UpdateOffset = 1
	b, _ := json.Marshal(ns)
	if err := os.WriteFile(filepath.Join(r.home, ".agentq", "notify.json"), b, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// say 排一条用户的消息（时间是假时钟的中午）并 tick 一次处理它。replyTo 为 0 表示不是回复。
func (r *relEnv) say(update int, text string, replyTo int64) {
	r.t.Helper()
	r.tg.push(update, 500+update, text, replyTo, ownerChat, r.noon)
	r.mustRun("tick")
}

func (f *fakeTG) push(updateID, msgID int, text string, replyTo, from int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]any{"message_id": msgID, "date": at.Unix(), "text": text, "chat": map[string]any{"id": ownerChat, "type": "private"},
		"from": map[string]any{"id": from, "is_bot": false}}
	if replyTo != 0 {
		m["reply_to_message"] = map[string]any{"message_id": replyTo, "date": at.Unix(), "text": "📦 alpha 1.2.0 准备好了 回复这条消息说「发」提交审核", "chat": map[string]any{"id": ownerChat}}
	}
	f.updates = append(f.updates, map[string]any{"update_id": updateID, "message": m})
}

func (r *relEnv) noWrites() {
	r.t.Helper()
	if w := r.asc.writesList(); len(w) != 0 {
		r.t.Fatalf("不该写 ASC：%v", w)
	}
}

func (r *relEnv) lastText() string {
	r.t.Helper()
	ts := r.tg.texts()
	if len(ts) == 0 {
		r.t.Fatal("没发任何消息")
	}
	return ts[len(ts)-1]
}

func (r *relEnv) prepOut(ready bool, version string, changes []string, blocker string) {
	r.t.Helper()
	if changes == nil {
		changes = []string{}
	}
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "session_id": "s-rel",
		"structured_output": map[string]any{"ready": ready, "version": version, "changes": changes, "blocker": blocker}})
	r.prepRaw(string(b))
}

func (r *relEnv) prepRaw(text string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.home, "fake-release.json"), []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *relEnv) relJobs() []*Job {
	var out []*Job
	for _, j := range r.state().Jobs {
		if j.Kind == state.KindRelease {
			out = append(out, j)
		}
	}
	return out
}

// startPrep：定点检查必须起一个准备任务，tick 到它收尾。
func (r *relEnv) startPrep() *Job {
	r.t.Helper()
	before := len(r.relJobs())
	r.mustRun("review", "--scheduled")
	if len(r.relJobs()) != before+1 {
		r.t.Fatalf("应启动准备任务：%d → %d\n%s", before, len(r.relJobs()), r.launchdLog())
	}
	return r.tickUntilEnded(func() *Job {
		js := r.relJobs()
		return js[len(js)-1]
	})
}

var relGoodChanges = []string{"修了账单页闪退", "加了按月对比", "优化了启动速度"}

// ---- 用例 ----

// 全流程：条件满足 → 只准备 → 推提示并登记回复链 → 回别的、不回复、回别的消息都不提交 → 回复那条说「发」才提交三步 → 重复回复不再提交。
func TestReleaseFlow(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.prepOut(true, "1.2.0", relGoodChanges, "")
	j := r.startPrep()
	if j.Status != state.StatusDone {
		t.Fatalf("%s %s", j.Status, j.Summary)
	}
	r.noWrites()
	// 准备任务：只准备、有结构化输出、有预算、有 deny
	args := r.fakeLogOf(j)
	for _, want := range []string{"只准备，绝不提交审核", "ARG[--json-schema]", "ARG[--settings]", "reviewSubmission", "ARG[--max-budget-usd]\nARG[30]", "ARG[--disallowed-tools]", "alpha", "1.2.0 (42)"} {
		if !strings.Contains(args, want) {
			t.Errorf("准备任务缺 %q", want)
		}
	}
	if !strings.Contains(args, "/alpha]\nPPID[") {
		t.Errorf("准备任务应在项目目录：%s", args)
	}
	if !state.TakesSlot(j.Kind) {
		t.Error("准备任务占名额")
	}
	// tick 核对后推提示
	r.mustRun("tick")
	var offer string
	for _, m := range r.tg.texts() {
		if strings.Contains(m, "📦") {
			offer = m
		}
	}
	r.tg.mu.Lock()
	for _, form := range r.tg.sent { // 提交审核绝不做成按钮
		if strings.Contains(form.Get("text"), "📦") && form.Get("reply_markup") != "" {
			t.Errorf("发版提示不能带按钮：%s", form.Get("reply_markup"))
		}
	}
	r.tg.mu.Unlock()
	for _, want := range append([]string{"alpha 1.2.0 准备好了", release.ReleaseReplyHint}, relGoodChanges...) {
		if !strings.Contains(offer, want) {
			t.Fatalf("提示缺 %q：%s", want, offer)
		}
	}
	o := r.offer()
	if o.Status != release.OfferOffered || o.MessageID == 0 || o.VersionID != relVersionID {
		t.Fatalf("%+v", o)
	}
	th := r.readNotify().Threads[o.MessageID]
	if th.Kind != notify.ThreadRelease || th.Fingerprint != o.ID {
		t.Fatalf("回复链：%+v", th)
	}
	r.noWrites()
	runs := r.claudeRuns()
	if out := r.mustRun("status"); !strings.Contains(out, "发版助手：已开启") || !strings.Contains(out, "alpha 1.2.0") {
		t.Errorf("status：%s", out)
	}
	// 回别的：不提交，也不交给模型
	r.say(10, "发吧", o.MessageID)
	if !strings.Contains(r.lastText(), "没提交") {
		t.Fatalf("回复：%s", r.lastText())
	}
	// 不回复直接说「发」
	r.say(11, "发", 0)
	if !strings.Contains(r.lastText(), "没提交") {
		t.Fatalf("不回复：%s", r.lastText())
	}
	// 回复别的消息说「发」
	r.say(12, "发", 12345)
	if !strings.Contains(r.lastText(), "没提交") {
		t.Fatalf("回复别的：%s", r.lastText())
	}
	r.noWrites()
	if r.claudeRuns() != runs {
		t.Fatal("这些消息不该交给模型")
	}
	if ib := r.readNotify().Inbox; len(ib) != 0 {
		t.Fatalf("不该进收件箱：%+v", ib)
	}
	// 回复那条、整句「发」：三步
	r.say(13, "发", o.MessageID)
	want := []string{"POST /v1/reviewSubmissions", "POST /v1/reviewSubmissionItems", "PATCH /v1/reviewSubmissions/" + relSubID}
	if got := r.asc.writesList(); !slices.Equal(got, want) {
		t.Fatalf("写接口：%v", got)
	}
	if b := r.asc.bad(); len(b) != 0 {
		t.Fatalf("JWT 不合规：%v", b)
	}
	r.asc.mu.Lock()
	bodies := append([]string(nil), r.asc.bodies...)
	r.asc.mu.Unlock()
	if !strings.Contains(bodies[0], `"id":"`+senseAppID+`"`) || !strings.Contains(bodies[0], `"platform":"IOS"`) || !strings.Contains(bodies[0], `"type":"reviewSubmissions"`) {
		t.Errorf("第 1 步：%s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"id":"`+relSubID+`"`) || !strings.Contains(bodies[1], `"id":"`+relVersionID+`"`) || !strings.Contains(bodies[1], `"type":"reviewSubmissionItems"`) {
		t.Errorf("第 2 步：%s", bodies[1])
	}
	if !strings.Contains(bodies[2], `"submitted":true`) || !strings.Contains(bodies[2], `"id":"`+relSubID+`"`) {
		t.Errorf("第 3 步：%s", bodies[2])
	}
	if !strings.Contains(r.lastText(), "已提交审核") || !strings.Contains(r.lastText(), "WAITING_FOR_REVIEW") {
		t.Fatalf("结果：%s", r.lastText())
	}
	o = r.offer()
	if o.Status != release.OfferSubmitted || o.SubmissionID != relSubID {
		t.Fatalf("%+v", o)
	}
	if st := r.store(); st.LastSubmit[senseAppID].IsZero() {
		t.Fatal("没记本地提交时间")
	}
	if !strings.Contains(r.nightly(), "已提交审核") {
		t.Error("汇总没写")
	}
	// 重复回复：不再提交
	r.say(14, "发", o.MessageID)
	if len(r.asc.writesList()) != 3 || !strings.Contains(r.lastText(), "已经提交过") {
		t.Fatalf("重复回复：%v %s", r.asc.writesList(), r.lastText())
	}
	// 本地记着刚提交：release check 距上次提交不满足
	r.asc.setList("/v1/reviewSubmissions", relSubmission("old", release.StateComplete, r.noon.Add(-30*24*time.Hour)))
	if out := r.mustRun("release", "check", "alpha"); !strings.Contains(out, "✗ "+release.CondGap) {
		t.Fatalf("%s", out)
	}
}

// 每天最多查一次；同一构建只提示一次。
func TestReleaseDailyOnceAndOnePromptPerBuild(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.prepOut(true, "1.2.0", relGoodChanges, "")
	r.startPrep()
	r.mustRun("tick")
	sent := len(r.tg.texts())
	calls := r.asc.countCalls("GET /v1/apps")
	r.mustRun("review", "--scheduled")
	if len(r.relJobs()) != 1 {
		t.Fatal("同一天不该再起")
	}
	if r.store().CheckDay == "" {
		t.Fatal("CheckDay 没记")
	}
	if n := r.asc.countCalls("GET /v1/apps"); n != calls {
		t.Fatalf("同一天又查了：%d → %d", calls, n)
	}
	// 第二天：查，但同一构建不再起、不再提示
	r.clockAt(r.noon.Add(24 * time.Hour))
	r.mustRun("review", "--scheduled")
	if n := r.asc.countCalls("GET /v1/apps"); n == calls {
		t.Fatal("第二天应查")
	}
	r.mustRun("tick")
	if len(r.relJobs()) != 1 || len(r.tg.texts()) != sent {
		t.Fatalf("同一构建只提示一次：%d %v", len(r.relJobs()), r.tg.texts())
	}
	if !strings.Contains(r.launchdLog()+r.mustRun("release", "check"), "这个构建的记录") {
		t.Error("check 应显示记录")
	}
}

// 默认关：不发请求、不起进程、不建文件；回复「发」也不提交；不回复的「发」照常走对话。
func TestReleaseDisabled(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, map[string]any{"release_assist_enabled": false, "review_enabled": false})
	r.prepOut(true, "1.2.0", relGoodChanges, "")
	r.mustRun("review", "--scheduled")
	r.mustRun("tick")
	if n := r.asc.countCalls(""); n != 0 || r.claudeRuns() != 0 {
		t.Fatalf("关着：%d 个请求，%d 个 claude", n, r.claudeRuns())
	}
	if _, err := os.Stat(filepath.Join(r.home, ".agentq", "release.json")); err == nil {
		t.Fatal("关着不该建文件")
	}
	if strings.Contains(r.mustRun("status"), "发版助手") {
		t.Fatal("关着 status 不多一行")
	}
	if d := config.Default(); d.ReleaseAssistEnabled {
		t.Fatal("默认应关")
	}
	r.seedOffered(900, nil)
	r.say(1, "发", 900)
	r.noWrites()
	if !strings.Contains(r.lastText(), "关着") {
		t.Fatalf("%s", r.lastText())
	}
}

// 条件不满足：check 显示 ✗，定点检查不起模型、不提示。查不出也算不满足。
func TestReleaseConditions(t *testing.T) {
	t.Parallel()
	bl := func(r *relEnv, text string) {
		if err := os.WriteFile(r.alphaP.BacklogPath(), []byte(text), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	day := 24 * time.Hour
	cases := []struct {
		name string
		cond string
		mut  func(r *relEnv)
	}{
		{"构建不满 3 天", release.CondBuild, func(r *relEnv) {
			r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-71*time.Hour), "VALID", false, "APP_STORE_ELIGIBLE")
		}},
		{"构建还在处理", release.CondBuild, func(r *relEnv) {
			r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-4*day), "PROCESSING", false, "APP_STORE_ELIGIBLE")
		}},
		{"构建过期", release.CondBuild, func(r *relEnv) {
			r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-4*day), "VALID", true, "APP_STORE_ELIGIBLE")
		}},
		{"只能内部测试", release.CondBuild, func(r *relEnv) {
			r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-4*day), "VALID", false, "INTERNAL_ONLY")
		}},
		{"上架资格读不出", release.CondBuild, func(r *relEnv) { r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-4*day), "VALID", false, "") }},
		{"没有构建", release.CondBuild, func(r *relEnv) { r.asc.setList("/v1/builds") }},
		{"构建接口 500", release.CondBuild, func(r *relEnv) { r.asc.failNext("/v1/builds", 500, 500, 500, 500, 500, 500) }},
		{"构建接口 401", release.CondBuild, func(r *relEnv) { r.asc.failNext("/v1/builds", 401, 401) }},
		{"有新崩溃", release.CondCrash, func(r *relEnv) { r.crash(r.noon.Add(-time.Hour)) }},
		{"崩溃接口 500", release.CondCrash, func(r *relEnv) {
			r.asc.failNext("/v1/apps/"+senseAppID+"/betaFeedbackCrashSubmissions", 500, 500, 500, 500, 500, 500)
		}},
		{"崩溃没有时间", release.CondCrash, func(r *relEnv) {
			r.asc.setList("/v1/apps/"+senseAppID+"/betaFeedbackCrashSubmissions", ascRes("betaFeedbackCrashSubmissions", "c", map[string]any{}))
		}},
		{"有验收意见", release.CondFeedback, func(r *relEnv) { bl(r, senseBacklog+"\n- [ ] 验收 账单 的意见 —— 按钮歪了\n") }},
		{"BACKLOG 没了", release.CondFeedback, func(r *relEnv) { os.Remove(r.alphaP.BacklogPath()) }},
		{"ASC 上 14 天内提交过", release.CondGap, func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", relSubmission("s", release.StateComplete, r.noon.Add(-14*day+time.Hour)))
		}},
		{"翻页后才看到最近的提交", release.CondGap, func(r *relEnv) {
			r.asc.mu.Lock()
			r.asc.pageSize = 1
			r.asc.mu.Unlock()
			r.asc.setList("/v1/reviewSubmissions", relSubmission("a", release.StateComplete, r.noon.Add(-300*day)), relSubmission("b", release.StateComplete, r.noon.Add(-200*day)), relSubmission("c", release.StateComplete, r.noon.Add(-3*day)))
		}},
		{"本地 14 天内提交过", release.CondGap, func(r *relEnv) {
			b, _ := json.Marshal(&release.ReleaseStore{LastSubmit: map[string]time.Time{senseAppID: r.noon.Add(-3 * day)}})
			os.WriteFile(filepath.Join(r.home, ".agentq", "release.json"), b, 0o600)
		}},
		{"本地记录坏了", release.CondGap, func(r *relEnv) { os.WriteFile(filepath.Join(r.home, ".agentq", "release.json"), []byte("{坏"), 0o600) }},
		{"提交记录接口 500", release.CondGap, func(r *relEnv) { r.asc.failNext("/v1/reviewSubmissions", 500, 500, 500, 500, 500, 500) }},
		{"提交记录翻不完", release.CondGap, func(r *relEnv) {
			r.asc.mu.Lock()
			r.asc.pageSize = 1
			r.asc.mu.Unlock()
			var many []map[string]any
			for i := 0; i < release.ReleaseSubPages+1; i++ {
				many = append(many, relSubmission(fmt.Sprint(i), release.StateComplete, r.noon.Add(-100*day)))
			}
			r.asc.setList("/v1/reviewSubmissions", many...)
		}},
		{"提交状态不认识", release.CondGap, func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", relSubmission("s", "BRAND_NEW_STATE", time.Time{}))
		}},
		{"提交时间看不懂", release.CondGap, func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", ascRes("reviewSubmissions", "s", map[string]any{"state": release.StateComplete, "submittedDate": "昨天"}))
		}},
		{"有进行中的提交", release.CondOpen, func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", relSubmission("s", "WAITING_FOR_REVIEW", r.noon.Add(-30*day)))
		}},
		{"有没提交的草稿", release.CondOpen, func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", relSubmission("s", release.StateReady, time.Time{}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRelEnv(t, nil)
			tc.mut(r)
			out := r.mustRun("release", "check", "alpha")
			if !strings.Contains(out, "✗ "+tc.cond) || !strings.Contains(out, "不满足，不提示") {
				t.Fatalf("应显示 ✗ %s：\n%s", tc.cond, out)
			}
			r.prepOut(true, "1.2.0", relGoodChanges, "")
			r.mustRun("review", "--scheduled")
			r.mustRun("tick")
			if len(r.relJobs()) != 0 || r.claudeRuns() != 0 {
				t.Fatalf("条件不满足不该起模型：%d", r.claudeRuns())
			}
			for _, m := range r.tg.texts() {
				if strings.Contains(m, "📦") {
					t.Fatalf("不该提示：%s", m)
				}
			}
			r.noWrites()
		})
	}
}

// 边界：正好 3 天、正好 14 天算满足；崩溃在上传之前不算；没提交时间的旧记录不算提交；条目已勾掉的验收意见不算；正文顺带提到不算。
func TestReleaseConditionBoundaries(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-72*time.Hour), "VALID", false, "APP_STORE_ELIGIBLE")
	r.crash(r.noon.Add(-73 * time.Hour))
	r.asc.setList("/v1/reviewSubmissions", relSubmission("s", release.StateComplete, r.noon.Add(-14*24*time.Hour)), relSubmission("draft", release.StateComplete, time.Time{}))
	os.WriteFile(r.alphaP.BacklogPath(), []byte("# 待办\n\n## 待做\n\n- [ ] 普通条目 —— 验收后按审查人的意见改\n\n<!-- - [ ] 验收 模板 的意见 -->\n\n## 已完成\n\n- [x] 验收 账单 的意见 —— 改完了\n"), 0o644)
	out := r.mustRun("release", "check", "alpha")
	if strings.Contains(out, "✗") || !strings.Contains(out, "条件都满足") {
		t.Fatalf("%s", out)
	}
	// 只读：不建 release.json、不写 ASC
	if _, err := os.Stat(filepath.Join(r.home, ".agentq", "release.json")); err == nil {
		t.Fatal("check 不该建文件")
	}
	r.noWrites()
	if r.claudeRuns() != 0 {
		t.Fatal("check 不该调模型")
	}
	// 项目名错、ASC 没配
	if out, code := r.run("release", "check", "ghost"); code == 0 || !strings.Contains(out, "没有对应项目") {
		t.Fatalf("%d %s", code, out)
	}
	os.RemoveAll(filepath.Join(r.home, ".appstoreconnect"))
	if out, code := r.run("release", "check"); code == 0 || !strings.Contains(out, "没配") {
		t.Fatalf("%d %s", code, out)
	}
}

// 没有能提交的子命令；所有拼法都不写 ASC。
func TestReleaseNoSubmitCommand(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	for _, args := range [][]string{{"release"}, {"release", "submit"}, {"release", "submit", "alpha"}, {"release", "发"}, {"release", "发", "alpha"}, {"release", "check", "alpha", "--submit"}, {"submit"}, {"发"}, {"release", "--submit"}} {
		if out, code := r.run(args...); code == 0 {
			t.Errorf("%v 应失败：%s", args, out)
		}
	}
	r.noWrites()
	if s := r.offer(); s.Status != release.OfferOffered {
		t.Fatalf("%+v", s)
	}
}

// 回复各种文字都不提交；不交给模型。
func TestReleaseOnlyExactWord(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	for i, text := range []string{"发吧", "发。", "发 ", " 发", "不发", "发发", "fa", "可以发", "发！", "「发」", "提交", "发\n"} {
		r.tg.push(10+i, 600+i, text, 900, ownerChat, r.noon)
	}
	r.mustRun("tick")
	r.noWrites()
	if r.claudeRuns() != 0 || len(r.readNotify().Inbox) != 0 {
		t.Fatalf("不该交给模型：%d %+v", r.claudeRuns(), r.readNotify().Inbox)
	}
	if o := r.offer(); o.Status != release.OfferOffered {
		t.Fatalf("%+v", o)
	}
	for _, m := range r.tg.texts() {
		if !strings.Contains(m, "没提交") {
			t.Errorf("回复：%s", m)
		}
	}
}

func TestReleaseRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		seed  func(o *release.ReleaseOffer, s *release.ReleaseStore)
		mut   func(r *relEnv)
		reply int64
		want  string
	}{
		{name: "过期", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.OfferedAt = o.OfferedAt.Add(-8 * 24 * time.Hour)
		}, want: "过期"},
		{name: "构建变了", mut: func(r *relEnv) {
			r.setBuild("build-2", "43", "1.2.1", r.noon.Add(-4*24*time.Hour), "VALID", false, "APP_STORE_ELIGIBLE")
		}, want: "构建变了"},
		{name: "新崩溃", mut: func(r *relEnv) { r.crash(r.noon.Add(-time.Minute)) }, want: release.CondCrash},
		{name: "14 天内提交过", mut: func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", relSubmission("s", release.StateComplete, r.noon.Add(-24*time.Hour)))
		}, want: release.CondGap},
		{name: "别处开了提交", mut: func(r *relEnv) {
			r.asc.setList("/v1/reviewSubmissions", relSubmission("s", release.StateReady, time.Time{}))
		}, want: release.CondOpen},
		{name: "验收意见", mut: func(r *relEnv) {
			os.WriteFile(r.alphaP.BacklogPath(), []byte(senseBacklog+"\n- [ ] 验收 账单 的意见\n"), 0o644)
		}, want: release.CondFeedback},
		{name: "版本没了", mut: func(r *relEnv) { r.asc.setList("/v1/apps/" + senseAppID + "/appStoreVersions") }, want: "没有 PREPARE_FOR_SUBMISSION"},
		{name: "版本挂了别的构建", mut: func(r *relEnv) { r.setVersion(relVersionID, "1.2.0", "build-9") }, want: "不是提示的那个构建"},
		{name: "版本号变了", mut: func(r *relEnv) { r.setVersion(relVersionID, "1.3.0", relBuildID) }, want: "版本变了"},
		{name: "两个准备中的版本", mut: func(r *relEnv) {
			v := ascRes("appStoreVersions", "v2", map[string]any{"versionString": "2.0", "appVersionState": "PREPARE_FOR_SUBMISSION"})
			r.asc.mu.Lock()
			p := "/v1/apps/" + senseAppID + "/appStoreVersions"
			r.asc.lists[p] = append(r.asc.lists[p], v)
			r.asc.mu.Unlock()
		}, want: "说不清"},
		{name: "ASC 401", mut: func(r *relEnv) { r.asc.failNext("/v1/apps", 401) }, want: "查不出"},
		{name: "ASC 500", mut: func(r *relEnv) { r.asc.failNext("/v1/apps", 500, 500, 500) }, want: "查不出"},
		{name: "私钥坏了", mut: func(r *relEnv) {
			os.WriteFile(filepath.Join(r.home, ".appstoreconnect", "AuthKey_TEST.p8"), []byte("坏的"), 0o600)
		}, want: "没提交"},
		{name: "App 对应不上", mut: func(r *relEnv) { os.Remove(filepath.Join(r.alphaP.Dir, "project.yml")) }, want: "对应不上"},
		{name: "消息 id 对不上", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) { o.MessageID = 901 }, want: "对不上记录"},
		{name: "记录不在", seed: func(o *release.ReleaseOffer, s *release.ReleaseStore) { s.Offers = nil }, want: "对不上记录"},
		{name: "已提交", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) { o.Status = release.OfferSubmitted }, want: "已经提交过"},
		{name: "提交中崩溃", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Phase, o.SubmissionID = release.OfferSubmitting, release.PhaseItem, "x"
		}, want: "不重复提交"},
		{name: "失败过", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Result = release.OfferFailed, "第 2 步失败"
		}, want: "不自动重试"},
		{name: "正在校验", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.CheckingAt = release.OfferChecking, relTestNoon()
		}, want: "还在校验"},
		{name: "还没发提示", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) { o.Status = release.OfferPrepared }, want: "失效"},
		{name: "条件变了的记录", seed: func(o *release.ReleaseOffer, _ *release.ReleaseStore) { o.Status = release.OfferStale }, want: "失效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRelEnv(t, nil)
			r.seedOffered(900, tc.seed)
			if tc.mut != nil {
				tc.mut(r)
			}
			r.say(1, "发", 900)
			r.noWrites()
			got := strings.Join(r.tg.texts(), "\n")
			if !strings.Contains(got, "没提交") || !strings.Contains(got, tc.want) {
				t.Fatalf("回复应含 %q：%s\n%s", tc.want, got, r.launchdLog())
			}
		})
	}
}

// 校验失败放回等回复：修好后用户再回一次能提交（每次都重新校验）；校验到一半崩溃的 10 分钟后能再回。
func TestReleaseRecheckRecovers(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	r.asc.failNext("/v1/apps", 500, 500, 500)
	r.say(1, "发", 900)
	r.noWrites()
	if o := r.offer(); o.Status != release.OfferOffered || o.LastErr == "" {
		t.Fatalf("%+v", o)
	}
	r.say(2, "发", 900)
	if len(r.asc.writesList()) != 3 {
		t.Fatalf("修好后再回应提交：%v\n%s", r.asc.writesList(), r.lastText())
	}
	// 校验中崩溃（记录停在 checking、时间很久以前）
	r2 := newRelEnv(t, nil)
	r2.seedOffered(900, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
		o.Status, o.CheckingAt = release.OfferChecking, r2.noon.Add(-time.Hour)
	})
	r2.say(1, "发", 900)
	if len(r2.asc.writesList()) != 3 {
		t.Fatalf("校验到一半崩溃后应能再回：%v", r2.asc.writesList())
	}
}

// 回复别的已登记消息说「发」：有等回复的提示时不提交，也不接到那件事的会话上。
func TestReleaseReplyToOtherThread(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	ns := r.readNotify()
	ns.Threads[901] = notify.Thread{Kind: chat.ThreadChat, SessionID: threadSession, At: r.noon}
	ns.Threads[902] = notify.Thread{Kind: notify.ThreadDecide, Project: "alpha", Fingerprint: "decide:alpha:x", At: r.noon}
	b, _ := json.Marshal(ns)
	os.WriteFile(filepath.Join(r.home, ".agentq", "notify.json"), b, 0o600)
	for i, id := range []int64{901, 902} {
		r.say(i+1, "发", id)
		r.noWrites()
		if got := r.lastText(); !strings.Contains(got, "请回复那条") {
			t.Fatalf("回复 %d：%s", id, got)
		}
	}
	if r.claudeRuns() != 0 {
		t.Fatal("不该交给模型")
	}
	if o := r.offer(); o.Status != release.OfferOffered {
		t.Fatalf("%+v", o)
	}
	// 没有等回复的提示时，「发」是普通消息，照常走（不写 ASC）
	r2 := newRelEnv(t, map[string]any{"paused": true})
	r2.seedOffered(900, func(o *release.ReleaseOffer, _ *release.ReleaseStore) { o.Status = release.OfferSubmitted })
	r2.say(1, "发", 0)
	r2.noWrites()
	if r2.claudeRuns() == 0 {
		t.Fatal("没有等回复的提示时应照常交给模型")
	}
}

// 不是用户本人（群里别人回）、带附件的「发」不提交。
func TestReleaseSenderAndAttachment(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	r.tg.push(1, 601, "发", 900, ownerChat+1, r.noon)
	r.mustRun("tick")
	r.noWrites()
	if !strings.Contains(r.lastText(), "配好的账号本人") {
		t.Fatalf("%s", r.lastText())
	}
	// 没有 from
	r.tg.mu.Lock()
	r.tg.updates = append(r.tg.updates, map[string]any{"update_id": 2, "message": map[string]any{"message_id": 602, "date": r.noon.Unix(), "text": "发", "chat": map[string]any{"id": ownerChat},
		"reply_to_message": map[string]any{"message_id": 900, "date": r.noon.Unix(), "text": "x", "chat": map[string]any{"id": ownerChat}}}})
	// 带图片、文字说明是「发」
	r.tg.updates = append(r.tg.updates, map[string]any{"update_id": 3, "message": map[string]any{"message_id": 603, "date": r.noon.Unix(), "caption": "发", "chat": map[string]any{"id": ownerChat}, "from": map[string]any{"id": ownerChat},
		"photo":            []map[string]any{{"file_id": "x", "file_size": 1}},
		"reply_to_message": map[string]any{"message_id": 900, "date": r.noon.Unix(), "text": "x", "chat": map[string]any{"id": ownerChat}}}})
	// 文字是「发」、又带了文件
	r.tg.updates = append(r.tg.updates, map[string]any{"update_id": 4, "message": map[string]any{"message_id": 604, "date": r.noon.Unix(), "text": "发", "chat": map[string]any{"id": ownerChat}, "from": map[string]any{"id": ownerChat},
		"document":         map[string]any{"file_id": "y", "file_size": 1},
		"reply_to_message": map[string]any{"message_id": 900, "date": r.noon.Unix(), "text": "x", "chat": map[string]any{"id": ownerChat}}}})
	r.tg.mu.Unlock()
	r.mustRun("tick")
	r.noWrites()
	if o := r.offer(); o.Status != release.OfferOffered {
		t.Fatalf("%+v", o)
	}
}

// 第 2、3 步失败如实报告半截状态；再回复不重试。
func TestReleaseSubmitFailures(t *testing.T) {
	t.Parallel()
	create, item, patch := "/v1/reviewSubmissions", "/v1/reviewSubmissionItems", "/v1/reviewSubmissions/"+relSubID
	cases := []struct {
		name   string
		mut    func(r *relEnv)
		writes int
		status string
		want   []string
	}{
		{"第 1 步 422", func(r *relEnv) { r.asc.failNext(create+"#post", 422) }, 1, release.OfferFailed, []string{"第 1 步", "没有留下东西", "不自动重试"}},
		{"第 1 步 500", func(r *relEnv) { r.asc.failNext(create+"#post", 500) }, 1, release.OfferFailed, []string{"第 1 步", "说不准"}},
		{"第 1 步 401", func(r *relEnv) { r.asc.failNext(create+"#post", 401) }, 1, release.OfferFailed, []string{"第 1 步", "401"}},
		{"第 2 步 409", func(r *relEnv) { r.asc.failNext(item, 409) }, 2, release.OfferFailed, []string{"第 2 步", "留下了审核提交 " + relSubID, "还没提交给 Apple"}},
		{"第 2 步 500", func(r *relEnv) { r.asc.failNext(item, 500) }, 2, release.OfferFailed, []string{"第 2 步", relSubID}},
		{"第 3 步 500 没提交上", func(r *relEnv) { r.asc.failNext(patch+"#patch", 500) }, 3, release.OfferFailed, []string{"第 3 步", "READY_FOR_REVIEW", relSubID}},
		{"第 3 步 500 其实提交上了", func(r *relEnv) {
			r.asc.failNext(patch+"#patch", 500)
			r.subGET.Store("WAITING_FOR_REVIEW")
		}, 3, release.OfferSubmitted, []string{"已提交审核", "WAITING_FOR_REVIEW"}},
		{"第 3 步 500 状态读不出", func(r *relEnv) {
			r.asc.failNext(patch, 500, 500, 500, 500) // PATCH 一次，之后读状态三次
		}, 3, release.OfferFailed, []string{"第 3 步", "读不出"}},
		{"第 3 步说成功但状态不对", func(r *relEnv) { r.patchS.Store("READY_FOR_REVIEW") }, 3, release.OfferFailed, []string{"结果不对", "READY_FOR_REVIEW"}},
		{"第 3 步 409", func(r *relEnv) { r.asc.failNext(patch+"#patch", 409) }, 3, release.OfferFailed, []string{"第 3 步", "409"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRelEnv(t, nil)
			r.methodFail()
			r.seedOffered(900, nil)
			tc.mut(r)
			r.say(1, "发", 900)
			if w := r.asc.writesList(); len(w) != tc.writes {
				t.Fatalf("写了 %v，应 %d 次\n%s", w, tc.writes, r.launchdLog())
			}
			got := r.lastText()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("回复缺 %q：%s", want, got)
				}
			}
			o := r.offer()
			if o.Status != tc.status {
				t.Fatalf("状态 %s，应 %s：%+v", o.Status, tc.status, o)
			}
			if !strings.Contains(r.nightly(), "发版助手") {
				t.Error("汇总没写")
			}
			// 调过第 3 步就算提交过（结果不明也算）
			if tc.writes == 3 && r.store().LastSubmit[senseAppID].IsZero() {
				t.Error("第 3 步调过应记本地提交时间")
			}
			if tc.writes < 3 && !r.store().LastSubmit[senseAppID].IsZero() {
				t.Error("没调第 3 步不算提交过")
			}
			// 再回复「发」：绝不重试
			r.say(2, "发", 900)
			if w := r.asc.writesList(); len(w) != tc.writes {
				t.Fatalf("重复回复又写了：%v", w)
			}
			if got := r.lastText(); !strings.Contains(got, "没提交") {
				t.Fatalf("重复回复：%s", got)
			}
		})
	}
}

// methodFail：failNext 的路径带 #post/#patch 时只对这种方法生效（/v1/reviewSubmissions 既有 GET 又有 POST）。
func (r *relEnv) methodFail() {
	r.asc.setHandle(func(w http.ResponseWriter, req *http.Request, body []byte) bool {
		key := req.URL.Path + "#" + strings.ToLower(req.Method)
		r.asc.mu.Lock()
		q := r.asc.fail[key]
		code := 0
		if len(q) > 0 {
			code = q[0]
			r.asc.fail[key] = q[1:]
		}
		r.asc.mu.Unlock()
		if code != 0 {
			ascJSON(w, code, ascErrBody(code))
			return true
		}
		return r.handleWrites(w, req, body)
	})
}

// 先落盘再调接口：每一步调接口时，磁盘上的记录已经写好了。
func TestReleasePersistBeforeCall(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	var seen []string
	var mu sync.Mutex
	r.asc.setHandle(func(w http.ResponseWriter, req *http.Request, body []byte) bool {
		if req.Method != http.MethodGet {
			s := r.store()
			o := s.Find(release.OfferID(senseAppID, relBuildID))
			mu.Lock()
			seen = append(seen, fmt.Sprintf("%s %s/%s/%s/%v", req.Method, o.Status, o.Phase, o.SubmissionID, !s.LastSubmit[senseAppID].IsZero()))
			mu.Unlock()
		}
		return r.handleWrites(w, req, body)
	})
	r.say(1, "发", 900)
	want := []string{"POST submitting/create//false", "POST submitting/item/" + relSubID + "/false", "PATCH submitting/submit/" + relSubID + "/true"}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, want) {
		t.Fatalf("%v", seen)
	}
}

// 记录落不了盘：一个写接口都不调。
func TestReleaseNoCallWhenCannotPersist(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	p := filepath.Join(r.home, ".agentq", "release.json")
	os.Remove(p)
	os.MkdirAll(p, 0o700) // 读不了也写不了
	r.say(1, "发", 900)
	r.noWrites()
	if !strings.Contains(r.lastText(), "读写不了") {
		t.Fatalf("%s", r.lastText())
	}
	// 内容坏了：备份、重置，之前的提示作废，14 天内不提示
	os.RemoveAll(p)
	os.WriteFile(p, []byte("{坏"), 0o600)
	r.say(2, "发", 900)
	r.noWrites()
	if !strings.Contains(r.lastText(), "对不上记录") {
		t.Fatalf("%s", r.lastText())
	}
	baks, _ := filepath.Glob(p + ".bad-*")
	if len(baks) != 1 {
		t.Fatalf("备份：%v", baks)
	}
	if s := r.store(); s.UnknownSince.IsZero() {
		t.Fatalf("%+v", s)
	}
	if out := r.mustRun("release", "check", "alpha"); !strings.Contains(out, "✗ "+release.CondGap) || !strings.Contains(out, "坏过") {
		t.Fatalf("%s", out)
	}
	// 14 天后恢复
	r.clockAt(r.noon.Add(15 * 24 * time.Hour))
	r.setBuild(relBuildID, "42", "1.2.0", r.noon.Add(-4*24*time.Hour), "VALID", false, "APP_STORE_ELIGIBLE")
	if out := r.mustRun("release", "check", "alpha"); strings.Contains(out, "✗ "+release.CondGap) {
		t.Fatalf("%s", out)
	}
}

// 一批里两条「发」、并发 tick：只提交一次。
func TestReleaseConcurrent(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, nil)
	r.tg.push(1, 601, "发", 900, ownerChat, r.noon)
	r.tg.push(2, 602, "发", 900, ownerChat, r.noon)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(agentqBin, "tick")
			var envs []string
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "HOME=") {
					envs = append(envs, kv)
				}
			}
			cmd.Env = append(append(envs, "HOME="+r.home, "AGENTQ_FAKE_LOG="+r.fakeLg), r.extra...)
			_ = cmd.Run()
		}()
	}
	wg.Wait()
	r.mustRun("tick")
	if w := r.asc.writesList(); len(w) != 3 {
		t.Fatalf("应只提交一次：%v", w)
	}
	if o := r.offer(); o.Status != release.OfferSubmitted {
		t.Fatalf("%+v", o)
	}
}

// 写接口超时：结果说不准，如实说，不重试。
func TestSubmitOfferTimeout(t *testing.T) {
	t.Parallel()
	a, f, _, _ := testASCApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	o := release.ReleaseOffer{ID: "x", Project: "alpha", Version: "1.2.0", AppID: senseAppID, Status: release.OfferChecking}
	b, _ := json.Marshal(&release.ReleaseStore{Offers: []*release.ReleaseOffer{&o}})
	os.WriteFile(a.Release.ReleasePath(), b, 0o600)
	c, err := a.ASC.NewASC()
	if err != nil {
		t.Fatal(err)
	}
	c.Http.Timeout = 50 * time.Millisecond
	f.mu.Lock()
	f.hang["/v1/reviewSubmissions"] = 3 * time.Second
	f.mu.Unlock()
	msg := a.Release.SubmitOffer(context.Background(), c, o, release.ReleaseVersion{ID: "v", Version: "1.2.0"})
	if !strings.Contains(msg, "第 1 步") || !strings.Contains(msg, "说不准") {
		t.Fatalf("%s", msg)
	}
	if n := f.countCalls("POST "); n != 1 {
		t.Fatalf("写请求超时不该重试：%d 次", n)
	}
	s, _ := a.Release.ReadRelease()
	if s.Find("x").Status != release.OfferFailed {
		t.Fatalf("%+v", s.Find("x"))
	}
	if !strings.HasPrefix(msg, "提交") || strings.Contains(msg, c.Token) {
		t.Fatalf("错误信息带了 token：%s", msg)
	}
}

// 准备没成：不提示，告诉用户；同一构建最多准备两次。
func TestReleasePrepFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  func(r *relEnv)
		mut  func(r *relEnv)
		want string
	}{
		{name: "任务说没准备好", out: func(r *relEnv) { r.prepOut(false, "", nil, "缺截图") }, want: "缺截图"},
		{name: "输出不合格", out: func(r *relEnv) {
			r.prepRaw(`{"type":"result","subtype":"success","is_error":false,"result":"都好了","session_id":"s"}`)
		}, want: "不合格"},
		{name: "多出字段", out: func(r *relEnv) {
			r.prepRaw(`{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s","structured_output":{"ready":true,"version":"1.2.0","changes":["a"],"blocker":"","submit":true}}`)
		}, want: "不合格"},
		{name: "版本号不合法", out: func(r *relEnv) { r.prepOut(true, "1.2.0; rm", relGoodChanges, "") }, want: "不合格"},
		{name: "任务失败", out: func(r *relEnv) { r.prepRaw(`{"type":"result","is_error":true,"result":"出错","session_id":"s"}`) }, want: "失败"},
		{name: "预算撞顶", out: func(r *relEnv) {
			r.prepRaw(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s"}`)
		}, want: "没准备好"},
		{name: "ASC 上没有准备中的版本", out: func(r *relEnv) { r.prepOut(true, "1.2.0", relGoodChanges, "") },
			mut: func(r *relEnv) { r.asc.setList("/v1/apps/" + senseAppID + "/appStoreVersions") }, want: "没有 PREPARE_FOR_SUBMISSION"},
		{name: "版本没挂构建", out: func(r *relEnv) { r.prepOut(true, "1.2.0", relGoodChanges, "") },
			mut: func(r *relEnv) { r.setVersion(relVersionID, "1.2.0", "") }, want: "还没挂构建"},
		{name: "版本挂了别的构建", out: func(r *relEnv) { r.prepOut(true, "1.2.0", relGoodChanges, "") },
			mut: func(r *relEnv) { r.setVersion(relVersionID, "1.2.0", "build-9") }, want: "挂的不是这个构建"},
		{name: "版本号对不上", out: func(r *relEnv) { r.prepOut(true, "1.2.1", relGoodChanges, "") }, want: "准备任务说的是"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRelEnv(t, nil)
			tc.out(r)
			if tc.mut != nil {
				tc.mut(r)
			}
			r.startPrep()
			r.mustRun("tick")
			for _, m := range r.tg.texts() {
				if strings.Contains(m, release.ReleaseReplyHint) {
					t.Fatalf("不该提示：%s", m)
				}
			}
			r.noWrites()
			o := r.offer()
			if o.Status != release.OfferPrepFailed || o.MessageID != 0 {
				t.Fatalf("%+v", o)
			}
			got := strings.Join(r.tg.texts(), "\n") + o.Result
			if !strings.Contains(got, "没准备好") && !strings.Contains(o.Result, tc.want) {
				t.Errorf("用户应看到原因：%s", got)
			}
			if !strings.Contains(o.Result, tc.want) && !strings.Contains(got, tc.want) {
				t.Errorf("原因应含 %q：%s", tc.want, got)
			}
		})
	}
	t.Run("最多准备两次", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.prepOut(false, "", nil, "缺截图")
		r.startPrep()
		r.clockAt(r.noon.Add(24 * time.Hour))
		r.startPrep()
		if r.offer().Tries != 2 {
			t.Fatalf("%+v", r.offer())
		}
		r.clockAt(r.noon.Add(48 * time.Hour))
		r.mustRun("review", "--scheduled")
		if len(r.relJobs()) != 2 {
			t.Fatalf("第三次不该起：%d", len(r.relJobs()))
		}
	})
}

// 准备期间条件变了：不提示。查不出留到下次；查得出再发。
func TestReleaseOfferRechecks(t *testing.T) {
	t.Parallel()
	hasHint := func(r *relEnv) bool {
		for _, m := range r.tg.texts() {
			if strings.Contains(m, release.ReleaseReplyHint) {
				return true
			}
		}
		return false
	}
	t.Run("新崩溃", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		r.startPrep()
		// startPrep 里的 tick 可能已经发了；这里种一个「准备好了还没发」的状态
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Changes, o.OfferedAt = release.OfferPrepared, relGoodChanges, time.Time{}
		})
		r.tg.mu.Lock()
		r.tg.sent = nil
		r.tg.mu.Unlock()
		r.crash(r.noon.Add(-time.Minute))
		r.mustRun("tick")
		if hasHint(r) {
			t.Fatal("不该提示")
		}
		if o := r.offer(); o.Status != release.OfferStale || !strings.Contains(o.Result, release.CondCrash) {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("接口失败留到下次", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Changes, o.OfferedAt = release.OfferPrepared, relGoodChanges, time.Time{}
		})
		r.asc.failNext("/v1/builds", 500, 500, 500)
		r.mustRun("tick")
		if hasHint(r) || r.offer().Status != release.OfferPrepared {
			t.Fatalf("%+v", r.offer())
		}
		r.mustRun("tick")
		if !hasHint(r) || r.offer().Status != release.OfferOffered {
			t.Fatalf("恢复后应发：%+v %v", r.offer(), r.tg.texts())
		}
	})
	t.Run("夜里不发", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Changes, o.OfferedAt = release.OfferPrepared, relGoodChanges, time.Time{}
		})
		r.clockAt(r.noon.Add(11*time.Hour + 30*time.Minute))
		r.mustRun("tick")
		if hasHint(r) || r.offer().Status != release.OfferPrepared {
			t.Fatal("23:30 不该发")
		}
		r.clockAt(r.noon.Add(20*time.Hour + 30*time.Minute))
		r.mustRun("tick")
		if !hasHint(r) {
			t.Fatal("08:30 应发")
		}
	})
	t.Run("发送失败下次再发", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Changes, o.OfferedAt = release.OfferPrepared, relGoodChanges, time.Time{}
		})
		r.tg.mu.Lock()
		r.tg.failSend = 1
		r.tg.mu.Unlock()
		r.mustRun("tick")
		if r.offer().Status != release.OfferPrepared {
			t.Fatalf("%+v", r.offer())
		}
		r.mustRun("tick")
		if r.offer().Status != release.OfferOffered {
			t.Fatalf("%+v", r.offer())
		}
	})
	t.Run("没配 Telegram", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Changes, o.OfferedAt = release.OfferPrepared, relGoodChanges, time.Time{}
		})
		os.Remove(filepath.Join(r.home, ".agentq", "secrets", "telegram.env"))
		out := r.mustRun("tick")
		if r.offer().Status != release.OfferPrepared || !strings.Contains(out, "没配 Telegram") {
			t.Fatalf("%+v %s", r.offer(), out)
		}
	})
	t.Run("过期标记", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(900, nil)
		r.mustRun("tick")
		if r.offer().Status != release.OfferOffered {
			t.Fatal("没到期")
		}
		r.clockAt(r.noon.Add(8 * 24 * time.Hour))
		r.mustRun("tick")
		if r.offer().Status != release.OfferExpired {
			t.Fatalf("%+v", r.offer())
		}
		r.say(1, "发", 900)
		r.noWrites()
		if !strings.Contains(r.lastText(), "过期") {
			t.Fatalf("%s", r.lastText())
		}
	})
}

// 回复过期边界：正好 7 天内能提交。
func TestReleaseTTLBoundary(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.seedOffered(900, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
		o.OfferedAt = r.noon.Add(-release.ReleaseOfferTTL + time.Hour)
	})
	r.say(1, "发", 900)
	if len(r.asc.writesList()) != 3 {
		t.Fatalf("7 天内应能提交：%v %s", r.asc.writesList(), r.lastText())
	}
	// 进程时钟没到期、消息时间已经过了 7 天（时钟被拨回）：也算过期
	r2 := newRelEnv(t, nil)
	r2.seedOffered(900, func(o *release.ReleaseOffer, _ *release.ReleaseStore) { o.OfferedAt = r2.noon.Add(-time.Hour) })
	r2.tg.push(1, 601, "发", 900, ownerChat, r2.noon.Add(8*24*time.Hour))
	r2.mustRun("tick")
	r2.noWrites()
}

// 提交路径唯一：源码里的调用关系。
func TestSubmitPathIsSingle(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("%v %v", files, err)
	}
	if err := filepath.WalkDir("internal", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	callers := map[string]map[string]bool{}
	lowerFirst := func(s string) string { return strings.ToLower(s[:1]) + s[1:] } // 按函数名比，包外调用的名字是导出的
	note := func(callee, caller string) {
		callee, caller = lowerFirst(callee), lowerFirst(caller)
		if callers[callee] == nil {
			callers[callee] = map[string]bool{}
		}
		callers[callee][caller] = true
	}
	var writeLits []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range parsed.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						note(sel.Sel.Name, fn.Name.Name)
					}
					if id, ok := x.Fun.(*ast.Ident); ok {
						note(id.Name, fn.Name.Name)
					}
				case *ast.SelectorExpr:
					// 函数当值传的也算（如 a.releaseIntercept 不加括号）
					if name := lowerFirst(x.Sel.Name); name == "submitOffer" || name == "submitFromReply" || name == "releaseIntercept" {
						note(x.Sel.Name, fn.Name.Name)
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "/v1/reviewSubmission") {
						writeLits = append(writeLits, f+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	only := func(callee string, want ...string) {
		t.Helper()
		var got []string
		for c := range callers[callee] {
			got = append(got, c)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s 只能被 %v 调用，实际 %v", callee, want, got)
		}
	}
	only("submitOffer", "submitFromReply")
	only("submitFromReply", "releaseIntercept")
	only("releaseIntercept", "takeUpdates")
	only("takeUpdates", "takeUpdatesSafe")
	// 按钮：回调路径到不了提交代码，发消息带按钮只有决策卡片一处
	only("handleCallback", "takeUpdates")
	only("tgSendMarkup", "tgSend", "sendDecideCards")
	forward := map[string][]string{}
	for callee, from := range callers {
		for c := range from {
			forward[c] = append(forward[c], callee)
		}
	}
	reach := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if !reach[n] {
			reach[n] = true
			for _, m := range forward[n] {
				walk(m)
			}
		}
	}
	walk("handleCallback")
	if !reach["routerDecide"] {
		t.Error("按钮要走 routerDecide（调用图没算对？）")
	}
	for _, bad := range []string{"submitOffer", "submitFromReply", "releaseIntercept", "send", "roundTrip", "runRouterAction", "startNew"} {
		if reach[bad] {
			t.Errorf("回调路径能走到 %s", bad)
		}
	}
	// 写请求只有 send，send 只在 submitOffer 里调；roundTrip 只有读和 send 用
	only("send", "submitOffer")
	only("roundTrip", "getURL", "send")
	// 提交接口的路径只在 release.go（查有没有进行中的提交，只读）和 release_submit.go
	for _, w := range writeLits {
		if file, _, _ := strings.Cut(w, ":"); file != "internal/release/release.go" && file != "internal/release/release_submit.go" {
			t.Errorf("提交接口的路径出现在 %s", w)
		}
	}
	// 写接口的方法只在 release_submit.go
	readOnly, err := filepath.Glob("internal/asc/*.go")
	if err != nil {
		t.Fatal(err)
	}
	inRelease, err := filepath.Glob("internal/release/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(readOnly, inRelease...) {
		if strings.HasSuffix(f, "_test.go") || f == "internal/release/release_submit.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range []string{"MethodPost", "MethodPatch", "MethodPut", "MethodDelete", `"POST"`, `"PATCH"`} {
			if bytes.Contains(b, []byte(m)) {
				t.Errorf("%s 里有写方法 %s", f, m)
			}
		}
	}
	// Router 的动作、任务提示里没有提交；prompts 也没有
	for _, bad := range []string{"submit", "release", "reviewSubmission"} {
		if strings.Contains(chat.RouterSchema, bad) {
			t.Errorf("Router 的动作里有 %s", bad)
		}
		if strings.Contains(sense.SenseSchema, bad) || strings.Contains(suggest.SuggestSchema, bad) || strings.Contains(chat.FollowupSchema, bad) {
			t.Errorf("结构化输出里有 %s", bad)
		}
	}
	prompts, _ := filepath.Glob("prompts/*")
	for _, p := range prompts {
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte("reviewSubmission")) {
			t.Errorf("%s 提到了提交接口", p)
		}
	}
	// 没有 CLI 子命令能到达：cmdRelease 不调用 claimOffer、submit*
	for caller := range callers["claimOffer"] {
		if caller != "submitFromReply" {
			t.Errorf("claimOffer 被 %s 调用", caller)
		}
	}
}

// 对话和 Router 走不到提交：有等回复的提示时，别的话照常交给模型，也写不了 ASC。
func TestReleaseChatAndRouterCannotSubmit(t *testing.T) {
	t.Parallel()
	for _, router := range []bool{false, true} {
		t.Run(fmt.Sprintf("router=%v", router), func(t *testing.T) {
			t.Parallel()
			r := newRelEnv(t, map[string]any{"router_enabled": router, "paused": true})
			r.seedOffered(900, nil)
			r.say(1, "发布那个版本吧", 0)
			r.noWrites()
			if r.claudeRuns() == 0 {
				t.Fatal("别的话应照常交给模型")
			}
			if o := r.offer(); o.Status != release.OfferOffered {
				t.Fatalf("%+v", o)
			}
			// 提示词写明提交不归它
			logs, _ := filepath.Glob(filepath.Join(r.fakeLg, "*.txt"))
			got := ""
			for _, f := range logs {
				b, _ := os.ReadFile(f)
				got += string(b)
			}
			if !strings.Contains(got, "提交 App Store 审核") {
				t.Errorf("提示词没写明：%s", textfmt.TruncRunes(got, 300))
			}
		})
	}
}

// 准备任务占名额：名额满了不起，明天再查；暂停时不起。
func TestReleaseRespectsGates(t *testing.T) {
	t.Parallel()
	t.Run("暂停", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, map[string]any{"paused": true})
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		r.mustRun("review", "--scheduled")
		if len(r.relJobs()) != 0 || r.claudeRuns() != 0 {
			t.Fatal("暂停不该起")
		}
		if !strings.Contains(r.launchdLog()+r.mustRun("release", "check"), "条件都满足") {
			t.Fatal("check 应显示满足")
		}
		if r.store().Find(release.OfferID(senseAppID, relBuildID)) != nil {
			t.Fatal("没起任务不该留记录，明天还能起")
		}
	})
	t.Run("额度高", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.setUsage("You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 95% used\nCurrent week (all models): 20% used\n")
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		r.mustRun("review", "--scheduled")
		if len(r.relJobs()) != 0 {
			t.Fatal("额度高不该起")
		}
	})
	t.Run("项目在跑任务", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		if err := os.WriteFile(r.alphaP.BacklogPath(), []byte("# 待办\n\n## 待做\n\n- [ ] 有活 —— SLEEP=3\n\n## 已完成\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testkit.GitT(t, r.home, r.alphaP.Dir, "commit", "-qam", "加一条活")
		r.mustRun("run", "alpha")
		out := r.mustRun("review", "--scheduled")
		if len(r.relJobs()) != 0 || !strings.Contains(out, "明天再查") {
			t.Fatalf("名额满了不该起：%s", out)
		}
		if r.store().Find(release.OfferID(senseAppID, relBuildID)) != nil {
			t.Fatal("没起任务不该留记录")
		}
	})
	t.Run("准备任务的记录丢了", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Tries, o.UpdatedAt, o.OfferedAt = release.OfferPreparing, 1, r.noon.Add(-48*time.Hour), time.Time{}
		})
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		r.startPrep()
		if r.offer().Tries != 2 {
			t.Fatalf("%+v", r.offer())
		}
	})
	t.Run("准备任务还在跑", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		r.seedOffered(0, func(o *release.ReleaseOffer, _ *release.ReleaseStore) {
			o.Status, o.Tries, o.UpdatedAt, o.OfferedAt = release.OfferPreparing, 1, r.noon.Add(-time.Hour), time.Time{}
		})
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		r.mustRun("review", "--scheduled")
		if len(r.relJobs()) != 0 {
			t.Fatal("不该再起一个")
		}
	})
}

// 撞预算只留一条提醒：不走通用收尾，也不算连环失败。
func TestReleasePrepBudgetOneNotice(t *testing.T) {
	t.Parallel()
	r := newRelEnv(t, nil)
	r.prepRaw(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s"}`)
	j := r.startPrep()
	if j.BudgetHit || j.ResumePending || !j.LimitHit || j.Status != state.StatusFailed {
		t.Fatalf("%+v", j)
	}
	if !strings.Contains(j.Summary, "撞到预算上限") {
		t.Fatalf("%s", j.Summary)
	}
	r.mustRun("tick")
	all := strings.Join(r.tg.texts(), "\n")
	b, _ := os.ReadFile(filepath.Join(r.home, ".agentq", "notify.json"))
	if strings.Contains(string(b), "budget:") {
		t.Fatalf("不该有通用的预算提醒：%s", b)
	}
	got := all
	if !strings.Contains(got, "发版没准备好") {
		var ns notify.State
		json.Unmarshal(b, &ns)
		for _, n := range ns.Pending {
			got += n.Text
		}
	}
	if strings.Count(got, "发版没准备好") != 1 {
		t.Fatalf("应只有一条提醒：%s", got)
	}
	if o := r.offer(); o.Status != release.OfferPrepFailed || !strings.Contains(o.Result, "撞到预算上限") {
		t.Fatalf("%+v", o)
	}
}

// 提示发不出去就不起准备任务：起了也回复不了，白花钱。
func TestReleaseNoPromptChannelNoPrep(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, r *relEnv, want string) {
		t.Helper()
		r.prepOut(true, "1.2.0", relGoodChanges, "")
		out := r.mustRun("review", "--scheduled")
		if len(r.relJobs()) != 0 || r.claudeRuns() != 0 {
			t.Fatalf("不该起准备任务：%s", out)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("日志应说 %q：%s", want, out)
		}
		if r.store().Find(release.OfferID(senseAppID, relBuildID)) != nil {
			t.Fatal("没起任务不该留记录，配好之后还能起")
		}
	}
	t.Run("没配 Telegram", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		os.Remove(filepath.Join(r.home, ".agentq", "secrets", "telegram.env"))
		check(t, r, "没配 Telegram")
	})
	t.Run("凭据坏了", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, nil)
		os.Chmod(filepath.Join(r.home, ".agentq", "secrets", "telegram.env"), 0o644)
		check(t, r, "提示发不出去")
	})
	t.Run("推送关着", func(t *testing.T) {
		t.Parallel()
		r := newRelEnv(t, map[string]any{"notify_enabled": false})
		check(t, r, "notify_enabled=false")
	})
}

// relTestNoon 是 newRelEnv 假时钟的起点。种子数据的时间要和假时钟对齐，不能用真实时间：上午跑时两者差几小时，「刚开始校验」会被当成早就超时。
func relTestNoon() time.Time {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, 12, 0, 0, 0, time.Local)
}
