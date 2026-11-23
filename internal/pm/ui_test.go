package pm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/gitx"
	"agentq/internal/testkit"
)

// 有界面的项目多一路界面和视觉，至少一条落在界面或易用性上，或在 ui 里写明看过了没问题。
func TestBuildPromptUI(t *testing.T) {
	p := BuildPrompt("demo", TriggerEmpty, nil, nil, &UI{Checklist: "1. 首屏能看出产品和题材。", Shots: []string{"docs/design/approved/home.png", "docs/shot 2.png"}})
	for _, want := range []string{"第 4 路 界面和视觉", "同类的界面怎么做", "用户对同类观感和易用性的抱怨", "逐条对照下面的审查表",
		"docs/design/approved/home.png、docs/shot 2.png", "问题的出处写截图路径", "审查表（no-ai-look）：\n1. 首屏能看出产品和题材。",
		"至少一条落在界面或易用性上", "在 ui 里写明看了哪些界面、为什么没问题", "ui 是界面这一路的结论"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	// 没截图、本机没装技能：按界面代码看，用内置的几条标准
	p = BuildPrompt("demo", TriggerEmpty, nil, nil, &UI{})
	for _, want := range []string{"第 4 路 界面和视觉", "没有现成截图，就读界面代码", "出处写文件路径和行号", "审查表：首屏能看出产品和题材"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	// 起调研前截了图：列出每张的路径，要求逐张看，建议指到哪张截图的哪里；项目里存的截图降为参考
	shots := []string{"/h/.agentq/jobs/1123-1-demo/screenshots/home.png", "/h/.agentq/jobs/1123-1-demo/screenshots/m/设置.png"}
	p = BuildPrompt("demo", TriggerEmpty, nil, nil, &UI{Shots: []string{"docs/old.png"}, Captured: shots, Missed: "1 张截图有问题：m/设置.png 是空白的"})
	for _, want := range []string{"在默认分支上跑 .agentq/ui 截了现在的界面，共 2 张，逐张用读文件打开看，每一张都要看",
		"- " + shots[0] + "\n", "- " + shots[1] + "\n", "界面建议必须指到具体哪张截图的哪里：source 写截图路径，why 写图上哪个位置、看到的是什么",
		"其中1 张截图有问题：m/设置.png 是空白的", "项目里还存着这些截图（可能过期，和刚截的对不上时以刚截的为准）：docs/old.png",
		"官网截图、README 里的图、应用商店页", "不凭印象描述别人的界面"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	if strings.Contains(p, "没有现成截图") || strings.Contains(p, "这次没截到图") {
		t.Errorf("截到了就不说没截图：\n%s", p)
	}
	// 没截到：写明原因，照旧看存的截图或界面代码
	p = BuildPrompt("demo", TriggerEmpty, nil, nil, &UI{Missed: ".agentq/ui 没过：退出码 1（日志 ~/x/ui.log）"})
	for _, want := range []string{"这次没截到图：.agentq/ui 没过：退出码 1（日志 ~/x/ui.log）。", "没有现成截图，就读界面代码"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	if strings.Contains(p, "逐张用读文件打开看") {
		t.Errorf("没截到不要求逐张看：\n%s", p)
	}
	if !strings.Contains(Schema, `"ui":{"type":"string"}`) {
		t.Errorf("结果要能带界面结论：%s", Schema)
	}
}

// 审查表从装好的技能里取那一节：拿仓库里真的 no-ai-look 验。
func TestChecklistSection(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "skills", "no-ai-look", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := ChecklistSection(string(b))
	if !strings.HasPrefix(got, "审查人对实际截图逐条判断") || !strings.Contains(got, "14. ") || strings.Contains(got, "## ") || strings.Contains(got, "字体与许可") {
		t.Fatalf("要正好是截图审查表那一节：\n%s", got)
	}
	if ChecklistSection("# 技能\n\n## 1. 动手前\n\n先想\n") != "" {
		t.Errorf("没有审查表那一节是空的")
	}
}

func uiService(t *testing.T) (*Service, string) {
	t.Helper()
	home := t.TempDir() // 默认后端是 Claude（import testkit 时注册），技能目录在 home/.claude/skills
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	e := &env.Env{Home: home, Cfg: config.Default()}
	return &Service{Env: e, Gitx: &gitx.Service{Env: e}}, home
}

func repo(t *testing.T, home string, files map[string]string) backlog.Project {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	os.MkdirAll(dir, 0o755)
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-qm", "init")
	return backlog.Project{Name: "demo", Dir: dir}
}

// 有没有界面：git 管的文件里有界面文件（界面门同一份后缀），或有截图命令 .agentq/ui；纯命令行、后端没有。
func TestUIOf(t *testing.T) {
	a, home := uiService(t)
	if ui := a.uiOf(repo(t, home, map[string]string{"main.go": "package main\n", "README.md": "# cli\n", "docs/arch.png": "x"})); ui != nil {
		t.Errorf("纯命令行项目不带界面那一路：%+v", ui)
	}
	if ui := a.uiOf(repo(t, home, map[string]string{"main.go": "package main\n", ".agentq/ui": "#!/bin/sh\n"})); ui == nil {
		t.Errorf("有截图命令算有界面")
	}
	skill := "# no-ai-look\n\n## 4. 字体\n\n别的\n\n## 5. 截图审查表\n\n1. 首屏能看出产品。\n\n## 6. 其他\n"
	os.MkdirAll(filepath.Join(home, ".claude", "skills", "no-ai-look"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "skills", "no-ai-look", "SKILL.md"), []byte(skill), 0o644)
	p := repo(t, home, map[string]string{"src/App.tsx": "x", "docs/screens/截图 1.png": "x", "docs/design/approved/home.png": "x",
		"assets/logo.png": "x", "ios/Screenshot-1.jpg": "x", "docs/notes.md": "x"})
	ui := a.uiOf(p)
	if ui == nil {
		t.Fatal("有 .tsx 算有界面")
	}
	if strings.Join(ui.Shots, "|") != "docs/design/approved/home.png|docs/screens/截图 1.png|ios/Screenshot-1.jpg" {
		t.Errorf("截图：基线在前，docs/ 下和带 screenshot、截图的图片；中文路径原样：%q", ui.Shots)
	}
	if ui.Checklist != "1. 首屏能看出产品。" {
		t.Errorf("审查表从装好的技能里取：%q", ui.Checklist)
	}
}
