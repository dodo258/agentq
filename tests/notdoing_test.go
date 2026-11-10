package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
)

// ndEnv：一个带「建议」的真仓库，编号 1、2 在索引里。
func ndEnv(t *testing.T) (*notifyEnv, backlog.Project) {
	t.Helper()
	e := newNotifyEnv(t, true)
	os.WriteFile(filepath.Join(e.a.Home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	p := sgRepo(t, e.a, e.root, "demo", "# 待办\n\n## 待做\n\n- [ ] 已有\n\n## 建议\n\n"+
		"- [ ] 加夜间模式 —— 用户要；好处：好看；风险：低（建议 1）\n- [ ] 加云同步 —— 用户要（建议 2）\n\n## 已完成\n")
	ls := strings.Split(backlogOf(t, p), "\n")
	var fp1, fp2 string
	for _, l := range ls {
		if strings.Contains(l, "（建议 1）") {
			fp1 = backlog.Fingerprint(l)
		}
		if strings.Contains(l, "（建议 2）") {
			fp2 = backlog.Fingerprint(l)
		}
	}
	store := suggest.SuggestStore{Next: 3, Items: []suggest.SuggestEntry{
		{N: 1, Project: "demo", Fingerprint: fp1, Title: "加夜间模式", CreatedAt: e.now},
		{N: 2, Project: "demo", Fingerprint: fp2, Title: "加云同步", CreatedAt: e.now},
	}}
	b, _ := json.Marshal(store)
	if err := os.WriteFile(e.a.Suggest.SuggestionsPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// 回「不做 1」：不起模型；从「建议」拿掉，写进不做清单，两个文件一次提交；编号作废，再说一次算失效；另一条不动。
func TestNotDoFastPath(t *testing.T) {
	e, p := ndEnv(t)
	before := commits(t, e.a, p)
	e.tg.msg(1, ownerChat, 11, "不做 1", "")
	e.flush()
	if len(e.chats) != 0 || len(e.tg.acks()) != 0 {
		t.Fatalf("不该起模型：%v %q", e.chats, e.tg.acks())
	}
	got := backlogOf(t, p)
	if strings.Contains(got, "加夜间模式") || !strings.Contains(got, "加云同步") {
		t.Errorf("%s", got)
	}
	entries, err := backlog.NotDoingEntries(p)
	if err != nil || len(entries) != 1 {
		t.Fatalf("%+v %v", entries, err)
	}
	if entries[0].Title != "加夜间模式" || !strings.Contains(entries[0].Quote, "不做 1") || !strings.Contains(entries[0].Quote, "用户要") || entries[0].Date != "2026-10-11" {
		t.Errorf("%+v", entries[0])
	}
	if strings.Contains(entries[0].Quote, "（建议 1）") {
		t.Errorf("编号标记不进原话：%s", entries[0].Quote)
	}
	if commits(t, e.a, p) != before+1 {
		t.Error("要一次提交")
	}
	if out := testkit.GitT(t, e.a.Home, p.Dir, "status", "--porcelain"); out != "" {
		t.Errorf("都要提交进去：%s", out)
	}
	if out := testkit.GitT(t, e.a.Home, p.Dir, "show", "--name-only", "--format=", "HEAD"); !strings.Contains(out, "docs/BACKLOG.md") || !strings.Contains(out, "docs/NOT-DOING.md") {
		t.Errorf("%s", out)
	}
	if text := e.tg.sentLast().Get("text"); !strings.Contains(text, "已写进「不做」清单") || !strings.Contains(text, "加夜间模式") {
		t.Errorf("%s", text)
	}
	store, _ := e.a.Suggest.LoadSuggest()
	if store.Find(1) != nil || store.Find(2) == nil || len(store.Past) != 1 || store.Past[0].Title != "加夜间模式" {
		t.Errorf("编号作废、标题留 past：%+v", store)
	}
	// 「不做 1 2」：1 失效、2 生效
	e.tg.msg(2, ownerChat, 12, "不做 1 2", "")
	e.flush()
	text := e.tg.sentLast().Get("text")
	if !strings.Contains(text, "加云同步") || !strings.Contains(text, "编号失效") {
		t.Errorf("%s", text)
	}
	if entries, _ := backlog.NotDoingEntries(p); len(entries) != 2 {
		t.Errorf("%+v", entries)
	}
	// 全部无效：放行给模型，什么都不动
	n := commits(t, e.a, p)
	e.tg.msg(3, ownerChat, 13, "不做 1 9", "")
	e.flush()
	if len(e.chats) != 1 || commits(t, e.a, p) != n {
		t.Errorf("全部无效要放行给模型：%v", e.chats)
	}
}

// 带附件、回复已登记的消息：不拦截。
func TestNotDoNotIntercepted(t *testing.T) {
	e, p := ndEnv(t)
	e.tg.media(1, 11, "photo", "不做 1", "", []byte("x"), 0, "")
	e.flush()
	if len(e.chats) != 1 {
		t.Errorf("带附件交给模型：%v", e.chats)
	}
	if _, err := os.Stat(p.NotDoingPath()); err == nil {
		t.Error("不该写")
	}
}

// 主目录脏：不写清单、建议留着、索引留着，回复说没能移。
func TestNotDoCommitRefused(t *testing.T) {
	e, p := ndEnv(t)
	orig := backlogOf(t, p)
	dirty := orig + "用户正在改\n"
	os.WriteFile(p.BacklogPath(), []byte(dirty), 0o644)
	e.tg.msg(1, ownerChat, 11, "不做 1", "")
	e.flush()
	if text := e.tg.sentLast().Get("text"); !strings.Contains(text, "没能移") {
		t.Errorf("%s", text)
	}
	if backlogOf(t, p) != dirty {
		t.Error("不能动用户的改动")
	}
	if _, err := os.Stat(p.NotDoingPath()); err == nil {
		t.Error("不该写")
	}
	if store, _ := e.a.Suggest.LoadSuggest(); store.Find(1) == nil {
		t.Error("下次还能做")
	}
	os.WriteFile(p.BacklogPath(), []byte(orig), 0o644)
	e.tg.msg(2, ownerChat, 12, "不做 1", "")
	e.flush()
	if entries, _ := backlog.NotDoingEntries(p); len(entries) != 1 {
		t.Errorf("%+v", entries)
	}
}

// 提交被钩子拒绝：两个文件都改回去，暂存区也干净；新文件不留。
func TestCommitNotDoingRollback(t *testing.T) {
	e, p := ndEnv(t)
	hook := filepath.Join(p.Dir, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := backlogOf(t, p)
	entry := &backlog.NotDoingEntry{Title: "加夜间模式", Quote: "不要", Date: "2026-11-04"}
	err := e.a.Suggest.CommitNotDoing(p, "测试", entry, func(old string) (string, error) {
		text, _, err := suggest.RemoveSuggestion(old, "", "加夜间模式")
		return text, err
	})
	if err == nil {
		t.Fatal("钩子拒绝要报错")
	}
	if backlogOf(t, p) != orig {
		t.Error("BACKLOG 要改回去")
	}
	if _, err := os.Stat(p.NotDoingPath()); err == nil {
		t.Error("新文件要删掉")
	}
	if out := testkit.GitT(t, e.a.Home, p.Dir, "status", "--porcelain"); out != "" {
		t.Errorf("暂存区要干净：%q", out)
	}
	// 已有的文件：内容也要恢复
	os.Remove(hook)
	if err := e.a.Suggest.CommitNotDoing(p, "先写一条", &backlog.NotDoingEntry{Title: "旧的", Quote: "q", Date: "d"}, func(old string) (string, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}
	was, _ := os.ReadFile(p.NotDoingPath())
	os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if err := e.a.Suggest.CommitNotDoing(p, "再写", &backlog.NotDoingEntry{Title: "新的", Quote: "q", Date: "d"}, func(old string) (string, error) { return old, nil }); err == nil {
		t.Fatal("要报错")
	}
	if now, _ := os.ReadFile(p.NotDoingPath()); string(now) != string(was) {
		t.Errorf("%s", now)
	}
	if out := testkit.GitT(t, e.a.Home, p.Dir, "status", "--porcelain"); out != "" {
		t.Errorf("%q", out)
	}
	// 同标题不重复写，也不空提交
	os.Remove(hook)
	n := commits(t, e.a, p)
	if err := e.a.Suggest.CommitNotDoing(p, "重复", &backlog.NotDoingEntry{Title: "旧 的!", Quote: "q", Date: "d"}, func(old string) (string, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}
	if commits(t, e.a, p) != n {
		t.Error("同标题不提交")
	}
}

// 清单按私有记录处理，和 BACKLOG-archive 一样。
func TestNotDoingIsPrivateRecord(t *testing.T) {
	b, err := os.ReadFile("config/clean-source.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Private []string `json:"private_paths"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range m.Private {
		found = found || p == suggest.NotDoingRel
	}
	if !found {
		t.Errorf("%v", m.Private)
	}
	doc, _ := os.ReadFile("docs/privacy.md")
	if !strings.Contains(string(doc), suggest.NotDoingRel) {
		t.Error("privacy.md 要写")
	}
}

func TestAgentTaskSkillMentionsNotDoingAndOptions(t *testing.T) {
	b, err := os.ReadFile("skills/agent-task/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{suggest.NotDoingRel, "（选项：A 做法一｜B 做法二；推荐 A）"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("技能缺 %s", want)
		}
	}
	if _, _, ok := notify.ParseDecideOptions("（选项：A 做法一｜B 做法二；推荐 A）"); !ok {
		t.Error("技能里写的格式要能解析")
	}
}
