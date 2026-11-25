// Package lock 是文件锁：进程没了锁自己就没了。
package lock

import (
	"os"
	"syscall"
)

// Flock 拿文件锁。wait 为假时拿不到返回 nil。关文件就放开。
func Flock(path string, wait bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for err = syscall.EINTR; err == syscall.EINTR; {
		err = syscall.Flock(int(f.Fd()), how)
	}
	if err == nil {
		return f, nil
	}
	f.Close()
	if err == syscall.EWOULDBLOCK {
		return nil, nil
	}
	return nil, err
}
