package gate_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 门留下的临时目录什么时候还归门：在过门、门进程拿着锁、它的进程组里还有进程（认不出的也算）。
func TestRunHeld(t *testing.T) {
	a := &gate.Service{Env: env.ForPaths(t.TempDir(), config.Config{})}
	const id, mark = "1111-100000-demo", "gate-1111-100000-demo"
	dir := a.Dir(id)
	os.MkdirAll(dir, 0o700)
	st := &state.State{Jobs: []*state.Job{{ID: id, Gate: &state.GateRecord{State: state.GateQueued}}}}
	if a.RunHeld(st, mark) {
		t.Error("没在跑，什么都没留下")
	}
	if a.RunHeld(st, "pm-ui-x") {
		t.Error("不是门的标记")
	}

	st.Jobs[0].Gate.State = state.GateRunning
	if !a.RunHeld(st, mark) {
		t.Error("在过门")
	}
	lockFile := filepath.Join(dir, "lock")
	os.Remove(lockFile)
	if a.RunHeld(st, mark); fileExists(lockFile) {
		t.Error("在过门时不能试拿门的锁：刚起的门进程会以为已经有一个在跑")
	}

	st.Jobs[0].Gate.State = state.GateQueued
	lock, err := fsx.AcquireLock(lockFile, 0)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	if !a.RunHeld(st, mark) {
		t.Error("门进程拿着锁")
	}
	lock.Close()

	cmd := exec.Command("/bin/sleep", "30")
	cmd.Env = []string{proc.JobEnv + "=" + mark}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pgid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	if !a.RunHeld(st, mark) {
		t.Error("进程组里还有进程")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if a.RunHeld(st, mark) {
		t.Error("进程组没了")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// 过门结束时只删自己这次的，别的门、任务的不动；还有进程在用的留给 tick。
func TestDropTmps(t *testing.T) {
	root := t.TempDir()
	caller := filepath.Join(root, "caller")
	os.Mkdir(caller, 0o700)
	t.Setenv("TMPDIR", caller)
	store := proc.TmpStore{Dir: filepath.Join(root, "data", "tmp")}
	mk := func(job, mark string) proc.Tmp {
		tmp, err := store.New(job, mark)
		if err != nil {
			t.Fatal(err)
		}
		return tmp
	}
	a, b := mk("", "gate-x"), mk("", "gate-x")
	other, task := mk("", "gate-x-2"), mk("x", "x")
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Dir, cmd.Env = b.Path, []string{}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	gate.DropTmps(store, "gate-x")
	for tmp, keep := range map[*proc.Tmp]bool{&a: false, &b: true, &other: true, &task: true} {
		if fileExists(tmp.Path) != keep {
			t.Errorf("%s（%v）留着应该是 %v", tmp.Path, tmp.Marks, keep)
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	gate.DropTmps(store, "gate-x")
	if fileExists(b.Path) {
		t.Error("没人用了要删")
	}
}

// 在任务里执行的 tick 起门：门进程比那个任务活得久，用的是原来的 TMPDIR；截图、脚本的目录也不建在任务的目录里。
func TestGateStartLeavesJobTmp(t *testing.T) {
	f := newFix(t, fixBacklog)
	root := t.TempDir()
	caller := filepath.Join(root, "caller")
	os.Mkdir(caller, 0o700)
	t.Setenv("TMPDIR", caller+"/")
	job, err := f.a.Tmps().New("job-a", "job-a")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	f.a.SelfPath = filepath.Join(root, "agentq")
	os.WriteFile(f.a.SelfPath, []byte("#!/bin/sh\necho \"$TMPDIR\" > "+out+".part && mv "+out+".part "+out+"\n"), 0o755)
	id := "1111-100000-demo"
	os.MkdirAll(f.a.JobDir(id), 0o755)
	f.st.Jobs = append(f.st.Jobs, &state.Job{ID: id, Project: "a", ProjectDir: "/p/a", Status: state.StatusDone,
		Gate: &state.GateRecord{State: state.GateQueued, Branch: "b", QueuedAt: f.a.Now()}})
	t.Setenv("TMPDIR", job.Value())
	if n := f.a.StartQueued(f.st); n != 1 {
		t.Fatalf("起了 %d 个", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !fileExists(out) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if b, err := os.ReadFile(out); err != nil || strings.TrimSpace(string(b)) != caller+"/" {
		t.Errorf("门进程的 TMPDIR 是 %q（%v），应该是 %s/", b, err, caller)
	}

	t.Setenv("TMPDIR", job.Value())
	script := filepath.Join(root, "s")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$TMPDIR\"\n"), 0o755)
	log := filepath.Join(root, "log")
	gate.RunScript(context.Background(), gate.ScriptOpts{Script: script, Dir: root, Log: log, Timeout: 10 * time.Second, WorkDir: root, Mark: "pm-ui-x", Tmps: f.a.Tmps()})
	if b, err := os.ReadFile(log); err != nil || filepath.Dir(filepath.Clean(strings.TrimSpace(string(b)))) != caller {
		t.Errorf("脚本的目录建在了 %q（%v），应该在 %s 里", b, err, caller)
	}
}

// 评测门：构建和 agentq eval judge 各有自己的 TMPDIR，跑完连里面留的现场一起清。
func TestEvalGateOwnTmp(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8})
	seen := filepath.Join(t.TempDir(), "seen")
	raw, err := os.ReadFile(f.a.SelfPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(raw), "#!/bin/sh\n", "#!/bin/sh\nmkdir -p \"$TMPDIR/aq-1/现场\"; echo \"$TMPDIR\" > "+seen+"\n", 1)
	os.WriteFile(f.a.SelfPath, []byte(body), 0o755)
	j := f.claim("做事", "agent/x", map[string]string{"internal/textfmt/a.go": "package textfmt\n"}, ready("agent/x"))
	res := f.run(j)
	if r := evalResult(t, res); r.Outcome != gate.Pass {
		t.Fatalf("%+v", r)
	}
	dir := strings.TrimSpace(testkit.ReadFile(t, seen))
	if filepath.Dir(strings.TrimSuffix(dir, "/")) != f.a.Tmps().Dir { // 测试的数据目录在临时目录里，建在登记处
		t.Errorf("评测的 TMPDIR 是 %s", dir)
	}
	if fileExists(dir) {
		t.Error("跑完要清")
	}
}
