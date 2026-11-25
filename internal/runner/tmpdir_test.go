package runner_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

func tmpStack(t *testing.T, todo string) (*env.Env, *stack.Stack, *bytes.Buffer, string) {
	t.Helper()
	home, out := t.TempDir(), &bytes.Buffer{}
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".agentq"), 0o700)
	os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600)
	e, err := env.New(home, out)
	if err != nil {
		t.Fatal(err)
	}
	e.Cfg.ProjectsRoot = t.TempDir()
	dir := filepath.Join(e.Cfg.ProjectsRoot, "demo")
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n"+todo+"\n## 已完成\n"), 0o644)
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	return e, stack.New(e), out, dir
}

// 什么时候清：任务了结了才清。没收尾、在过门、等用户定、可续跑的都留着。
func TestSweepTmpByState(t *testing.T) {
	const item = "活 —— 要什么"
	plain, doing, stuck := "- [ ] "+item+"\n", "- [ ] "+item+"（进行中：agent/x 2026-01-01 10:00）\n", "- [ ] "+item+"（卡住：等用户：付费，分支 agent/x）\n"
	gated := func(s string) func(*state.Job) {
		return func(j *state.Job) { j.Gate = &state.GateRecord{State: s, Branch: "agent/x"} }
	}
	for _, c := range []struct {
		name, status, todo string
		edit               func(*state.Job)
		keep               bool
	}{
		{"正常结束", state.StatusDone, "", nil, false},
		{"失败", state.StatusFailed, doing, nil, false},
		{"超时", state.StatusTimeout, doing, nil, false},
		{"被停", state.StatusKilled, doing, nil, false},
		{"中断、条目已不在进行中", state.StatusInterrupted, plain, func(j *state.Job) { j.SessionID = "s" }, false},
		{"中断、没有会话", state.StatusInterrupted, doing, nil, false},
		{"夜班", state.StatusDone, "", func(j *state.Job) { j.Kind = "night" }, false},
		{"合并了", state.StatusDone, "", gated(state.GateMerged), false},
		{"放弃了", state.StatusDone, doing, gated(state.GateAbandoned), false},
		{"已经有任务接着续跑", state.StatusInterrupted, doing, func(j *state.Job) { j.SessionID, j.ResumedBy = "s", "x" }, false},

		{"在跑", state.StatusRunning, doing, nil, true},
		{"等续跑", state.StatusInterrupted, doing, func(j *state.Job) { j.SessionID, j.ResumePending = "s", true }, true},
		{"等续跑的新项目（没有条目）", state.StatusInterrupted, "", func(j *state.Job) { j.Kind, j.SessionID, j.ResumePending = state.KindNew, "s", true }, true},
		{"可续跑：中断、没人接、条目还在进行中", state.StatusInterrupted, doing, func(j *state.Job) { j.SessionID = "s" }, true},
		{"等用户定：条目标着卡住", state.StatusDone, stuck, nil, true},
		{"等用户定：卡住标记还没写上", state.StatusDone, doing, func(j *state.Job) { j.StuckPending = "等你" }, true},
		{"等用户定：卡在门上", state.StatusDone, doing, gated(state.GateStuck), true},
		{"排队过门", state.StatusDone, doing, gated(state.GateQueued), true},
		{"在过门", state.StatusDone, doing, gated(state.GateRunning), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, k, out, dir := tmpStack(t, c.todo)
			j := &state.Job{ID: "1111-100000-demo", Project: "demo", ProjectDir: dir, DefaultBranch: "main", Item: item, Title: "活",
				Fingerprint: backlog.Fingerprint("- [ ] " + item), Status: c.status, StartedAt: e.Now()}
			if c.edit != nil {
				c.edit(j)
			}
			tmp, err := e.Tmps().New(j.ID, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(tmp.Path, "f"), []byte("x"), 0o644)
			st := &state.State{Jobs: []*state.Job{j}}
			k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
			_, err = os.Lstat(tmp.Path)
			if kept := err == nil; kept != c.keep {
				t.Fatalf("留着=%v，应该是 %v\n%s", kept, c.keep, out)
			}
			if list, _ := e.Tmps().List(); (len(list) == 1) != c.keep {
				t.Errorf("登记要和目录一起：%v", list)
			}
			if c.keep && out.Len() > 0 {
				t.Errorf("正常留着的不用说：%s", out)
			}
			if !c.keep && !strings.Contains(out.String(), "任务 "+j.ID+" 用完了，清了 "+tmp.Path) {
				t.Errorf("清了要记日志：%s", out)
			}
		})
	}
}

// 又有任务接了这一条（接手、去掉卡住标记重做）：它从分支接着做，前一个的目录不用留。
func TestSweepTmpTakenOver(t *testing.T) {
	const item = "活 —— 要什么"
	e, k, out, dir := tmpStack(t, "- [ ] "+item+"（进行中：agent/x 2026-01-01 10:00）\n")
	old := &state.Job{ID: "1111-100000-demo", Project: "demo", ProjectDir: dir, DefaultBranch: "main", Item: item, Title: "活",
		Fingerprint: backlog.Fingerprint("- [ ] " + item), Status: state.StatusInterrupted, SessionID: "s", StartedAt: e.Now().Add(-time.Hour)}
	now := *old
	now.ID, now.Status, now.StartedAt, now.Takeover = "1111-110000-demo", state.StatusRunning, e.Now(), true
	a, err := e.Tmps().New(old.ID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Tmps().New(now.ID, now.ID)
	if err != nil {
		t.Fatal(err)
	}
	k.Runner.RemoveTmp(k.Runner.SweepTmp(&state.State{Jobs: []*state.Job{old, &now}}))
	if _, err := os.Lstat(a.Path); !os.IsNotExist(err) {
		t.Errorf("前一个的要清：%v\n%s", err, out)
	}
	if _, err := os.Lstat(b.Path); err != nil {
		t.Errorf("在跑的不能动：%v", err)
	}
}

// 说不准的不清：任务的 meta.json 读不懂、BACKLOG 读不了。任务记录过了保留期的可以清；状态丢了、meta.json 还写着在跑的不清。
func TestSweepTmpUnknown(t *testing.T) {
	e, k, out, dir := tmpStack(t, "")
	mk := func(id, meta string) proc.Tmp {
		t.Helper()
		if meta != "" {
			os.MkdirAll(e.JobDir(id), 0o755)
			os.WriteFile(filepath.Join(e.JobDir(id), "meta.json"), []byte(meta), 0o644)
		}
		tmp, err := e.Tmps().New(id, id)
		if err != nil {
			t.Fatal(err)
		}
		return tmp
	}
	gone := mk("1111-000001-demo", "")
	broken := mk("1111-000002-demo", "{")
	running := mk("1111-000003-demo", `{"id":"1111-000003-demo","status":"running"}`)
	ended := mk("1111-000004-demo", `{"id":"1111-000004-demo","status":"done"}`)
	unread := mk("1111-000005-demo", "")
	lost := filepath.Join(e.Cfg.ProjectsRoot, "没了")
	os.MkdirAll(filepath.Join(lost, "docs", "BACKLOG.md"), 0o755) // 是目录，读不了
	st := &state.State{Jobs: []*state.Job{{ID: "1111-000005-demo", Project: "没了", ProjectDir: lost, Status: state.StatusDone, Title: "x"}}}
	for range 2 {
		k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
	}
	for tmp, keep := range map[*proc.Tmp]bool{&gone: false, &broken: true, &running: true, &ended: false, &unread: true} {
		if _, err := os.Lstat(tmp.Path); (err == nil) != keep {
			t.Errorf("%s 留着应该是 %v：%v", tmp.Job, keep, err)
		}
	}
	for _, id := range []string{"1111-000002-demo", "1111-000005-demo"} {
		if n := strings.Count(out.String(), "任务 "+id+" 的 /"); n != 1 {
			t.Errorf("%s 没清的原因只说一次，说了 %d 次：%s", id, n, out)
		}
	}

	// 登记读不懂：指的目录不动，说一次
	out.Reset()
	victim := filepath.Join(dir, "docs")
	bad := filepath.Join(e.Tmps().Dir, "agqbad.json")
	os.WriteFile(bad, []byte(`{"path":"`+victim+`","inode":1,"marks":["x"]}`), 0o600)
	for range 2 {
		k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
	}
	if _, err := os.Lstat(filepath.Join(victim, "BACKLOG.md")); err != nil {
		t.Errorf("登记读不懂的不能动：%v", err)
	}
	if strings.Count(out.String(), "读不懂") != 1 {
		t.Errorf("%s", out)
	}
}

// 还有进程在用的不清，进程走了再清：进程组里有活进程、带着标记跑到别的组里、工作目录在里面的系统自带程序。
func TestSweepTmpBusy(t *testing.T) {
	const id = "1111-100000-demo"
	for _, c := range []struct {
		name  string
		start func(tmp proc.Tmp) *exec.Cmd
		group bool
	}{
		{"进程组里还有活进程", func(tmp proc.Tmp) *exec.Cmd {
			cmd := exec.Command("/bin/sleep", "60") // 看不到环境，认不出是不是它的：当还在用
			cmd.Env = []string{"PATH=/usr/bin:/bin", proc.JobEnv + "=" + id}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			return cmd
		}, true},
		{"带着任务标记、在别的进程组", func(tmp proc.Tmp) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run=^TestTmpHelperSleep$")
			cmd.Env = []string{"PATH=/usr/bin:/bin", proc.JobEnv + "=" + id, "AGENTQ_TMP_HELPER=1"}
			return cmd
		}, false},
		{"工作目录在里面", func(tmp proc.Tmp) *exec.Cmd {
			cmd := exec.Command("/bin/sleep", "60")
			cmd.Env, cmd.Dir = []string{"PATH=/usr/bin:/bin"}, tmp.Path
			return cmd
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, k, out, dir := tmpStack(t, "")
			tmp, err := e.Tmps().New(id, id)
			if err != nil {
				t.Fatal(err)
			}
			j := &state.Job{ID: id, Project: "demo", ProjectDir: dir, Title: "活", Status: state.StatusDone, StartedAt: e.Now()}
			st := &state.State{Jobs: []*state.Job{j}}
			cmd := c.start(tmp)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			kill := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
			defer kill()
			time.Sleep(300 * time.Millisecond) // 等它 exec 完，ps、lsof 看得到
			if c.group {
				j.PID = cmd.Process.Pid
			}
			for range 2 {
				k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
			}
			if _, err := os.Lstat(tmp.Path); err != nil {
				t.Fatalf("还有进程在用，不能清：%v\n%s", err, out)
			}
			if strings.Count(out.String(), "先留着") != 1 {
				t.Errorf("原因说一次：%s", out)
			}
			kill()
			k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
			if _, err := os.Lstat(tmp.Path); !os.IsNotExist(err) {
				t.Errorf("进程走了要清：%v\n%s", err, out)
			}
		})
	}
}

func TestTmpHelperSleep(t *testing.T) {
	if os.Getenv("AGENTQ_TMP_HELPER") != "1" {
		t.Skip("只给 TestSweepTmpBusy 当子进程")
	}
	time.Sleep(60 * time.Second)
}

// 任务启动后机器重启过：记下的组号可能已经是别的程序的，不看它；没有进程在用就清。
func TestSweepTmpRebooted(t *testing.T) {
	e, k, out, dir := tmpStack(t, "")
	const id = "1111-100000-demo"
	tmp, err := e.Tmps().New(id, id)
	if err != nil {
		t.Fatal(err)
	}
	st := &state.State{Jobs: []*state.Job{{ID: id, Project: "demo", ProjectDir: dir, Title: "活", Status: state.StatusDone, PID: syscall.Getpgrp(), BootTime: e.CurBoot() - 3600}}}
	k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
	if _, err := os.Lstat(tmp.Path); !os.IsNotExist(err) {
		t.Errorf("%v\n%s", err, out)
	}
}

// 重启后用同一个会话续跑，接着用重启前建的目录：续跑是这次开机起的，它的进程还在用就不能清。进程组、工作目录两种都认。
func TestSweepTmpResumedAfterReboot(t *testing.T) {
	const oldID, newID = "1111-100000-demo", "1111-110000-demo"
	for _, group := range []bool{true, false} {
		t.Run(map[bool]string{true: "进程组里还有活进程", false: "工作目录在里面"}[group], func(t *testing.T) {
			e, k, out, dir := tmpStack(t, "")
			tmp, err := e.Tmps().New(oldID, oldID)
			if err != nil {
				t.Fatal(err)
			}
			tmp.Job, tmp.Marks = newID, append(tmp.Marks, newID) // 续跑沿用
			if err := e.Tmps().Save(tmp); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sleep", "60")
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			now := &state.Job{ID: newID, Project: "demo", ProjectDir: dir, Title: "活", Status: state.StatusDone, ResumeOf: oldID, BootTime: e.CurBoot()}
			if group {
				cmd.Env = append(cmd.Env, proc.JobEnv+"="+newID)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			} else {
				cmd.Dir = tmp.Path
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			kill := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
			defer kill()
			time.Sleep(300 * time.Millisecond)
			if group {
				now.PID = cmd.Process.Pid
			}
			st := &state.State{Jobs: []*state.Job{
				{ID: oldID, Project: "demo", ProjectDir: dir, Title: "活", Status: state.StatusInterrupted, ResumedBy: newID, PID: syscall.Getpgrp(), BootTime: e.CurBoot() - 3600},
				now,
			}}
			k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
			if _, err := os.Lstat(tmp.Path); err != nil {
				t.Fatalf("续跑的进程还在用，不能清：%v\n%s", err, out)
			}
			kill()
			k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
			if _, err := os.Lstat(tmp.Path); !os.IsNotExist(err) {
				t.Errorf("进程走了要清：%v\n%s", err, out)
			}
		})
	}
}

// 定了要删、删到一半被杀：下次 tick 接着删；定了要删的续跑不会拿去用。
func TestSweepTmpFinishesRemoving(t *testing.T) {
	e, k, out, dir := tmpStack(t, "")
	const id = "1111-100000-demo"
	tmp, err := e.Tmps().New(id, id)
	if err != nil {
		t.Fatal(err)
	}
	st := &state.State{Jobs: []*state.Job{{ID: id, Project: "demo", ProjectDir: dir, Title: "活", Status: state.StatusDone}}}
	if got := k.Runner.SweepTmp(st); len(got) != 1 || got[0].Path != tmp.Path {
		t.Fatalf("%v", got)
	}
	if _, ok := e.Tmps().OfJob(id); ok {
		t.Error("定了要删的不能再拿去用")
	}
	k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
	if _, err := os.Lstat(tmp.Path); !os.IsNotExist(err) {
		t.Errorf("%v\n%s", err, out)
	}
	list, bad := e.Tmps().List()
	if len(list)+len(bad) != 0 {
		t.Errorf("%v %v", list, bad)
	}
	ents, _ := os.ReadDir(e.Tmps().Dir)
	if slices.ContainsFunc(ents, func(d os.DirEntry) bool { return d.Name() != "remove.lock" }) {
		t.Errorf("%v", ents)
	}
}

// 看不到的不当成了结：项目目录不见了（外置盘没挂上、改名）、BACKLOG 没了、任务记下的分支查不到，等用户定和可续跑的都留着、说一次；看得到了照旧判断。
func TestSweepTmpProjectHidden(t *testing.T) {
	const item, id = "活 —— 要什么", "1111-100000-demo"
	for _, c := range []struct {
		name, todo, status, session string
	}{
		{"等用户定", "- [ ] " + item + "（卡住：等用户：付费，分支 agent/x）\n", state.StatusDone, ""},
		{"可续跑", "- [ ] " + item + "（进行中：agent/x 2026-01-01 10:00）\n", state.StatusInterrupted, "s"},
	} {
		for _, h := range []struct {
			name       string
			hide, show func(home, dir string) error
		}{
			{"项目目录不见了", func(_, dir string) error { return os.Rename(dir, dir+".off") }, func(_, dir string) error { return os.Rename(dir+".off", dir) }},
			{"BACKLOG 没了", func(_, dir string) error {
				return os.Rename(filepath.Join(dir, "docs"), filepath.Join(dir, "off"))
			}, func(_, dir string) error { return os.Rename(filepath.Join(dir, "off"), filepath.Join(dir, "docs")) }},
			{"记下的分支查不到", func(home, dir string) error { return testkit.GitTErr(home, dir, "branch", "-m", "main", "other") },
				func(home, dir string) error { return testkit.GitTErr(home, dir, "branch", "-m", "other", "main") }},
		} {
			t.Run(c.name+"/"+h.name, func(t *testing.T) {
				e, k, out, dir := tmpStack(t, c.todo)
				tmp, err := e.Tmps().New(id, id)
				if err != nil {
					t.Fatal(err)
				}
				st := &state.State{Jobs: []*state.Job{{ID: id, Project: "demo", ProjectDir: dir, DefaultBranch: "main", Item: item, Title: "活",
					Fingerprint: backlog.Fingerprint("- [ ] " + item), Status: c.status, SessionID: c.session, StartedAt: e.Now()}}}
				sweep := func() {
					t.Helper()
					k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
					if _, err := os.Lstat(tmp.Path); err != nil {
						t.Fatalf("不能清：%v\n%s", err, out)
					}
				}
				sweep()
				if err := h.hide(e.Home, dir); err != nil {
					t.Fatal(err)
				}
				sweep()
				sweep()
				if n := strings.Count(out.String(), "不知道这一条了结没有"); n != 1 {
					t.Errorf("原因说一次，说了 %d 次：%s", n, out)
				}
				if err := h.show(e.Home, dir); err != nil {
					t.Fatal(err)
				}
				sweep()
				os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 活\n"), 0o644)
				k.Runner.RemoveTmp(k.Runner.SweepTmp(st))
				if _, err := os.Lstat(tmp.Path); !os.IsNotExist(err) {
					t.Errorf("了结了要清：%v\n%s", err, out)
				}
			})
		}
	}
}

// 等用户定超过 30 天：任务记录已经不在 state.json，只剩 meta.json，条目还标着卡住，照样留着。
func TestSweepTmpWaitsPastPrune(t *testing.T) {
	const item, id = "活 —— 要什么", "1111-100000-demo"
	e, k, out, dir := tmpStack(t, "- [ ] "+item+"（卡住：等用户：付费，分支 agent/x）\n")
	tmp, err := e.Tmps().New(id, id)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(e.JobDir(id), 0o755)
	meta, _ := json.Marshal(&state.Job{ID: id, Project: "demo", ProjectDir: dir, DefaultBranch: "main", Item: item, Title: "活",
		Fingerprint: backlog.Fingerprint("- [ ] " + item), Status: state.StatusDone, StartedAt: e.Now()})
	os.WriteFile(filepath.Join(e.JobDir(id), "meta.json"), meta, 0o644)
	k.Runner.RemoveTmp(k.Runner.SweepTmp(&state.State{}))
	if _, err := os.Lstat(tmp.Path); err != nil {
		t.Errorf("还在等用户：%v\n%s", err, out)
	}
}
