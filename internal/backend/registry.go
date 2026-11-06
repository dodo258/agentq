package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"agentq/internal/proc"
)

// Default 是不写 backend 配置时用的后端，老任务记录（Job.Backend 为空）也算它。
const Default = "claude"

// Factory 按 Options 建一个实例。
type Factory func(Options) Backend

var registry = struct {
	sync.RWMutex
	m map[string]Factory
}{m: map[string]Factory{}}

// Register 注册一个后端，放行它声明的环境变量，返回撤销注册的函数（测试用）。同名的后注册覆盖先注册的。
func Register(name string, f Factory) (undo func()) {
	keep := f(Options{}).EnvKeep()
	registry.Lock()
	defer registry.Unlock()
	old := registry.m[name]
	registry.m[name] = f
	proc.AddChildEnvKeep(keep...)
	return func() {
		registry.Lock()
		defer registry.Unlock()
		proc.RemoveChildEnvKeep(keep...)
		if old == nil {
			delete(registry.m, name)
		} else {
			registry.m[name] = old
		}
	}
}

// Known：这个名字注册过。空名字算 Default。
func Known(name string) bool {
	if name == "" {
		name = Default
	}
	registry.RLock()
	defer registry.RUnlock()
	_, ok := registry.m[name]
	return ok
}

// Names 是注册过的后端名，排好序。
func Names() []string {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]string, 0, len(registry.m))
	for n := range registry.m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Get 建一个实例。空名字算 Default；没注册过的名字（配置写错、老记录来自别的版本）返回一个什么都做不了的实例：
// 启动被拒绝，读结果当没有终态。调用方不用到处判断 nil。
func Get(name string, o Options) Backend {
	if name == "" {
		name = Default
	}
	registry.RLock()
	f := registry.m[name]
	registry.RUnlock()
	if f == nil {
		return missing{name}
	}
	return f(o)
}

// missing 是没注册过的后端。
type missing struct{ name string }

func (m missing) err() error {
	return fmt.Errorf("未知后端 %q，不接活（config.json 的 backend 写错了，或这个版本没有它；现有：%v）", m.name, Names())
}
func (m missing) Name() string      { return m.name }
func (m missing) Caps() Caps        { return Caps{} }
func (m missing) EnvKeep() []string { return nil }
func (m missing) Probe(context.Context, ProbeEnv) (Probe, error) {
	return Probe{}, m.err()
}
func (m missing) Auth(context.Context) Auth { return Auth{State: AuthError, Detail: m.err().Error()} }
func (m missing) Build(Request) (Invocation, []Degradation, error) {
	return Invocation{}, nil, m.err()
}
func (m missing) Parse(JobRef, Exit) Result             { return Result{} }
func (m missing) LastActivity(JobRef) (time.Time, bool) { return time.Time{}, false }
func (m missing) CanResume(string) bool                 { return false }
func (m missing) RecordedModel([]string) string         { return "" }

// ErrUnsupported 是 Build 因为做不到 Must 而拒绝的错误，用 errors.Is 认。
var ErrUnsupported = errors.New("这个后端做不到")
