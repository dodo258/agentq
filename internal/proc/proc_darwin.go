package proc

import (
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// PsLocale：launchd 下没有 LANG，ps 会把中文转义。
const PsLocale = "en_US.UTF-8"

// BootTime 返回本次开机的时间（秒），取自 sysctl -n kern.boottime：「{ sec = 1791363855, usec = 257843 } Wed Oct  7 …」。
func BootTime() (int64, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return 0, err
	}
	return parseBootTime(string(out))
}

// procsEnvMarked：pids 里有没有进程带着这个环境变量（ps -E 显示的环境）。
// 系统自带的程序（/bin/zsh、/bin/sleep 等）ps 看不到环境变量，组里只剩它们时认不出，宁可不杀。
func procsEnvMarked(pids []string, mark string) (bool, error) {
	out, err := PsOutput("-E", "-ww", "-o", "command=", "-p", strings.Join(pids, ","))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // 列完之后都退出了
		}
		return false, err
	}
	envMark := mark + " "
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line+" ", " "+envMark) {
			return true, nil
		}
	}
	return false, nil
}

// procEnvHas 看一个进程的环境有没有 mark。ps -E 和不带 -E 的输出一样，就是读不到环境（系统自带的程序）。
// 实测（macOS 27、codex-cli 0.166.0）：Homebrew 的 node、桌面端带的硬化签名 node、Codex 的原生程序都读得到，/bin/sleep 读不到。
func procEnvHas(pid int, mark string) (found, readable bool) {
	withEnv, err := PsOutput("-E", "-ww", "-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false, false
	}
	plain, err := PsOutput("-ww", "-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false, false
	}
	return strings.Contains(strings.TrimRight(withEnv, "\n")+" ", " "+mark+" "), strings.TrimSpace(withEnv) != strings.TrimSpace(plain)
}

// EnvMarkedPIDs：pids 里哪些进程的环境带着这个变量（ps -E）。看不到环境的系统程序认不出，也看不到别的用户的。
func EnvMarkedPIDs(pids []string, mark string) ([]string, error) {
	out, err := PsOutput("-E", "-ww", "-A", "-o", "pid=", "-o", "command=")
	if err != nil {
		return nil, err
	}
	var marked []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && slices.Contains(pids, f[0]) && strings.Contains(line+" ", " "+mark+" ") {
			marked = append(marked, f[0])
		}
	}
	return marked, nil
}
