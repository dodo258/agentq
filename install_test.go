package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/order"
)

// put 写一个文件，目录没有就建。
func put(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// read 读一个文件，没有是空。
func read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Telegram 密钥：新路径优先，没有就用旧版写好的；数据目录换了不看旧的。
func TestTelegramFile(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agentq2")
	file, old := filepath.Join(root, "telegram.env"), filepath.Join(home, ".agentq", "secrets", "telegram.env")
	want := func(when, root, want string) {
		t.Helper()
		if got := telegramFile(root, home); got != want {
			t.Errorf("%s：要 %s，得 %s", when, want, got)
		}
	}
	want("都没有", root, file)
	put(t, old, "x")
	want("只有旧的", root, old)
	want("只有旧的，目录末尾带斜杠", root+"/", old)
	away := t.TempDir()
	want("换了数据目录", away, filepath.Join(away, "telegram.env"))
	put(t, file, "x")
	want("新旧都有", root, file)
}

// 换了数据目录的真进程不读旧路径：不会往真的 Telegram 发。
func TestAwayStaysQuiet(t *testing.T) {
	home, away := t.TempDir(), t.TempDir()
	put(t, filepath.Join(home, ".agentq", "secrets", "telegram.env"), "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n")
	brief := func(env ...string) {
		t.Helper()
		cmd := exec.Command(bin, "brief") // 地址连不上：要发就会留在 outbox
		cmd.Env = append([]string{"HOME=" + home, "PATH=/usr/bin:/bin", "AGENTQ_TELEGRAM_API=http://127.0.0.1:1"}, env...)
		_ = cmd.Run()
	}
	brief("AGENTQ_HOME=" + away)
	if exists(filepath.Join(away, "outbox")) || !exists(filepath.Join(away, "brief.md")) {
		t.Error("读了旧的密钥")
	}
	brief()
	if left, _ := filepath.Glob(filepath.Join(home, ".agentq2", "outbox", "*.txt")); len(left) != 1 {
		t.Error("默认目录要接着用旧的密钥")
	}
}

// 派活技能：没有就装，我们装的换新，用户自己的一个字节不动。
func TestPutSkill(t *testing.T) {
	type at struct{ home, root, file, record string }
	fresh := func(t *testing.T) at {
		home, root := t.TempDir(), t.TempDir()
		file, record := skillFiles(home, root)
		return at{home, root, file, record}
	}
	install := func(t *testing.T, a at) string {
		t.Helper()
		told, err := putSkill(a.home, a.root)
		if err != nil || !strings.Contains(told, a.file) {
			t.Fatalf("要说清在哪：%q %v", told, err)
		}
		return told
	}
	kept := func(t *testing.T, a at, want string) {
		t.Helper()
		if told := install(t, a); !strings.Contains(told, "没动") || !strings.Contains(told, "挪开") {
			t.Errorf("要说没动、怎么办：%s", told)
		}
		if got := read(a.file); got != want {
			t.Errorf("动了用户的：%q", got)
		}
	}
	mine := string(dispatch)
	if !strings.HasPrefix(mine, "---\nname: dispatch\n") {
		t.Fatal("没编进派活技能")
	}

	t.Run("没有就装上", func(t *testing.T) {
		a := fresh(t)
		install(t, a)
		if read(a.file) != mine || read(a.record) != mine {
			t.Error("没装上")
		}
	})
	t.Run("目录在、里面没有SKILL.md", func(t *testing.T) {
		a := fresh(t)
		other := filepath.Join(filepath.Dir(a.file), "笔记.md")
		put(t, other, "用户的")
		install(t, a)
		if read(a.file) != mine || read(other) != "用户的" {
			t.Error("没装上")
		}
	})
	t.Run("用户放的恰好一样，装和卸都不动", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, mine)
		os.Chmod(a.file, 0o444) // 写它会报错
		if told := install(t, a); !strings.Contains(told, "已经有了") || exists(a.record) {
			t.Errorf("不是我们装的，不记：%s", told)
		}
		if told, err := dropSkill(a.home, a.root); err != nil || told != "" || read(a.file) != mine {
			t.Errorf("%q %v", told, err)
		}
	})
	t.Run("我们装的就是这一版，再装再卸", func(t *testing.T) {
		a := fresh(t)
		install(t, a)
		install(t, a)
		if told, err := dropSkill(a.home, a.root); err != nil || told == "" || exists(a.file) || exists(a.record) {
			t.Errorf("%q %v", told, err)
		}
	})
	t.Run("装过旧版，用户自己换成和新版一样的", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, mine)
		put(t, a.record, "旧版")
		install(t, a)
		if _, err := dropSkill(a.home, a.root); err != nil || read(a.file) != mine {
			t.Errorf("删了用户的：%v", err)
		}
	})
	t.Run("我们装的旧版换新", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, "旧版")
		put(t, a.record, "旧版")
		install(t, a)
		if read(a.file) != mine || read(a.record) != mine {
			t.Error("没换")
		}
	})
	t.Run("用户自己的", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, "用户自己的")
		kept(t, a, "用户自己的")
		if exists(a.record) {
			t.Error("不是我们装的，不记")
		}
	})
	t.Run("用户自己的空文件", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, "")
		kept(t, a, "")
	})
	t.Run("装了之后用户改过", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, "旧版，用户加了一句")
		put(t, a.record, "旧版")
		kept(t, a, "旧版，用户加了一句")
		if read(a.record) != "旧版" {
			t.Error("记录不该动")
		}
	})
	t.Run("目录是软链", func(t *testing.T) {
		for body, same := range map[string]bool{mine: true, "用户的": false, "旧版": false} {
			a, real := fresh(t), t.TempDir()
			put(t, filepath.Join(real, "SKILL.md"), body)
			os.MkdirAll(filepath.Dir(filepath.Dir(a.file)), 0o755)
			if err := os.Symlink(real, filepath.Dir(a.file)); err != nil {
				t.Fatal(err)
			}
			if body == "旧版" { // 就算内容和记录一样也不穿过软链写
				put(t, a.record, "旧版")
			}
			if told := install(t, a); same != strings.Contains(told, "已经有了") || same == strings.Contains(told, "没动") {
				t.Errorf("%s", told)
			}
			if read(filepath.Join(real, "SKILL.md")) != body || read(a.record) == mine {
				t.Error("穿过软链写了")
			}
		}
	})
	t.Run("目录是断了的软链", func(t *testing.T) {
		a := fresh(t)
		gone := filepath.Join(t.TempDir(), "没有")
		os.MkdirAll(filepath.Dir(filepath.Dir(a.file)), 0o755)
		os.Symlink(gone, filepath.Dir(a.file))
		kept(t, a, "")
		if exists(gone) {
			t.Error("穿过软链建了目录")
		}
	})
	t.Run("文件是软链", func(t *testing.T) {
		a, real := fresh(t), filepath.Join(t.TempDir(), "SKILL.md")
		put(t, real, "旧版")
		put(t, a.record, "旧版") // 就算内容和记录一样也不穿过软链写
		os.MkdirAll(filepath.Dir(a.file), 0o755)
		os.Symlink(real, a.file)
		kept(t, a, "旧版")
		if read(real) != "旧版" {
			t.Error("穿过软链写了")
		}
	})
	t.Run("那个位置是个文件", func(t *testing.T) {
		a := fresh(t)
		put(t, filepath.Dir(a.file), "用户的")
		kept(t, a, "")
		if read(filepath.Dir(a.file)) != "用户的" {
			t.Error("动了用户的")
		}
	})

	t.Run("卸掉", func(t *testing.T) {
		a := fresh(t)
		install(t, a)
		if told, err := dropSkill(a.home, a.root); err != nil || told == "" {
			t.Errorf("%q %v", told, err)
		}
		if exists(filepath.Dir(a.file)) || exists(a.record) || !exists(filepath.Join(a.home, ".claude", "skills")) {
			t.Error("只去掉 dispatch 和记录")
		}
	})
	t.Run("卸掉时目录里有别的", func(t *testing.T) {
		a := fresh(t)
		install(t, a)
		other := filepath.Join(filepath.Dir(a.file), "笔记.md")
		put(t, other, "用户的")
		if _, err := dropSkill(a.home, a.root); err != nil || exists(a.file) || read(other) != "用户的" {
			t.Errorf("别的要留着：%v", err)
		}
	})
	t.Run("卸的时候改过的留着", func(t *testing.T) {
		a := fresh(t)
		install(t, a)
		put(t, a.file, mine+"用户加的")
		if told, err := dropSkill(a.home, a.root); err != nil || !strings.Contains(told, "留着：") || read(a.file) != mine+"用户加的" {
			t.Errorf("%q %v", told, err)
		}
	})
	t.Run("卸的时候换成软链的留着", func(t *testing.T) {
		a, real := fresh(t), t.TempDir()
		install(t, a)
		moved := filepath.Join(real, "dispatch")
		os.Rename(filepath.Dir(a.file), moved)
		os.Symlink(moved, filepath.Dir(a.file))
		if told, err := dropSkill(a.home, a.root); err != nil || !strings.Contains(told, "留着：") || read(filepath.Join(moved, "SKILL.md")) != mine {
			t.Errorf("%q %v", told, err)
		}
	})
	t.Run("没装过不吭声", func(t *testing.T) {
		a := fresh(t)
		put(t, a.file, mine) // 内容一样，但不是我们装的
		if told, err := dropSkill(a.home, a.root); err != nil || told != "" || read(a.file) != mine {
			t.Errorf("%q %v", told, err)
		}
	})
}

// 整个装一遍、卸一遍。PATH 里只有假的 launchctl，碰不到真的。
func TestInstall(t *testing.T) {
	home, path := t.TempDir(), t.TempDir()
	put(t, filepath.Join(path, "launchctl"), "#!/bin/sh\necho \"$1 ${3##*/}\" >> '"+path+"/calls'\n")
	os.Chmod(filepath.Join(path, "launchctl"), 0o755)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + path}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return string(out)
	}
	says := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("缺「%s」：%s", w, out)
			}
		}
	}
	plist := func(name string) string {
		return filepath.Join(home, "Library", "LaunchAgents", "cc.agentq."+name+".plist")
	}
	file, _ := skillFiles(home, filepath.Join(home, ".agentq2"))
	secret := filepath.Join(home, ".agentq2", "telegram.env")

	says(run("install"), "写好 "+secret+" 再跑一次 agentq install", "派活技能在 "+file, noClaude)
	if exists(plist("listen")) || !exists(plist("resume")) || !exists(plist("brief")) || strings.Contains(read(filepath.Join(path, "calls")), "listen") {
		t.Errorf("没配 Telegram 不装 listen：%s", read(filepath.Join(path, "calls")))
	}
	put(t, secret, "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n")
	put(t, filepath.Join(path, "claude"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(path, "claude"), 0o755)
	out := run("install")
	says(out, "收发 Telegram 消息，密钥在 "+secret)
	if !exists(plist("listen")) || strings.Contains(out, noClaude) || read(file) != string(dispatch) {
		t.Errorf("%s", out)
	}
	says(run("uninstall"), "派活技能卸掉了", "正在做的会做完")
	if exists(plist("listen")) || exists(plist("resume")) || exists(plist("brief")) || exists(file) {
		t.Error("没卸干净")
	}
	if !exists(secret) {
		t.Error("用户的数据要留着")
	}
}

// 服务停不掉就不说卸掉了：服务文件、技能都留着，修好再卸一次就干净。
func TestUninstallFails(t *testing.T) {
	home, path := t.TempDir(), t.TempDir()
	ctl := filepath.Join(path, "launchctl")
	fake := func(bootout string) {
		put(t, ctl, "#!/bin/sh\n[ \"$1\" = bootout ] || exit 0\n"+bootout+"\n")
		os.Chmod(ctl, 0o755)
	}
	run := func(path string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + path}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	plist := func(name string) string {
		return filepath.Join(home, "Library", "LaunchAgents", "cc.agentq."+name+".plist")
	}
	file, _ := skillFiles(home, filepath.Join(home, ".agentq2"))
	fake("exit 0")
	if out, err := run(path, "install"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	kept := func(want string, out string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(out, want) || strings.Contains(out, "卸掉了") {
			t.Errorf("要报停不掉、带上「%s」：%v %s", want, err, out)
		}
		if !exists(plist("resume")) || !exists(plist("brief")) || !exists(file) {
			t.Error("服务还在跑，文件要留着")
		}
	}
	out, err := run(t.TempDir(), "uninstall") // 找不到 launchctl
	kept("launchctl", out, err)
	fake("echo 'Boot-out failed: 5: Input/output error' >&2; exit 5")
	out, err = run(path, "uninstall")
	kept("5: Input/output error", out, err)
	fake("[ \"${2##*.}\" = resume ] && exit 5; exit 0") // 一个停不掉，别的照停
	if out, err := run(path, "uninstall"); err == nil || exists(plist("brief")) || !exists(plist("resume")) {
		t.Errorf("%v %s", err, out)
	}
	for _, code := range []string{"3", "112", "113"} { // 本来就没在跑
		fake("exit 0")
		if out, err := run(path, "install"); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		fake("exit " + code)
		if out, err := run(path, "uninstall"); err != nil || exists(plist("resume")) || exists(file) {
			t.Errorf("%s：%v %s", code, err, out)
		}
	}
}

// 路径设置写成相对的：后台进程不在现在这个目录，也要找得到。
func TestRelativePaths(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base) // 程序看到的当前目录
	if err != nil {
		t.Fatal(err)
	}
	r.home = filepath.Join(real, "state")
	put(t, filepath.Join(real, "bin", "claude"), read(filepath.Join(r.fake, "claude")))
	os.Chmod(filepath.Join(real, "bin", "claude"), 0o755)
	put(t, filepath.Join(real, "ctl", "launchctl"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(real, "ctl", "launchctl"), 0o755)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = real
		cmd.Env = []string{"HOME=" + real, "PATH=" + filepath.Join(real, "ctl") + ":" + os.Getenv("PATH"), "AGENTQ_HOME=state", "AGENTQ_CLAUDE=bin/claude", "AGENTQ_CODEX=codex", "AGENTQ_PROJECTS=work", "AGENTQ_TELEGRAM=./tg"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return string(out)
	}
	run("do", "-C", r.dir, "做")
	for end := time.Now().Add(20 * time.Second); r.order() == nil || r.order().State != order.Done || r.order().Running(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("没做完：%s", run("ls"))
		}
	}
	run("install")
	got := read(filepath.Join(real, "Library", "LaunchAgents", "cc.agentq.resume.plist"))
	for k, v := range map[string]string{"HOME": "/state", "CLAUDE": "/bin/claude", "PROJECTS": "/work", "TELEGRAM": "/tg"} {
		if want := "<key>AGENTQ_" + k + "</key><string>" + real + v + "</string>"; !strings.Contains(got, want) {
			t.Errorf("缺 %s：%s", want, got)
		}
	}
	if !strings.Contains(got, "<key>AGENTQ_CODEX</key><string>codex</string>") { // 只写了名字的去 PATH 里找
		t.Errorf("%s", got)
	}
}

// 没找到 claude：开工前就说，不记需求。只看待办不用它。
func TestNoClaude(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	put(t, filepath.Join(r.dir, "docs", "BACKLOG.md"), "## 待做\n\n- [ ] 甲 —— 做甲\n")
	r.git(r.dir, "add", "-A")
	r.git(r.dir, "commit", "-qm", "待办")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = r.dir, []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "AGENTQ_HOME=" + r.home}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, args := range [][]string{{"do", "加个东西"}, {"todo", "all"}, {"todo", "甲"}} {
		if out, err := run(args...); err == nil || !strings.Contains(out, "没找到 claude") {
			t.Errorf("%v：%v %s", args, err, out)
		}
	}
	if n := len((&order.Store{Root: r.home}).All()); n != 0 {
		t.Errorf("记了 %d 条需求", n)
	}
	if out, err := run("todo"); err != nil || !strings.Contains(out, "甲") {
		t.Errorf("%v %s", err, out)
	}
}

// 版本号：两种写法都认，不建数据目录；发布时能换掉。
func TestVersion(t *testing.T) {
	home := t.TempDir()
	run := func(bin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"HOME=" + home}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return string(out)
	}
	if a, b := run(bin, "version"), run(bin, "--version"); a != "dev\n" || b != "dev\n" {
		t.Errorf("%q %q", a, b)
	}
	made := filepath.Join(t.TempDir(), "agentq")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9", "-o", made, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if got := run(made, "version"); got != "v9.9.9\n" {
		t.Errorf("%q", got)
	}
	help := run(bin, "--help")
	if !strings.HasPrefix(help, "agentq：说一句需求，做到合进主分支\n") { // install.sh 靠这一行认
		t.Errorf("%s", help)
	}
	for _, l := range []string{
		"  agentq install          装上：派活技能、自动接着做、早上 8 点早报，配了 Telegram 就收消息（uninstall 卸掉）\n",
		"  agentq version          版本号\n",
	} {
		if !strings.Contains(help, l) {
			t.Errorf("缺「%s」", l)
		}
	}
	if exists(filepath.Join(home, ".agentq2")) {
		t.Error("看版本、帮助不该建数据目录")
	}
}
