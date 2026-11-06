package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/testkit"
)

// with-lock.sh 用真实的 stat。锁目录没有 pid 文件时靠修改时间判断是不是死锁。
type lockRun struct {
	dir, marker string
	cmd         *exec.Cmd
	out         *strings.Builder
}

func startWithLock(t *testing.T, path string) *lockRun {
	t.Helper()
	src, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := &lockRun{dir: filepath.Join(dir, "locks"), marker: filepath.Join(dir, "ran"), out: &strings.Builder{}}
	if err := os.MkdirAll(filepath.Join(r.dir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.cmd = exec.Command("/bin/bash", filepath.Join(src, "support", "scripts", "with-lock.sh"), "x", "/bin/sh", "-c", "echo ok > "+fsx.ShQuote(r.marker))
	r.cmd.Env = []string{"PATH=" + path, "HOME=" + dir, "AGENTQ_LOCK_DIR=" + r.dir}
	r.cmd.Stdout, r.cmd.Stderr = r.out, r.out
	return r
}

func (r *lockRun) age(t *testing.T, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(filepath.Join(r.dir, "x"), old, old); err != nil {
		t.Fatal(err)
	}
}

func (r *lockRun) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		r.cmd.Process.Kill()
		<-done
		t.Fatalf("with-lock.sh 过了 %s 还没结束：%s", d, r.out)
		return nil
	}
}

func (r *lockRun) mustHaveRun(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(r.marker); err != nil {
		t.Errorf("命令没执行：%v\n%s", err, r.out)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "x")); !os.IsNotExist(err) {
		t.Errorf("锁没释放：%v", err)
	}
}

const lockSystemPath = "/usr/bin:/bin"

// 没有 pid、超过 30 秒的旧锁：回收，命令确实执行。GNU 的 stat -f 会往 stdout 打文件系统信息，
// 老写法在 Linux 上这里算术报错、脚本中止。
func TestWithLockReclaimsOldLockWithoutPID(t *testing.T) {
	t.Parallel()
	r := startWithLock(t, lockSystemPath)
	r.age(t, time.Minute)
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.wait(t, 20*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	r.mustHaveRun(t)
}

// macOS 上也能测到老写法的问题：假 stat 按 GNU 的样子响应。
func TestWithLockGNUStatFileSystemOutput(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	old := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	testkit.WriteInstall(t, filepath.Join(bin, "stat"), fmt.Sprintf(`#!/bin/sh
case "$1" in
  -c) echo %s ;;
  -f) printf '  File: "%%s"\n    ID: 1 Namelen: 255\n' "$3"; exit 1 ;;
esac
`, old), 0o755)
	r := startWithLock(t, bin+":"+lockSystemPath)
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.wait(t, 20*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	r.mustHaveRun(t)
}

// BSD 的 stat：-c 不认，-f %m 才给时间。
func TestWithLockBSDStat(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	old := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	testkit.WriteInstall(t, filepath.Join(bin, "stat"), fmt.Sprintf(`#!/bin/sh
case "$1" in
  -c) echo 'stat: illegal option -- c' >&2; exit 1 ;;
  -f) echo %s ;;
esac
`, old), 0o755)
	r := startWithLock(t, bin+":"+lockSystemPath)
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.wait(t, 20*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	r.mustHaveRun(t)
}

// 刚建的锁（mkdir 和写 pid 之间）和读不出修改时间的锁都不回收；持有者放手后拿到锁、执行命令。
func TestWithLockKeepsFreshOrUnknownLock(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	testkit.WriteInstall(t, filepath.Join(bin, "stat"), "#!/bin/sh\necho 'not a number'\nexit 1\n", 0o755)
	for name, path := range map[string]string{"刚建的": lockSystemPath, "读不出时间": bin + ":" + lockSystemPath} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := startWithLock(t, path)
			if err := r.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(1500 * time.Millisecond)
			if _, err := os.Stat(r.marker); err == nil {
				t.Fatalf("不该抢走别人的锁：\n%s", r.out)
			}
			if _, err := os.Stat(filepath.Join(r.dir, "x")); err != nil {
				t.Fatalf("锁被删了：%v\n%s", err, r.out)
			}
			if err := os.RemoveAll(filepath.Join(r.dir, "x")); err != nil {
				t.Fatal(err)
			}
			if err := r.wait(t, 20*time.Second); err != nil {
				t.Fatalf("%v\n%s", err, r.out)
			}
			r.mustHaveRun(t)
		})
	}
}
