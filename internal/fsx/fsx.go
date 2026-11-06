// Package fsx 安全地读写本地文件：原子写、文件锁、读尾部。
package fsx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ExpandHome 把开头的 ~ 换成 home。
func ExpandHome(home, p string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// CheckParents：path 要在 home 下，home 以下的祖先目录都不能是软链接。安装写文件之前都查一遍，不跟着软链接写到别处。祖先不存在不算错。
func CheckParents(home, path string) error {
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("install path outside HOME: %s", path)
	}
	for p := filepath.Dir(path); p != home && p != filepath.Dir(p); p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install ancestor is a symlink: %s; not following it", p)
		}
	}
	return nil
}

// WriteFileAtomic 先写同目录临时文件再 rename，读的一方不会看到写了一半的文件。
// path 是软链接时写到它指向的文件，不把链接换成普通文件。
func WriteFileAtomic(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = real
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func XmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ShQuote 把 s 包成 sh 的单引号字符串。
func ShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ReadTail 读文件最后 max 字节（从完整的一行开始）。
func ReadTail(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cut := false
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		if _, err := f.Seek(fi.Size()-max, io.SeekStart); err != nil {
			return "", err
		}
		cut = true
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := string(b)
	if cut {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, nil
}

// WriteKeepMode 原子写，保留文件原来的权限（BACKLOG 不是秘密，别变成只有自己能读）。
func WriteKeepMode(path string, data []byte) error {
	fi, err := os.Stat(path)
	if werr := WriteFileAtomic(path, data); werr != nil {
		return werr
	}
	if err != nil {
		return nil
	}
	return os.Chmod(path, fi.Mode().Perm())
}

// FileLock 用 flock 拿 ~/.agentq/tick.lock。
type FileLock struct{ f *os.File }

func (l *FileLock) Close() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

// AcquireLock：wait 为 0 时拿不到立刻返回 (nil, nil)；否则最多等 wait。
func AcquireLock(path string, wait time.Duration) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			noteHolder(f)
			return &FileLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// noteHolder 把自己的 pid 写进锁文件，mtime 就是拿到锁的时间，给 LockHolder 看。写不进去不影响锁本身（锁靠 flock，磁盘满时照样要能拿）。
func noteHolder(f *os.File) {
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
}

// LockHolder 看这把锁现在有没有人拿着：拿着的话返回 pid（读不出是 0）和拿到锁的时间。
// 试拿共享锁再马上放开：极短的窗口里撞上同时开始的 tick，那个 tick 会当成「上一个没结束」跳过一次，下次定时接上。只在 status 这种人手动看的地方用。
func LockHolder(path string) (pid int, since time.Time, held bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return 0, time.Time{}, false
		}
		b, _ := io.ReadAll(io.LimitReader(f, 32))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if fi, err := f.Stat(); err == nil {
			since = fi.ModTime()
		}
		return pid, since, true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return 0, time.Time{}, false
}

// TailLines 读文件末尾最多 n 行（只读最后 64KB）。
func TailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 64 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		_, _ = f.Seek(fi.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
