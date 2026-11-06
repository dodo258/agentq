package fsx

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 重命名失败（目标是目录）不留下临时文件。
func TestRegressAtomicWriteCleansTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "meta.json")
	if err := os.MkdirAll(filepath.Join(target, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("{}")); err == nil {
		t.Fatal("目标是目录应报错")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("留下了临时文件：%v", ents)
	}
}

// 卡住的 tick 冻着锁时 status 要说得出是谁：LockHolder 给 pid，放开后不再算拿着。锁文件是旧内容也一样。
func TestLockHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tick.lock")
	if _, _, held := LockHolder(path); held {
		t.Fatal("文件不存在不算拿着")
	}
	if err := os.WriteFile(path, []byte("上次的内容，比 pid 长得多\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, held := LockHolder(path); held {
		t.Fatal("没人拿不算拿着")
	}
	lock, err := AcquireLock(path, 0)
	if err != nil || lock == nil {
		t.Fatalf("拿锁：%v %v", lock, err)
	}
	pid, since, held := LockHolder(path)
	if !held || pid != os.Getpid() {
		t.Errorf("拿着时：pid %d 拿着 %v，自己是 %d", pid, held, os.Getpid())
	}
	if d := time.Since(since); d < -time.Minute || d > time.Minute {
		t.Errorf("拿锁时间 %v 不对", since)
	}
	if other, err := AcquireLock(path, 0); err != nil || other != nil {
		t.Errorf("LockHolder 不该影响锁：%v %v", other, err)
	}
	lock.Close()
	if _, _, held := LockHolder(path); held {
		t.Error("放开后还算拿着")
	}
	if again, err := AcquireLock(path, 0); err != nil || again == nil {
		t.Errorf("LockHolder 放开后拿不到：%v %v", again, err)
	} else {
		again.Close()
	}
}

func TestExpandHome(t *testing.T) {
	if got := ExpandHome("/h", "~/projects"); got != "/h/projects" {
		t.Error(got)
	}
	if got := ExpandHome("/h", "/abs"); got != "/abs" {
		t.Error(got)
	}
}
