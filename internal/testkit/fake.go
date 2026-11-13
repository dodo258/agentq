package testkit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
)

// FakeOpts 决定假后端有哪些能力。能力全开用 AllCaps。
type FakeOpts struct {
	Caps backend.Caps
	Auth backend.Auth
	// EnvKeep 是它声明的环境变量。注册时会写进全局白名单，有这个的用例不能 t.Parallel。
	EnvKeep []string
	// Path 是假 CLI 脚本，空的话只能测收尾。
	Path string
	// ToolLog、LiveUsage 决定实不实现对应的可选接口。
	ToolLog, LiveUsage bool
	// NoTimestamps：事件流没有时间戳，用 backend.NoteSeen 补。
	NoTimestamps bool
	// SkipCheckMust：Build 不查硬要求，模拟新后端漏写。
	SkipCheckMust bool
}

// AllCaps 是全部能力都有的 Caps。
func AllCaps() backend.Caps {
	return backend.Caps{Resume: true, PresetSession: true, HardBudget: true, MaxTurns: true, Schema: true, ToolProfiles: true, WebTools: true,
		NoExternalMCP: true, Usage: true, Cost: true, LimitSignal: true, AuthProbe: true, Skills: true, ReviewProfile: true, ImageInput: true}
}

// Fake 是假后端。输出是一行一个 JSON 的事件流（result.json），见 FakeEvent。
type Fake struct {
	name string
	o    FakeOpts
	self backend.Backend // 带上可选接口的那个实例：能力检查要看到它
}

// FakeEvent 是假 CLI 输出的一行。type：session、tool、usage、result。
type FakeEvent struct {
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Cmd        string          `json:"cmd,omitempty"`
	At         *time.Time      `json:"at,omitempty"`
	Input      int64           `json:"input,omitempty"`
	Output     int64           `json:"output,omitempty"`
	Cost       float64         `json:"cost,omitempty"`
	OK         bool            `json:"ok,omitempty"`
	Text       string          `json:"text,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Turns      int             `json:"turns,omitempty"`
	Status     int             `json:"status,omitempty"`
	Auth       bool            `json:"auth,omitempty"`
	MaxBudget  bool            `json:"max_budget,omitempty"`
}

var fakeSeq atomic.Int64

// RegisterFake 注册一个假后端，测试结束时撤销，返回它的名字（每次不同，能并行）。
func RegisterFake(t *testing.T, o FakeOpts) string {
	t.Helper()
	name := "fake" + strconv.FormatInt(fakeSeq.Add(1), 10)
	undo := backend.Register(name, func(backend.Options) backend.Backend {
		f := &Fake{name: name, o: o}
		f.self = f
		switch {
		case o.ToolLog && o.LiveUsage:
			f.self = struct {
				*Fake
				fakeTools
				fakeLive
			}{f, fakeTools{f}, fakeLive{f}}
		case o.ToolLog:
			f.self = struct {
				*Fake
				fakeTools
			}{f, fakeTools{f}}
		case o.LiveUsage:
			f.self = struct {
				*Fake
				fakeLive
			}{f, fakeLive{f}}
		}
		return f.self
	})
	t.Cleanup(undo)
	return name
}

func (f *Fake) Name() string       { return f.name }
func (f *Fake) Caps() backend.Caps { return f.o.Caps }
func (f *Fake) EnvKeep() []string  { return f.o.EnvKeep }

func (f *Fake) Probe(ctx context.Context, env backend.ProbeEnv) (backend.Probe, error) {
	if f.o.Path == "" {
		return backend.Probe{}, errors.New("假 CLI 没装")
	}
	if f.o.Auth.Blocked() {
		return backend.Probe{Path: f.o.Path, Version: "fake 1.0"}, errors.New(f.o.Auth.Detail)
	}
	return backend.Probe{Path: f.o.Path, Version: "fake 1.0", Headless: true, Authenticated: f.o.Auth.State == backend.AuthIn}, nil
}

func (f *Fake) Auth(ctx context.Context) backend.Auth { return f.o.Auth }

func (f *Fake) Build(req backend.Request) (backend.Invocation, []backend.Degradation, error) {
	if !f.o.SkipCheckMust {
		if err := backend.CheckMust(f.self, req); err != nil {
			return backend.Invocation{}, nil, err
		}
	}
	args := []string{"--label", req.Label}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	} else if req.Session != "" {
		args = append(args, "--session", req.Session)
	}
	if f.o.Caps.Schema && req.Schema != "" {
		args = append(args, "--schema", req.Schema)
	}
	if f.o.Caps.MaxTurns && req.MaxTurns > 0 {
		args = append(args, "--turns", strconv.Itoa(req.MaxTurns))
	}
	if f.o.Caps.HardBudget && req.BudgetUSD > 0 {
		args = append(args, "--budget", strconv.FormatFloat(req.BudgetUSD, 'f', -1, 64))
	}
	if req.Model != "" {
		args = append(args, "--mdl", req.Model)
	}
	args = append(args, "--prompt", req.Prompt)
	rec := append([]string(nil), args...)
	rec[len(rec)-1] = "<见 prompt.txt>"
	return backend.Invocation{Path: f.o.Path, Args: args, Record: rec, Dir: req.Dir, EnvKeep: f.o.EnvKeep}, backend.Degrade(f.self, req), nil
}

func (f *Fake) events(dir string) []FakeEvent {
	file, err := os.Open(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []FakeEvent
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var e FakeEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

var fakeLimitRe = regexp.MustCompile(`(?i)usage limit`)

// Parse 撞限只扫错误通道（stderr），不扫模型正文。
func (f *Fake) Parse(job backend.JobRef, exit backend.Exit) backend.Result {
	var res backend.Result
	if f.o.Caps.LimitSignal {
		res.LimitText = fakeLimitRe.MatchString(fsx.TailLines(filepath.Join(job.Dir, "stderr.log"), 5))
	}
	for _, e := range f.events(job.Dir) {
		switch e.Type {
		case "session":
			res.SessionID = e.ID
		case "usage":
			if f.o.Caps.Usage {
				res.Usage = &backend.JobUsage{Input: e.Input, Output: e.Output, CostUSD: e.Cost, CostUnknown: !f.o.Caps.Cost}
			}
		case "result":
			res.HaveTerminal = true
			res.Failed = !e.OK
			res.Text, res.Structured, res.Turns, res.HTTPStatus = e.Text, e.Structured, e.Turns, e.Status
			if e.Auth {
				res.Stop = backend.StopAuth
			}
			if e.MaxBudget {
				res.Stop = backend.StopMaxBudget
			}
			if e.Status == 429 {
				res.LimitStructured = true
			}
		}
	}
	return res
}

// LastActivity 读 agentq 自己保存的事件流，不碰厂商的内部文件。
func (f *Fake) LastActivity(job backend.JobRef) (time.Time, bool) {
	if f.o.NoTimestamps {
		backend.NoteSeen(job.Dir, "result.json", time.Now())
		return backend.LastSeen(job.Dir, "result.json"), true
	}
	fi, err := os.Stat(filepath.Join(job.Dir, "result.json"))
	if err != nil {
		return time.Time{}, true
	}
	return fi.ModTime(), true
}

func (f *Fake) CanResume(session string) bool { return f.o.Caps.Resume && session != "" }

func (f *Fake) RecordedModel(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--mdl" {
			return args[i+1]
		}
	}
	return ""
}

type fakeTools struct{ f *Fake }

// ToolEvents 没有时间戳时按字节位置取首次看到的时刻。
func (t fakeTools) ToolEvents(job backend.JobRef, withSubs bool) ([]backend.ToolEvent, [][]backend.ToolEvent, error) {
	b, err := os.ReadFile(filepath.Join(job.Dir, "result.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("读事件流：%w", err)
	}
	var main []backend.ToolEvent
	var off int64
	for _, line := range strings.SplitAfter(string(b), "\n") {
		at := off
		off += int64(len(line))
		var e FakeEvent
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "tool" {
			continue
		}
		ev := backend.ToolEvent{Name: e.Name, Cmd: e.Cmd}
		if e.At != nil {
			ev.At = *e.At
		} else {
			seen, err := backend.SeenAt(job.Dir, "result.json", at)
			if err != nil {
				continue
			}
			ev.At = seen
		}
		main = append(main, ev)
	}
	return main, nil, nil
}

type fakeLive struct{ f *Fake }

func (l fakeLive) LiveUsage(job backend.JobRef) *backend.JobUsage {
	var u *backend.JobUsage
	for _, e := range l.f.events(job.Dir) {
		if e.Type == "usage" {
			u = &backend.JobUsage{Input: e.Input, Output: e.Output, CostUSD: e.Cost, CostUnknown: !l.f.o.Caps.Cost}
		}
	}
	return u
}

// WriteEvents 把事件写成假 CLI 的输出。
func WriteEvents(t *testing.T, dir string, events ...FakeEvent) {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
