package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentq/internal/proc"
	"agentq/internal/state"
)

// JobProc 是 ps 列出的一个进程。
type JobProc struct {
	pid, pgid int
	command   string
}

// jobLookupTries、jobLookupGap：外壳死在 fork 和 exec 之间时，claude 过一小会儿才看得到。测试里改小。
var (
	JobLookupTries = 3
	JobLookupGap   = 150 * time.Millisecond
)

// ListProcs 列出本机进程（僵尸不算）。
var ListProcs = func() ([]JobProc, error) {
	out, err := proc.PsOutput("-A", "-ww", "-o", "pid=", "-o", "pgid=", "-o", "stat=", "-o", "command=")
	if err != nil {
		return nil, fmt.Errorf("列进程失败：%w", err)
	}
	return parseProcs(out), nil
}

func parseProcs(out string) []JobProc {
	var procs []JobProc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || strings.HasPrefix(f[2], "Z") {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, JobProc{pid, pgid, strings.Join(f[3:], " ")})
	}
	return procs
}

// jobProcs 找这个任务的进程，分开返回外壳和外壳以外的。认法三选一：
// 环境里有 AGENTQ_JOB=<任务 id>（claude 和它的子进程都继承）；命令行同时有进程标记和这个任务的会话 id
// （macOS 上系统自带的 sh 看不到环境，claude 是 sh 包装脚本时靠这个）；命令行是这个任务的 agentq _job。
func (a *Service) jobProcs(j *state.Job) (shells, others []JobProc, err error) {
	procs, err := ListProcs()
	if err != nil {
		return nil, nil, err
	}
	pids := make([]string, 0, len(procs))
	for _, p := range procs {
		pids = append(pids, strconv.Itoa(p.pid))
	}
	marked, err := proc.EnvMarkedPIDs(pids, proc.JobEnv+"="+j.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("查进程环境失败：%w", err)
	}
	shellArg := a.jobShellArg(j)
	for _, p := range procs {
		switch {
		case strings.Contains(p.command, shellArg):
			shells = append(shells, p)
		case slices.Contains(marked, strconv.Itoa(p.pid)),
			j.SessionID != "" && strings.Contains(p.command, j.SessionID) && strings.Contains(p.command, proc.NameMarker(j.Project)):
			others = append(others, p)
		}
	}
	return shells, others, nil
}

// pickClaude 从任务的进程里挑 CLI：自己一个进程组的组长，命令行带进程标记（每种任务启动时都带）。
// 只有环境标记、没有进程标记的是它拉起的子进程：收尾和 stop 都靠进程标记认活着，把它当 CLI 会被说成已结束。
// 命令行放不了标记的后端（noLabel）：组长、命令行里有 CLI 的名字。它拉起的命令也可能自己一个进程组，不能只看组长。
func pickClaude(j *state.Job, procs []JobProc, noLabel bool) int {
	cli := ""
	if len(j.Args) > 0 {
		cli = filepath.Base(j.Args[0])
	}
	for _, p := range procs {
		if p.pid != p.pgid {
			continue
		}
		if strings.Contains(p.command, proc.NameMarker(j.Project)) || (noLabel && cli != "" && strings.Contains(p.command, cli)) {
			return p.pid
		}
	}
	return 0
}

// jobShellArg 是这个任务的外壳命令行里的一段。
func (a *Service) jobShellArg(j *state.Job) string {
	return "_job " + filepath.Dir(a.Ledger.JobOutput(j.ID, ClaudePidFile)) + " "
}

// JobLive 认这个任务的 CLI 还在不在。命令行带标记的后端和原来一样按标记认（认不出就是结束了或 pid 被复用）；放不了标记的按环境认。
func (a *Service) JobLive(j *state.Job) proc.Liveness {
	if a.BackendFor(j.Backend).Caps().NoArgvLabel {
		return proc.JobLiveness(j.PID, j.ID, a.jobShellArg(j))
	}
	if proc.ProcAlive(j.PID, proc.NameMarker(j.Project)) {
		return proc.ProcOurs
	}
	return proc.ProcGone
}

// KillLostJob：状态文件和 meta.json 都读不出，这个任务的记录没了，进程可能还活着。留着它没人收尾、超时不管，名额不算，
// 条目还会被再接一次，两个进程做同一条。杀掉，条目走重新接活或孤儿接手（工作在分支和 worktree 里）。返回杀了几个进程；err 非空表示查不了，不能下结论。
func (a *Service) KillLostJob(id string) (int, error) {
	shells, others, err := a.jobProcs(&state.Job{ID: id})
	if err != nil {
		return 0, err
	}
	// 认不准就不杀。任务 id 不含 HOME，别的安装可能有同名的任务：只认这个 HOME 下的
	pids := make([]string, 0, len(others))
	for _, p := range others {
		pids = append(pids, strconv.Itoa(p.pid))
	}
	mine, err := proc.EnvMarkedPIDs(pids, "HOME="+a.Home)
	if err != nil {
		return 0, fmt.Errorf("查进程环境失败：%w", err)
	}
	others = slices.DeleteFunc(others, func(p JobProc) bool { return !slices.Contains(mine, strconv.Itoa(p.pid)) })
	haveShell := len(shells) > 0
	for _, s := range shells { // 外壳先走，不然它等到 CLI 退出还要起 tick --after
		_ = syscall.Kill(s.pid, syscall.SIGKILL)
	}
	pgids := map[int]bool{}
	for _, p := range others {
		pgids[p.pgid] = true
	}
	n := len(shells) + len(others)
	// macOS 上 sh 包装脚本看不到环境，会话 id 又随 meta.json 丢了：靠外壳写下的 claude.pid。外壳还活着才信它：外壳等着这个子进程，pid 不会被复用；
	// 外壳没了这个文件可能是很久以前的，pid 已经是别人的
	if b, err := os.ReadFile(a.Ledger.JobOutput(id, ClaudePidFile)); haveShell && err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && !pgids[pid] && proc.ProcAlive(pid, proc.NamePrefix) {
			pgids[pid] = true
			n++
		}
	}
	for pgid := range pgids {
		proc.KillGroup(pgid, proc.LeftoverGrace)
	}
	return n, nil
}

// FindLostClaude 找外壳没报回 pid 的任务的 claude。返回 0 表示确认这个任务已经没有进程了；err 非空表示查不了，不能下结论。
// 还活着的外壳（没报 pid，也过了时限）先杀掉，不然它之后还可能起一个 claude。没有 claude、只剩子进程的，清掉它们：记失败后没人再管。
func (a *Service) FindLostClaude(j *state.Job) (int, error) {
	for try := 0; ; try++ {
		shells, others, err := a.jobProcs(j)
		if err != nil {
			return 0, err
		}
		for _, s := range shells {
			a.Logf("任务 %s 的外壳（pid %d）过了时限还没报 pid，杀掉", j.ID, s.pid)
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
		if pid := pickClaude(j, others, a.BackendFor(j.Backend).Caps().NoArgvLabel); pid > 0 {
			return pid, nil
		}
		if len(others) > 0 {
			pgids := map[int]bool{}
			for _, p := range others {
				pgids[p.pgid] = true
			}
			for pgid := range pgids {
				a.Logf("任务 %s 没有 %s，但进程组 %d 还有它的子进程，清掉", j.ID, a.BackendFor(j.Backend).Name(), pgid)
				proc.KillGroup(pgid, proc.LeftoverGrace)
			}
			return 0, nil
		}
		if try+1 >= JobLookupTries {
			return 0, nil
		}
		time.Sleep(JobLookupGap)
	}
}
