package onboard

import (
	"os"
	"syscall"
)

// flushTerminalInput 丢掉终端里还没读的输入（TCFLSH + TCIFLUSH）。不是终端时什么也不做。
func flushTerminalInput(f *os.File) {
	const tcflsh, tciflush = 0x540B, 0
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tcflsh, tciflush)
}
