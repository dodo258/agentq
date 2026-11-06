package onboard

import (
	"os"
	"syscall"
	"unsafe"
)

// flushTerminalInput 丢掉终端里还没读的输入（TIOCFLUSH + FREAD）。不是终端时什么也不做。
func flushTerminalInput(f *os.File) {
	what := int32(1) // FREAD
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCFLUSH), uintptr(unsafe.Pointer(&what)))
}
