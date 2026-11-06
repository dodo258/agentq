package env

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/fsx"
)

// AuthPausePath 是后端认证失败后的暂停记录。任务和门都可能撞上，启动前的登录检查读它。
func (a *Env) AuthPausePath(name string) string {
	return filepath.Join(a.DataDir(), "backend-auth", name)
}

// PauseAuth：因认证失败结束，记下这个后端暂停到什么时候。记不下只记日志：最多再浪费一个进程，不花模型的钱。
func (a *Env) PauseAuth(name string) time.Time {
	until := a.Now().Add(a.Cfg.LimitCooldown.Duration)
	err := fsx.WriteFileAtomic(a.AuthPausePath(name), []byte(until.Format(time.RFC3339)+"\n"))
	if err != nil {
		a.Logf("记不下后端 %s 的认证暂停：%v", name, err)
	}
	return until
}

// AuthPausedUntil 读暂停记录，过期或读不懂的顺手删掉。
func (a *Env) AuthPausedUntil(name string) (time.Time, bool) {
	b, err := os.ReadFile(a.AuthPausePath(name))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil || !a.Now().Before(t) {
		os.Remove(a.AuthPausePath(name))
		return time.Time{}, false
	}
	return t, true
}
