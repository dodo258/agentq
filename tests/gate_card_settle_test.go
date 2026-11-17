package tests

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// gateCardEnv：一个停在门上的任务，门卡片已经推出去。返回按钮和卡片那条消息的 id。
func gateCardEnv(t *testing.T) (*notifyEnv, *Job, []inlineButton, int64) {
	t.Helper()
	e := newNotifyEnv(t, true)
	j := &Job{ID: "1117-000000-demo", Project: "demo", Title: "修东西", Status: state.StatusDone, StartedAt: e.now,
		Gate: &state.GateRecord{State: state.GateStuck, Branch: "agent/t-a", Head: "0123456789abcdef"}}
	e.st.Jobs = append(e.st.Jobs, j)
	os.MkdirAll(e.a.Gate.Dir(j.ID), 0o700)
	e.flush() // 第一次只记下现有的拍板条目
	opts := []notify.DecideOption{{Label: "A", Text: "放行"}, {Label: "B", Text: "退回"}, {Label: "C", Text: "放弃"}, {Label: "D", Text: "再审一次"}}
	e.a.Notify.Notify(notify.Notice{Cat: "decide", Key: "gate:" + j.ID, JobID: j.ID, Text: "demo　「修东西」门没过，等你定",
		Card: &notify.DecideCard{Project: j.Project, Title: j.Title, Recommend: "A", GateJob: j.ID, Options: opts}})
	e.flush()
	sent := e.tg.sentLastWith("门没过")
	btns := keyboardOf(t, sent)
	if len(btns) != 4 {
		t.Fatalf("%+v", btns)
	}
	id := e.tg.msgIDWith("门没过")
	for _, c := range e.state().Cards {
		if c.GateJob == j.ID && c.MessageID != id {
			t.Fatalf("卡片要记下消息号 %d：%+v", id, c)
		}
	}
	return e, j, btns, id
}

func settledEdit(t *testing.T, e *notifyEnv, msgID int64) string {
	t.Helper()
	for _, ed := range e.tg.edits {
		if ed.Get("message_id") == strconv.FormatInt(msgID, 10) {
			if ed.Get("reply_markup") != `{"inline_keyboard":[]}` {
				t.Errorf("要去掉按钮：%v", ed)
			}
			return textfmt.HTMLToPlain(ed.Get("text"))
		}
	}
	t.Fatalf("卡片消息 %d 没改：%v", msgID, e.tg.edits)
	return ""
}

// 命令行放行后、tick 落决定前点卡片：回已经定过（谁定的、选了什么），消息改成结论，不改写命令行的决定。
func TestGateCardAfterCommandPass(t *testing.T) {
	e, j, btns, msgID := gateCardEnv(t)
	if err := e.a.Gate.CmdGate([]string{"pass", j.ID, "--reason", "命令行看过了"}); err != nil {
		t.Fatal(err)
	}
	e.tg.click(1, ownerChat, int(msgID), btns[2].Data) // 点「放弃」
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已经定过了：在命令行或会话里选了放行（命令行看过了）") {
		t.Errorf("%s", a)
	}
	if text := settledEdit(t, e, msgID); !strings.Contains(text, "已经定过了") || !strings.Contains(text, "选了放行") {
		t.Errorf("%s", text)
	}
	if act, why, ok := e.a.Gate.PendingDecision(j.ID); !ok || act != gate.DecidePass || why != "命令行看过了" {
		t.Errorf("命令行的决定不能被按钮改掉：%s %s %v", act, why, ok)
	}
	// 再点：照样回已经定过，不再改消息
	n := len(e.tg.edits)
	e.tg.click(2, ownerChat, int(msgID), btns[0].Data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.HasPrefix(a, "已经定过了：") || len(e.tg.edits) != n {
		t.Errorf("%s %d", a, len(e.tg.edits))
	}
}

// 决定在 tick 里落了（任务排队合并），那次推送就把卡片改成结论；之后点也回已经定过，不再写决定。
func TestGateCardSettledAtTick(t *testing.T) {
	e, j, btns, msgID := gateCardEnv(t)
	j.Gate.State, j.Gate.Override = state.GateQueued, "命令行看过了" // 门落了命令行的放行
	e.flush()
	if text := settledEdit(t, e, msgID); !strings.Contains(text, "已经定过了") || !strings.Contains(text, "在命令行或会话里选了放行（命令行看过了），等合并") {
		t.Errorf("%s", text)
	}
	e.flush()
	if len(e.tg.edits) != 1 {
		t.Errorf("只改一次：%v", e.tg.edits)
	}
	e.tg.click(1, ownerChat, int(msgID), btns[1].Data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已经定过了：在命令行或会话里选了放行") {
		t.Errorf("%s", a)
	}
	if _, _, ok := e.a.Gate.PendingDecision(j.ID); ok {
		t.Error("定过了不再写决定")
	}
	if strings.Contains(e.out.String(), "没记下") {
		t.Errorf("%s", e.out.String())
	}
}

// 没登记消息号的老卡片：tick 只记下结论不改消息，点了照样回已经定过；放弃了、退回了各说各的。
func TestGateCardSettledOldCard(t *testing.T) {
	e, j, btns, msgID := gateCardEnv(t)
	if err := e.a.Notify.WithNotify(func(s *notify.State) error {
		for _, c := range s.Cards {
			c.MessageID = 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j.Gate.State, j.Gate.ReturnWhy = state.GateReturned, "测试没写"
	e.flush()
	if len(e.tg.edits) != 0 {
		t.Errorf("没有消息号不改：%v", e.tg.edits)
	}
	e.tg.click(1, ownerChat, int(msgID), btns[0].Data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已经定过了：在命令行或会话里选了退回（测试没写）") {
		t.Errorf("%s", a)
	}
}

// 卡片还有效时点：照旧写给门，回已定，消息改成已定。
func TestGateCardStillValid(t *testing.T) {
	e, j, btns, msgID := gateCardEnv(t)
	e.tg.click(1, ownerChat, int(msgID), btns[0].Data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已定：A 放行") {
		t.Errorf("%s", a)
	}
	if act, why, ok := e.a.Gate.PendingDecision(j.ID); !ok || act != gate.DecidePass || why != notify.GatePassByCard {
		t.Errorf("%s %s %v", act, why, ok)
	}
	if text := settledEdit(t, e, msgID); !strings.Contains(text, "已定") || !strings.Contains(text, "选了 A 放行") {
		t.Errorf("%s", text)
	}
	// 门落了按钮的放行：卡片已经定了，tick 不再改它
	j.Gate.State, j.Gate.Override = state.GateQueued, notify.GatePassByCard
	e.flush()
	if len(e.tg.edits) != 1 {
		t.Errorf("%v", e.tg.edits)
	}
}

// 真二进制：卡住的任务用 agentq gate pass 放行，下一次 tick 落决定、合并，同一次 tick 把卡片改成结论；再点回已经定过。
func TestGateCardSettledByCommandE2E(t *testing.T) {
	t.Parallel()
	e, tg := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	os.WriteFile(filepath.Join(dir, ".agentq", "verify"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "门")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
	e.waitGate(e.jobByTitle(title))
	e.editState(func(st *State) {
		for _, x := range st.Jobs {
			if x.Title == title && x.Gate != nil {
				x.Gate.Round, x.Gate.State, x.ResumePending = 2, state.GateQueued, false
			}
		}
	})
	e.mustRun("tick")
	e.waitGate(e.jobByTitle(title))
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	btns := keyboardOf(t, tg.sentLastWith("门没过"))
	msgID := tg.msgIDWith("门没过")
	e.mustRun("gate", "pass", j.ID, "--reason", "命令行看过了")
	e.mustRun("tick") // 落决定，同一次推送收起卡片
	var edit string
	for _, ed := range tg.edits {
		if ed.Get("message_id") == strconv.FormatInt(msgID, 10) && ed.Get("reply_markup") == `{"inline_keyboard":[]}` {
			edit = textfmt.HTMLToPlain(ed.Get("text"))
		}
	}
	if !strings.Contains(edit, "已经定过了") || !strings.Contains(edit, "在命令行或会话里选了放行（命令行看过了）") {
		t.Fatalf("落决定那次 tick 要把卡片改成结论：%q\n%s", edit, e.launchdLog())
	}
	tg.click(70, ownerChat, int(msgID), btns[2].Data)
	e.mustRun("tick")
	if a := tg.lastAnswer(); !strings.Contains(a, "已经定过了：在命令行或会话里选了放行") {
		t.Errorf("%s", a)
	}
	e.waitGate(e.jobByTitle(title))
	if j = e.jobByTitle(title); j.Gate.State == state.GateAbandoned {
		t.Errorf("点了放弃不能生效：%+v", j.Gate)
	}
}
