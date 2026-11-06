// Package gate 是合并前的质量门：任务只做到分支可合并，引擎在合并态上跑门，过了由引擎合并并写收尾。
//
// 只有一条合并路径：门全设 off 也走这里，只是一道门都不跑。流程见 docs/reference/features.md 的质量门一节。
// 门防的是模型偷懒和误操作，任务有本机全部权限，不是安全边界。
//
// 加一道门：实现 Gate，用 Register 登记名字和默认档位。项目在 .agentq/gates.json 调档位（读 main 上的，任务改不了）。
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"agentq/internal/backend"
	"agentq/internal/env"
	"agentq/internal/state"
)

// Level 是一道门的档位：off 不跑，warn 只记录，error 不过不合并。
type Level string

const (
	LevelOff   Level = "off"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Outcome 是一道门的结论。unknown 是判不了：error 档也不放行。
type Outcome string

const (
	Pass    Outcome = "pass"
	Fail    Outcome = "fail"
	Unknown Outcome = "unknown"
)

// Result 是一道门跑完的结果，写进报告。
type Result struct {
	Gate    string  `json:"gate"`
	Outcome Outcome `json:"outcome"`
	Summary string  `json:"summary"`
	// Evidence 是命令原文、文件行号、日志路径。
	Evidence []string `json:"evidence,omitempty"`
	// Level 由引擎填：跑这道门时的档位。
	Level Level `json:"level,omitempty"`
	// Defer 非空表示这次不算结论，过一会儿重跑，比如审查撞了用量上限。不消耗退回的轮数。
	Defer string `json:"defer,omitempty"`
	// Limit：Defer 的原因是撞了用量上限，tick 要进冷却。ResetAt 是提示的重置时刻，零值表示读不出。
	Limit   bool      `json:"limit,omitempty"`
	ResetAt time.Time `json:"reset_at,omitzero"`
	// Auth：Defer 的原因是后端认证失败，tick 要暂停这个后端并通知用户。
	Auth bool `json:"auth,omitempty"`
	// Upstream：Defer 的原因是上游 5xx：按 state.TransientBackoff 退避重跑，连续用完才判不了。
	Upstream bool `json:"upstream,omitempty"`
	// Cached：这个 tree 上次的结果，这次没跑，不算次数。
	Cached bool `json:"cached,omitempty"`
	// Usage 是这道门调模型花的用量，算在任务头上。
	Usage *backend.JobUsage `json:"usage,omitempty"`
}

// Ready 是任务写的交付单。
type Ready = state.GateReady

// Candidate 是这次过门的候选：任务分支合进最新 main 得到的合并提交，在一次性 worktree 里。
type Candidate struct {
	JobID      string
	Project    string
	ProjectDir string
	Title      string
	Item       string
	Backend    string
	Branch     string
	// DefaultBranch 是要合进去的分支，这次过门里不变。
	DefaultBranch string
	// Head 是分支尖，Base 是默认分支基点，Merge 是候选合并提交。
	Head, Base, Merge string
	// Tree 是候选的 tree hash，PatchID 是任务改动的 patch-id：main 动过时 Tree 变，PatchID 不变。
	Tree, PatchID string
	// Dir 是一次性 worktree，门可以随便改，下一道门开始前引擎会还原。
	Dir string
	// WorkDir 是这次过门的私有目录，放日志。
	WorkDir string
	// Files 是相对 main 基点改动的文件。
	Files []string
	Type  string
	Ready Ready
	Round int
	// Product：agentq new 的第一版，或者条目标了（成品）。成品门只看它。
	Product bool
	// Timeout 是每个脚本、每次调模型的超时。超时算 unknown。
	Timeout time.Duration
	// Fresh：用户要再审一次，不沿用审查结论。
	Fresh bool
	// Upstream：前面已经因为上游 5xx 重跑了几次。
	Upstream int
	// Prior 是这次已经跑完的门的结果，比如让审查门看到测试门标出的事。
	Prior []Result
}

// Gate 是一道门。
type Gate interface {
	Name() string
	// AppliesTo 决定这个候选要不要跑这道门，比如界面门只在改到界面文件时跑。
	AppliesTo(c Candidate) bool
	Run(ctx context.Context, c Candidate) Result
}

// Deps 是门能用的东西。门不碰 state.json，也不写 BACKLOG。
type Deps struct {
	*env.Env
	// Git 在 dir 跑 git，不跑钩子，返回标准输出。
	Git func(ctx context.Context, dir string, args ...string) (string, error)
	// Script 在隔离环境跑一个脚本，见 ScriptOpts。
	Script func(ctx context.Context, o ScriptOpts) ScriptResult
	// CLI 跑一次后端 CLI，见 RunCLI。要调模型的门都走它。
	CLI func(ctx context.Context, inv backend.Invocation, o CLIOpts) error
}

type registration struct {
	name  string
	def   Level
	build func(Deps) Gate
}

var (
	regMu    sync.Mutex
	registry []registration
)

// Register 登记一道门，按登记顺序跑。返回撤销函数（测试用）。重名 panic：是接线写错了。
func Register(name string, def Level, build func(Deps) Gate) (undo func()) {
	regMu.Lock()
	defer regMu.Unlock()
	if slices.ContainsFunc(registry, func(r registration) bool { return r.name == name }) {
		panic("gate：重复登记 " + name)
	}
	registry = append(registry, registration{name, def, build})
	return func() {
		regMu.Lock()
		defer regMu.Unlock()
		registry = slices.DeleteFunc(registry, func(r registration) bool { return r.name == name })
	}
}

func registered() []registration {
	regMu.Lock()
	defer regMu.Unlock()
	return slices.Clone(registry)
}

// 内置的门。不花钱的在前：补丁门只看 git，测试都没过不用花额度审。默认档位写在这里，方案第 6 节定的。
func init() {
	Register("patch", LevelWarn, func(d Deps) Gate { return patchGate{d} })
	Register("test", LevelError, func(d Deps) Gate { return testGate{d} })
	Register("smoke", LevelWarn, func(d Deps) Gate { return smokeGate{d} })
	Register("review", LevelError, func(d Deps) Gate { return reviewGate{d} })
	Register("ui", LevelWarn, func(d Deps) Gate { return uiGate{d} })
}

// levelOf 是这道门这次的档位：项目调过的优先，没调的用默认。没登记的门算 off。
func levelOf(name string, levels map[string]Level) Level {
	if l, ok := levels[name]; ok {
		return l
	}
	for _, r := range registered() {
		if r.name == name {
			return r.def
		}
	}
	return LevelOff
}

// GatesFile 是项目调档位的文件，读 main 上的那份。
const GatesFile = ".agentq/gates.json"

// gatesConfig 是 .agentq/gates.json：{"levels":{"test":"warn","review":"off"},"ui_files":[".tsx","View.swift"]}。
type gatesConfig struct {
	Levels map[string]Level `json:"levels"`
	// UIFiles 是算界面文件的路径后缀，写了就替换默认的。
	UIFiles *[]string `json:"ui_files"`
}

// ParseLevels 解析 .agentq/gates.json。写坏了不放宽：报错，调用方照默认档位跑。不认识的门名不算错（可能是装了插件的机器写的），只写进说明。
func ParseLevels(raw []byte) (map[string]Level, []string, error) {
	var cfg gatesConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("不是合法的 JSON：%w", err)
	}
	var notes []string
	known := registered()
	for name, lvl := range cfg.Levels {
		if lvl != LevelOff && lvl != LevelWarn && lvl != LevelError {
			return nil, nil, fmt.Errorf("门 %s 的档位 %q 不认识，只有 off、warn、error", name, lvl)
		}
		if !slices.ContainsFunc(known, func(r registration) bool { return r.name == name }) {
			notes = append(notes, "不认识的门 "+name+"，没用")
		}
	}
	slices.Sort(notes)
	return cfg.Levels, notes, nil
}

// levelsAt 读基点上的 .agentq/gates.json，任务分支上的改动不算。读不了（没有这个文件是正常的）就用默认档位。
func (a *Service) levelsAt(ctx context.Context, dir, base string) (map[string]Level, []string) {
	if _, err := a.git(ctx, dir, "cat-file", "-e", base+":"+GatesFile); err != nil {
		return nil, nil
	}
	raw, err := a.git(ctx, dir, "show", base+":"+GatesFile)
	if err != nil {
		return nil, []string{GatesFile + " 读不了，按默认档位：" + err.Error()}
	}
	levels, notes, err := ParseLevels([]byte(raw))
	if err != nil {
		return nil, []string{GatesFile + " 有问题，按默认档位：" + err.Error()}
	}
	return levels, notes
}

func summarize(results []Result) string {
	var parts []string
	for _, r := range results {
		if r.Outcome != Pass {
			parts = append(parts, r.Gate+"："+r.Summary)
		}
	}
	return strings.Join(parts, "；")
}
