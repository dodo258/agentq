package proc

import (
	"os"
	"strconv"
	"strings"
)

// PsLocale：系统里不一定装了 en_US.UTF-8，C.UTF-8 各发行版都有。C 区域下 ps 会把中文项目名转义，procAlive 就认不出任务了。
const PsLocale = "C.UTF-8"

// BootTime 返回本次开机的时间（秒），取自 /proc/stat 的 btime。和 macOS 的 kern.boottime 同一种含义，
// Job.BootTime 的老记录和 bootSlack 的判断都不用变。
func BootTime() (int64, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	return ParseProcStatBtime(string(b))
}

// procsEnvMarked：pids 里有没有进程带着这个环境变量。Linux 从 /proc/<pid>/environ 读，系统自带的程序也读得到。
func procsEnvMarked(pids []string, mark string) (bool, error) {
	return ProcEnvMarked("/proc", pids, mark), nil
}

// procEnvHas 看一个进程的环境有没有 mark。/proc/<pid>/environ 读不了（别的用户的）就是读不到。
func procEnvHas(pid int, mark string) (found, readable bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return false, false
	}
	for _, kv := range strings.Split(string(b), "\x00") {
		if kv == mark {
			return true, true
		}
	}
	return false, true
}

// EnvMarkedPIDs：pids 里哪些进程的环境带着这个变量。从 /proc 读，读不了的（别的用户的、已经退出的）不算。
func EnvMarkedPIDs(pids []string, mark string) ([]string, error) {
	var marked []string
	for _, pid := range pids {
		if ProcEnvMarked("/proc", []string{pid}, mark) {
			marked = append(marked, pid)
		}
	}
	return marked, nil
}
