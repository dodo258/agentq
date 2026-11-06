package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

func (a *App) CmdNotify(args []string) error {
	if len(args) == 0 {
		return errors.New("缺子命令\n" + notify.NotifyUsage)
	}
	switch args[0] {
	case "setup":
		return a.NotifySetup()
	case "test":
		return a.notifyTest()
	case "send":
		return a.notifySend(args[1:])
	case "file":
		return a.Notify.NotifyFile(args[1:])
	case "status":
		return a.notifyStatusCmd()
	}
	return fmt.Errorf("不认识的子命令 %q\n%s", args[0], notify.NotifyUsage)
}

func (a *App) NotifySetup() error {
	script := filepath.Join(a.DataDir(), "secrets", "telegram-setup.sh")
	if _, err := os.Stat(script); err == nil {
		cmd := exec.Command("/bin/bash", script)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Out, a.Out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s 没跑成功：%w", script, err)
		}
		fmt.Fprintln(a.Out, "配好后执行 agentq notify test 收一条测试消息。")
		return a.Notify.EnableNotifyIfConfigured()
	}
	if c, err := a.Telegram.TelegramCreds(); c != nil || err != nil {
		return a.Notify.EnableNotifyIfConfigured()
	}
	fmt.Fprintf(a.Out, `没有 %s，按下面手动配：
1. 在 Telegram 里找 @BotFather，发 /newbot 建一个机器人，记下它给的 token。
2. 给你的机器人发一句话，然后在浏览器打开 https://api.telegram.org/bot<token>/getUpdates，找到 "chat":{"id":…} 里的数字。
3. 写 %s（两行），再 chmod 600 它：
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_CHAT_ID=<数字>
4. 执行 agentq notify test，手机上收到测试消息就好了。之后 agentq 只给这个 chat id 发消息，也只认它发来的消息。
`, script, a.Telegram.TelegramEnvPath())
	return a.Notify.EnableNotifyIfConfigured()
}

func (a *App) notifyTest() error {
	c, credErr := a.Telegram.TelegramCreds()
	now := a.Now()
	msg := a.Notify.RenderMessage("status", now, []notify.MsgSection{
		{Emoji: "🟢", Head: "推送通了", Lines: []string{"以后要你定的事、巡逻发现的问题、每天的早报会推到这里", "回一句「状态」试试双向"}},
	}, "agentq notify test")
	via, _, err := a.Notify.Deliver(c, credErr, msg)
	if err != nil {
		return fmt.Errorf("发送失败（%s）：%w", via, err)
	}
	fmt.Fprintf(a.Out, "测试消息已发出（%s）。\n", via)
	if c == nil {
		fmt.Fprintln(a.Out, notify.LocalNotifyNote)
	}
	return nil
}

func (a *App) notifySend(args []string) error {
	var n notify.Notice
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case args[0] == "--security":
			n.Security = true
		case strings.HasPrefix(args[0], "--key="):
			n.Key = strings.TrimPrefix(args[0], "--key=")
		case strings.HasPrefix(args[0], "--file="):
			p := strings.TrimPrefix(args[0], "--file=")
			path, err := filepath.Abs(fsx.ExpandHome(a.Home, p))
			if err != nil || p == "" {
				return fmt.Errorf("--file 的路径不对：%q", p)
			}
			// 现在就检查、压缩（在调用方的进程里做，不让 tick 拿着锁去压录屏）；发不了的改成一行带路径的说明
			send, note, err := a.Telegram.PrepareMedia(path)
			if err != nil {
				return err
			}
			if note != "" {
				n.Lines = append(n.Lines, note)
			} else {
				n.Files = append(n.Files, send)
			}
		default:
			return fmt.Errorf("不认识的选项 %s\n%s", args[0], notify.NotifyUsage)
		}
		args = args[1:]
	}
	if len(args) < 2 || notify.CatRank(args[0]) == len(notify.NotifyOrder) || strings.TrimSpace(args[1]) == "" {
		return errors.New("要写类别和一句话\n" + notify.NotifyUsage)
	}
	n.Cat, n.Text, n.At = args[0], strings.TrimSpace(args[1]), a.Now()
	notes := n.Lines
	n.Lines = nil
	for _, l := range args[2:] {
		if l = strings.TrimSpace(l); l != "" {
			n.Lines = append(n.Lines, l)
		}
	}
	n.Lines = append(n.Lines, notes...)
	if !a.Cfg.NotifyEnabled {
		fmt.Fprintln(a.Out, "推送关着（notify_enabled=false），没记")
		return nil
	}
	st, _ := state.Load(a.StatePath()) // 读不出就不顺带推早报
	var pending int
	err := a.Notify.WithNotify(func(s *notify.State) error {
		s.Add(a.Now(), n)
		c, credErr := a.Telegram.TelegramCreds()
		a.Notify.FlushNotify(s, st, c, credErr)
		pending = len(s.Pending)
		for _, p := range s.Pending {
			if notify.SameNotice(p, n) {
				fmt.Fprintln(a.Out, "已记下，按规则稍后推（同类 30 分钟内合并、夜里攒到 08:00、每天最多 10 条）")
				return nil
			}
		}
		fmt.Fprintln(a.Out, "已推出")
		return nil
	})
	a.Notify.SendQueuedFiles()
	if err == nil && pending > 0 {
		fmt.Fprintf(a.Out, "（队列里还攒着 %d 条）\n", pending)
	}
	return err
}

func (a *App) notifyStatusCmd() error {
	fmt.Fprintf(a.Out, "推送：%s\n", a.Notify.NotifyStatus())
	s, err := a.Notify.LoadNotify()
	if err != nil {
		return err
	}
	for _, n := range s.Pending {
		sec := ""
		if n.Security {
			sec = "（安全）"
		}
		fmt.Fprintf(a.Out, "  攒着：%s %s「%s」%s\n", textfmt.WhenCN(n.At), notify.NotifyCats[n.Cat].Name, n.Text, sec)
	}
	if len(s.Inbox) > 0 {
		fmt.Fprintf(a.Out, "  你发来、排队等回答的消息 %d 条\n", len(s.Inbox))
	}
	if s.LastError != "" {
		fmt.Fprintf(a.Out, "  最近的错误（%s）：%s\n", textfmt.WhenCN(s.LastErrorAt), s.LastError)
	}
	return nil
}
