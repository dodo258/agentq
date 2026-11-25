package env

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// 门、任务里执行 agentq 命令（评测门跑的 agentq eval judge）：它自己的临时文件就该建在给它的 TMPDIR 里，
// 建运行环境时不能换成公共的，不然评测现场又堆到公共目录，门收尾时也清不到。
func TestNewKeepsTMPDIR(t *testing.T) {
	root := t.TempDir()
	caller := filepath.Join(root, "caller")
	if err := os.MkdirAll(caller, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	t.Setenv("TMPDIR", caller+"/")
	tmp, err := (&Env{Home: home}).Tmps().New("", "gate-x")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp.Value())
	e, err := New(home, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TMPDIR"); got != tmp.Value() {
		t.Errorf("TMPDIR 被换成了 %s", got)
	}
	dir, err := os.MkdirTemp("", "aq-*")
	if err != nil || filepath.Dir(dir) != tmp.Path {
		t.Errorf("临时文件要建在门的目录里：%s %v", dir, err)
	}
	e.Tmps().Leave() // 要起活得比自己久的进程时才换
	if got := os.Getenv("TMPDIR"); got != caller+"/" {
		t.Errorf("%s", got)
	}
}
