package setup

import (
	"errors"
	"fmt"
	"strings"
)

// bashRCNames：macOS 的终端开的是登录 shell，bash 只读这三个里第一个存在的。新建 .bash_profile 会让已有的 .profile 失效，所以先改已有的，都没有才新建第一个。
var bashRCNames = []string{".bash_profile", ".bash_login", ".profile"}

const gitMissingHint = "Git required; install Apple Command Line Tools or Git first"

// probeOS 检查系统版本和架构。
func probeOS(r *engineReport, arch string, probe func(string, ...string) ([]byte, error)) error {
	if arch != "arm64" && arch != "amd64" {
		return errors.New("this installer supports macOS arm64/amd64 only")
	}
	version, e := probe("/usr/bin/sw_vers", "-productVersion")
	if e != nil {
		return errors.New("cannot detect macOS version")
	}
	var major int
	if _, e = fmt.Sscanf(string(version), "%d", &major); e != nil || major < 13 {
		return errors.New("this installer requires macOS 13 or newer")
	}
	r.OSVersion = strings.TrimSpace(string(version))
	return nil
}
