package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"agentq/internal/claude"
	"agentq/internal/order"
)

// jobs 是交给 launchd 的三件事：被打断的接着做、收消息、早上 8 点发早报。
var jobs = map[string]string{
	"resume": "<key>RunAtLoad</key><true/><key>StartInterval</key><integer>300</integer><key>AbandonProcessGroup</key><true/>",
	"listen": "<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>30</integer><key>AbandonProcessGroup</key><true/>",
	"brief":  "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>8</integer><key>Minute</key><integer>0</integer></dict>",
}

//go:embed skills/dispatch/SKILL.md
var dispatch []byte

func install(s *order.Store, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := unload(); err != nil {
		return err
	}
	vars := "<key>LANG</key><string>en_US.UTF-8</string>"
	for _, kv := range claude.Env() {
		if k, v, _ := strings.Cut(kv, "="); k == "PATH" || strings.HasPrefix(k, "AGENTQ_") {
			vars += "<key>" + k + "</key><string>" + html.EscapeString(v) + "</string>"
		}
	}
	tg := teller(s)
	for name, when := range jobs {
		if name == "listen" && !tg.Ready() { // 没配就收不了，装上只会一直报错重启
			continue
		}
		label, p, dom := job(name)
		log := filepath.Join(s.Root, name+".log")
		body := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict>` +
			"<key>Label</key><string>" + label + "</string><key>ProgramArguments</key><array><string>" + self + "</string><string>" + name + "</string></array>" + when +
			"<key>StandardOutPath</key><string>" + log + "</string><key>StandardErrorPath</key><string>" + log + "</string>" +
			"<key>EnvironmentVariables</key><dict>" + vars + "</dict></dict></plist>\n"
		if err := errors.Join(os.MkdirAll(filepath.Dir(p), 0o755), os.WriteFile(p, []byte(body), 0o644)); err != nil {
			return err
		}
		if b, err := exec.Command("launchctl", "bootstrap", dom, p).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl 装不上 %s：%w %s", name, err, strings.TrimSpace(string(b)))
		}
	}
	fmt.Fprintln(out, "装好了：\n· 被打断的 5 分钟内自动接着做\n· 早上 8 点早报，写在 "+filepath.Join(s.Root, "brief.md"))
	mail := "没配 Telegram，不收发消息：写好 " + tg.File + " 再跑一次 agentq install"
	if tg.Ready() {
		mail = "收发 Telegram 消息，密钥在 " + tg.File
	}
	fmt.Fprintln(out, "· "+mail)
	home, _ := os.UserHomeDir()
	told, err := putSkill(home, s.Root)
	if err != nil {
		return fmt.Errorf("派活技能没装上：%w", err)
	}
	fmt.Fprintln(out, "· "+told)
	if runner().Path == "" {
		fmt.Fprintln(out, "· "+noClaude+"，再跑一次 agentq install")
	}
	return nil
}

func job(name string) (label, plist, domain string) {
	home, _ := os.UserHomeDir()
	label = "cc.agentq." + name
	return label, filepath.Join(home, "Library", "LaunchAgents", label+".plist"), fmt.Sprintf("gui/%d", os.Getuid())
}

// unload 停掉服务、删服务文件。停不掉的文件留着，报出来。
func unload() error {
	var all error
	for name := range jobs {
		label, p, dom := job(name)
		b, err := exec.Command("launchctl", "bootout", dom+"/"+label).CombinedOutput()
		var exit *exec.ExitError
		gone := errors.As(err, &exit) && slices.Contains([]int{3, 112, 113}, exit.ExitCode()) // 本来就没在跑：没有这个服务、没有登录的桌面
		if err != nil && !gone {
			err = fmt.Errorf("launchctl 停不掉 %s：%w %s", name, err, strings.TrimSpace(string(b)))
		} else if err = os.Remove(p); os.IsNotExist(err) {
			err = nil
		}
		all = errors.Join(all, err)
	}
	return all
}

func uninstall(root string, out io.Writer) error {
	if err := unload(); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	told, err := dropSkill(home, root)
	if err != nil {
		return err
	}
	if told != "" {
		fmt.Fprintln(out, told)
	}
	fmt.Fprintln(out, "卸掉了，正在做的会做完。程序和 "+root+" 留着")
	return nil
}

// skillFiles：派活技能放在哪，我们上次装的是什么记在哪。
func skillFiles(home, root string) (file, record string) {
	return filepath.Join(home, ".claude", "skills", "dispatch", "SKILL.md"), filepath.Join(root, "skill-dispatch.md")
}

// lookSkill 看现在的派活技能。none 是没有；ours 是我们装的、没改过，是普通文件，不隔着软链。
func lookSkill(file, record string) (now []byte, none, ours bool) {
	dir, derr := os.Lstat(filepath.Dir(file))
	f, ferr := os.Lstat(file)
	now, _ = os.ReadFile(file) // 读不出来的算用户自己的
	was, _ := os.ReadFile(record)
	none = os.IsNotExist(derr) || derr == nil && dir.IsDir() && os.IsNotExist(ferr)
	return now, none, derr == nil && ferr == nil && dir.IsDir() && f.Mode().IsRegular() && len(was) > 0 && bytes.Equal(now, was)
}

// putSkill 装派活技能。用户自己的、改过的、软链过去的都不碰。返回给用户看的一句话。
func putSkill(home, root string) (string, error) {
	file, record := skillFiles(home, root)
	now, none, ours := lookSkill(file, record)
	same := bytes.Equal(now, dispatch)
	switch {
	case same && !ours: // 内容一样也是用户放的：只有我们写的才记，记了会卸
		return "派活技能已经有了：" + file, nil
	case !none && !ours:
		return "派活技能没装：" + file + " 是你自己的或改过的，没动。要用 agentq 带的就先挪开，再跑一次 agentq install", nil
	case !same:
		if err := errors.Join(os.MkdirAll(filepath.Dir(file), 0o755), os.WriteFile(file, dispatch, 0o644)); err != nil {
			return "", err
		}
	}
	return "派活技能在 " + file, os.WriteFile(record, dispatch, 0o600)
}

// dropSkill 去掉我们装的派活技能，改过的留着。没装过不吭声。
func dropSkill(home, root string) (string, error) {
	file, record := skillFiles(home, root)
	if _, err := os.Stat(record); os.IsNotExist(err) {
		return "", nil
	}
	switch _, none, ours := lookSkill(file, record); {
	case ours:
		if err := os.Remove(file); err != nil {
			return "", err
		}
		_ = os.Remove(filepath.Dir(file)) // 里面还有别的就留着
	case !none:
		return "派活技能改过，留着：" + file, os.Remove(record)
	}
	return "派活技能卸掉了", os.Remove(record)
}
