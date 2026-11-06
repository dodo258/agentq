package backend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ProbeWait：探测命令最多等多久。
const ProbeWait = 20 * time.Second

// ProbePath 按目标环境的 PATH 找命令（不用当前 shell 的）。带路径的相对名字按 home 算。
func ProbePath(home, name string, env []string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(home, name)
		}
		return exec.LookPath(name)
	}
	for _, value := range env {
		if path, ok := strings.CutPrefix(value, "PATH="); ok {
			for _, dir := range filepath.SplitList(path) {
				if filepath.IsAbs(dir) {
					if p, e := exec.LookPath(filepath.Join(dir, name)); e == nil {
						return p, nil
					}
				}
			}
		}
	}
	return "", exec.ErrNotFound
}

// ProbeCommand 在 home 里用目标环境跑一个探测命令，返回标准输出。
func ProbeCommand(ctx context.Context, home, path string, env []string, args ...string) ([]byte, error) {
	path, e := ProbePath(home, path, env)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeWait)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Dir = home
	c.Env = env
	return c.Output()
}
