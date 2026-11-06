package tests

import (
	"os/exec"
	"runtime"
)

// plutilLint 校验 plist 格式。plutil 只有 macOS 有，别的平台不校验。
func plutilLint(path string) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	return exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput()
}
