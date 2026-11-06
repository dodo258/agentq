package notify

import (
	"fmt"
	"os/exec"

	"agentq/internal/textfmt"
)

const LocalNotifyNote = "（没配 Telegram，只弹了本机通知；要推到手机执行 agentq notify setup）"

// localNotify 弹一条 macOS 本机通知。
func localNotify(a *Service, title, body string) error {
	// 用参数传标题和正文，不拼进脚本，免得引号、反斜杠出问题
	out, err := exec.Command("/usr/bin/osascript", "-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", title, body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript：%v %s", err, textfmt.OneLine(string(out), 200))
	}
	return nil
}
