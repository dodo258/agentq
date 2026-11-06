package setup

import (
	"errors"
	"os"

	"agentq/internal/service"
)

// bashRCNames：Linux 的终端开的是交互 shell，读 .bashrc。
var bashRCNames = []string{".bashrc"}

const gitMissingHint = "Git required; install it with your distribution's package manager first"

// probeOS 检查架构，读发行版名字（读不到不算失败：只用来显示）。
func probeOS(r *engineReport, arch string, _ func(string, ...string) ([]byte, error)) error {
	if arch != "arm64" && arch != "amd64" {
		return errors.New("this installer supports Linux arm64/amd64 only")
	}
	r.OSVersion = "Linux"
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if b, err := os.ReadFile(p); err == nil {
			if name := service.ParseOSRelease(string(b)); name != "" {
				r.OSVersion = name
				break
			}
		}
	}
	return nil
}
