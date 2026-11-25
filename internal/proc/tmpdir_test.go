package proc

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tmpEnv：调用方的 TMPDIR 换成一个放了东西的目录，登记处在它外面，和真实安装一样。
func tmpEnv(t *testing.T) (TmpStore, string) {
	t.Helper()
	root := t.TempDir()
	caller := filepath.Join(root, "caller")
	if err := os.MkdirAll(filepath.Join(caller, "别人的"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, "别人的", "文件"), []byte("别动"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", caller+"/")
	return TmpStore{Dir: filepath.Join(root, "data", "tmp")}, caller
}

func checkCaller(t *testing.T, caller string, want ...string) {
	t.Helper()
	ents, err := os.ReadDir(caller)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want = append(want, "别人的")
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("调用方的 TMPDIR 里是 %v，应该是 %v", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(caller, "别人的", "文件")); err != nil || string(b) != "别动" {
		t.Errorf("调用方原有的文件被动了：%v %q", err, b)
	}
}

func TestTmpNewAndRemove(t *testing.T) {
	store, caller := tmpEnv(t)
	a, err := store.New("job-a", "job-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.New("job-b", "job-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path || filepath.Dir(a.Path) != caller {
		t.Fatalf("各用各的，建在调用方的 TMPDIR 里：%s %s", a.Path, b.Path)
	}
	fi, err := os.Lstat(a.Path)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("只有自己能进：%v %v", fi, err)
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Gid) != os.Getegid() {
		t.Errorf("组 %d 应该是自己的组 %d", st.Gid, os.Getegid())
	}
	env := a.Env([]string{"A=1", "TMPDIR=/old", "B=2"})
	if !slices.Equal(env, []string{"A=1", "B=2", "TMPDIR=" + a.Path + "/"}) {
		t.Errorf("换 TMPDIR（原来带斜杠就也带上）：%v", env)
	}
	if got, ok := store.OfJob("job-b"); !ok || got.Path != b.Path {
		t.Errorf("按任务找登记：%v %v", got, ok)
	}

	// 里面有没写权限的目录（Go 模块缓存）、指到外面的软链接
	ro := filepath.Join(a.Path, "mod", "x")
	os.MkdirAll(ro, 0o755)
	os.WriteFile(filepath.Join(ro, "f"), nil, 0o444)
	os.Chmod(ro, 0o555)
	os.Symlink(filepath.Join(caller, "别人的"), filepath.Join(a.Path, "link"))
	if err := store.Remove(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.Path); !os.IsNotExist(err) {
		t.Errorf("目录要没了：%v", err)
	}
	list, bad := store.List()
	if len(list) != 1 || list[0].Path != b.Path || len(bad) != 0 {
		t.Errorf("登记只剩 b：%v %v", list, bad)
	}
	checkCaller(t, caller, filepath.Base(b.Path))
	os.RemoveAll(b.Path) // 子进程自己把它删了：只去登记
	if err := store.Remove(b); err != nil {
		t.Fatal(err)
	}
	if list, _ := store.List(); len(list) != 0 {
		t.Errorf("%v", list)
	}
}

// 路径上已经不是当初建的那个目录：换成了别的目录、软链接，都不动。
func TestTmpRemoveOnlyOwn(t *testing.T) {
	for _, kind := range []string{"换了目录", "换成软链接"} {
		t.Run(kind, func(t *testing.T) {
			store, caller := tmpEnv(t)
			a, err := store.New("job-a", "job-a")
			if err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(caller, "挪走的")
			os.Rename(a.Path, keep) // inode 还占着，新建的不会碰巧一样
			if kind == "换成软链接" {
				os.Symlink(filepath.Join(caller, "别人的"), a.Path)
			} else {
				os.Mkdir(a.Path, 0o755)
				os.WriteFile(filepath.Join(a.Path, "f"), nil, 0o644)
			}
			if _, ok := store.OfJob("job-a"); ok {
				t.Error("续跑不能拿它去用")
			}
			if err := store.Remove(a); err == nil {
				t.Error("要说没动它")
			}
			if _, err := os.Lstat(a.Path); err != nil {
				t.Errorf("不能动：%v", err)
			}
			if list, _ := store.List(); len(list) != 0 {
				t.Errorf("对不上的登记去掉，不然每次都说一遍：%v", list)
			}
			checkCaller(t, caller, filepath.Base(a.Path), "挪走的")
		})
	}
}

// 登记读不懂、指到别处的，不当成自己的目录。
func TestTmpBadRecords(t *testing.T) {
	store, caller := tmpEnv(t)
	os.MkdirAll(store.Dir, 0o700)
	for name, text := range map[string]string{
		"agqbroken": "{",
		"agqother":  `{"path":"` + caller + `/别人的","inode":1,"marks":["x"]}`,
		"agqnoino":  `{"path":"` + caller + `/agqnoino","marks":["x"]}`,
		"别人的":       `{"path":"` + caller + `/别人的","inode":1,"marks":["x"]}`,
	} {
		os.WriteFile(filepath.Join(store.Dir, name+".json"), []byte(text), 0o600)
	}
	if list, bad := store.List(); len(list) != 0 || len(bad) != 4 {
		t.Errorf("%v %v", list, bad)
	}
}

func TestTmpBase(t *testing.T) {
	store, caller := tmpEnv(t)
	if got := store.base(); filepath.Clean(got) != caller {
		t.Errorf("调用方指定了 TMPDIR 就建在里面：%s", got)
	}
	// 装在临时目录里的（评测现场、测试）：放数据目录里，跟着现场一起清
	inner := TmpStore{Dir: filepath.Join(caller, "home", ".agentq", "tmp")}
	os.MkdirAll(inner.Dir, 0o700)
	if got := inner.base(); got != inner.Dir {
		t.Errorf("%s", got)
	}
	// macOS 默认的 TMPDIR 太长，换到 /var/tmp。不能是 /tmp：系统每天删里面 3 天没动的文件
	for _, dir := range []string{"/var/folders/ns/5_xxxxxxxxxxxxxxxxxxxxxxxxx0000gn/T/", "/private/var/folders/ns/5_xxxxxxxxxxxxxxxxxxxxxxxxx0000gn/T"} {
		t.Setenv("TMPDIR", dir)
		if got := store.base(); got != "/var/tmp" {
			t.Errorf("%s → %s", dir, got)
		}
	}
	t.Setenv("TMPDIR", "/var/folders/ns/5_xxxxxxxxxxxxxxxxxxxxxxxxx0000gn/T/别的")
	if got := store.base(); got == "/var/tmp" {
		t.Error("只有默认的那个才换")
	}
}

// 默认情况下的路径不比系统默认的 TMPDIR 长，子进程按原来的写法建 socket 照样建得起来。
func TestTmpSocketFits(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("路径上限的问题只在 macOS 默认的 TMPDIR 上")
	}
	store, _ := tmpEnv(t)
	t.Setenv("TMPDIR", "/var/folders/ns/5_xxxxxxxxxxxxxxxxxxxxxxxxx0000gn/T/")
	a, err := store.New("job-a", "job-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Remove(a); err != nil {
			t.Error(err)
		}
	})
	if len(a.Value()) > len(os.Getenv("TMPDIR")) {
		t.Errorf("%s 比默认的长", a.Value())
	}
	// 本机实测默认 TMPDIR 下最长的 socket：cmux-cua-501/com.cmuxterm.app/cmux-cua-codex.sock（共 98 字节）
	sock := filepath.Join(a.Path, strings.Repeat("x", 98-49))
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

// 任务里执行 agentq 命令：换回建目录时的 TMPDIR，它起的任务不会建在这个任务的目录里面。
func TestTmpLeave(t *testing.T) {
	store, caller := tmpEnv(t)
	a, err := store.New("job-a", "job-a")
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(a.Path, "sub")
	os.Mkdir(inside, 0o700)
	for _, dir := range []string{a.Value(), inside} {
		t.Setenv("TMPDIR", dir)
		store.Leave()
		if got := os.Getenv("TMPDIR"); got != caller+"/" {
			t.Errorf("%s → %s", dir, got)
		}
		b, err := store.New("job-b", "job-b")
		if err != nil || filepath.Dir(b.Path) != caller {
			t.Errorf("%v %v", b, err)
		}
	}
	t.Setenv("TMPDIR", caller)
	store.Leave()
	if got := os.Getenv("TMPDIR"); got != caller {
		t.Errorf("不在登记过的目录里不动：%s", got)
	}
	other := TmpStore{Dir: filepath.Join(t.TempDir(), "tmp")}
	t.Setenv("TMPDIR", a.Value())
	other.Leave()
	if got := os.Getenv("TMPDIR"); got != a.Value() {
		t.Errorf("别的安装的目录不认：%s", got)
	}
}

func waitUse(t *testing.T, a Tmp, want bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		scan, err := ScanTmp(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		why := scan.Users(a)
		if (why != "") == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("在用应该是 %v：%q", want, why)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// 还有进程在用：带着任务标记、环境里有这个目录、工作目录或打开的文件在里面（系统自带的程序看不到环境，靠这个）。
func TestTmpInUse(t *testing.T) {
	store, _ := tmpEnv(t)
	a, err := store.New("job-a", "job-a")
	if err != nil {
		t.Fatal(err)
	}
	waitUse(t, a, false)
	bare := []string{"PATH=/usr/bin:/bin"}
	f, err := os.Create(filepath.Join(a.Path, "log"))
	if err != nil {
		t.Fatal(err)
	}
	waitUse(t, a, false) // 自己开着的不算
	for _, c := range []struct {
		name string
		make func() *exec.Cmd
	}{
		{"带着标记", func() *exec.Cmd {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(bare, JobEnv+"=job-a", "AGENTQ_PROC_TEST_SLEEP=1")
			return cmd
		}},
		{"环境里的 TMPDIR", func() *exec.Cmd {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(bare, "TMPDIR="+a.Value(), "AGENTQ_PROC_TEST_SLEEP=1")
			return cmd
		}},
		{"工作目录在里面", func() *exec.Cmd {
			cmd := exec.Command("/bin/sleep", "30")
			cmd.Env, cmd.Dir = bare, a.Path
			return cmd
		}},
		{"开着里面的文件", func() *exec.Cmd {
			cmd := exec.Command("/bin/sleep", "30")
			cmd.Env, cmd.Stdout = bare, f
			return cmd
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := c.make()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				waitUse(t, a, false)
			}()
			waitUse(t, a, true)
		})
	}
	f.Close()
	t.Run("别的任务的不算", func(t *testing.T) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(bare, JobEnv+"=job-a-2", "AGENTQ_PROC_TEST_SLEEP=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		waitEnvVisible(t, cmd.Process.Pid)
		waitUse(t, a, false)
	})
	t.Run("查不了", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ScanTmp(ctx); err == nil {
			t.Error("查不了要报错，调用方当还在用")
		}
	})
}

func TestGroupLeft(t *testing.T) {
	start := func(path string, args []string, env ...string) int {
		t.Helper()
		cmd := exec.Command(path, args...)
		cmd.Env = append(env, "PATH=/usr/bin:/bin")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd.Process.Pid
	}
	ours := start(os.Args[0], nil, JobEnv+"=job-a", "AGENTQ_PROC_TEST_SLEEP=1")
	other := start(os.Args[0], nil, JobEnv+"=job-b", "AGENTQ_PROC_TEST_SLEEP=1")
	waitEnvVisible(t, ours)
	waitEnvVisible(t, other)
	if got := GroupLeft(ours, "job-a"); got != ProcOurs {
		t.Errorf("带标记的：%v", got)
	}
	if got := GroupLeft(other, "job-a"); got != ProcGone {
		t.Errorf("组号被别的程序占用：%v", got)
	}
	if runtime.GOOS == "darwin" { // 系统自带的程序看不到环境：说不准，当还在
		if got := GroupLeft(start("/bin/sleep", []string{"30"}), "job-a"); got != ProcUnsure {
			t.Errorf("认不出的：%v", got)
		}
	}
	if got := GroupLeft(0, "job-a"); got != ProcGone {
		t.Errorf("%v", got)
	}
}
