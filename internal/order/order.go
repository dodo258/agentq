// Package order 是一条需求的状态，存成一个 JSON。
package order

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"agentq/internal/lock"
)

// 一条需求走到哪一步。
const (
	Planning = "产品经理"
	Building = "在做"
	Testing  = "跑测试"
	Auditing = "审计"
	Fixing   = "修"
	Landing  = "合并"
	Done     = "做完"
	Stuck    = "卡住"
	Stopped  = "停了"
)

// Module 是方案里划的一块。
type Module struct {
	Name   string   `json:"name"`
	Goal   string   `json:"goal"`
	Paths  []string `json:"paths"`
	Needs  []string `json:"needs"`
	Accept string   `json:"accept"`
	// Hard：难的、牵动全局的，用最强的写。
	Hard bool `json:"hard"`
}

// Plan 是产品经理的方案。Small 的不调研、不审计。
type Plan struct {
	Small   bool     `json:"small"`
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Spec    string   `json:"spec"`
	Modules []Module `json:"modules"`
	Stop    string   `json:"stop"`
	// Later 是大项目后面几期，写进待办。
	Later []string `json:"later"`
	// Filed：方案和后面几期已经写进项目。
	Filed bool `json:"filed,omitempty"`
}

// Task 是一个模块做到哪了。
type Task struct {
	Session string `json:"session,omitempty"`
	// From 是开工时的提交。
	From   string `json:"from,omitempty"`
	Merged bool   `json:"merged,omitempty"`
}

// Bug 是审计找到的一条。
type Bug struct {
	Module string `json:"module"`
	Where  string `json:"where"`
	What   string `json:"what"`
	How    string `json:"how"`
}

// Use 是一次会话用了多少。
type Use struct {
	At     time.Time `json:"at"`
	Tokens int64     `json:"tokens"`
}

// Order 是一条需求。
type Order struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Dir     string `json:"dir"`
	Text    string `json:"text"`
	// Todo 是它出自哪条待办（标题）。Chain：做完接着做待办里能做的。
	Todo  string `json:"todo,omitempty"`
	Chain bool   `json:"chain,omitempty"`
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
	// Self 是这条需求里看到的 agentq 自己的毛病（不是项目的），早报报、agentq self 修。
	Self  []string         `json:"self,omitempty"`
	PID   int              `json:"pid,omitempty"`
	Main  string           `json:"main"`
	Base  string           `json:"base,omitempty"`
	Plan  *Plan            `json:"plan,omitempty"`
	Tasks map[string]*Task `json:"tasks,omitempty"`
	Bugs  []Bug            `json:"bugs,omitempty"`
	// Risks 是审计提到、这次不改的风险。
	Risks []string `json:"risks,omitempty"`
	// Invalid 是核对后不成立的条数。
	Invalid int       `json:"invalid,omitempty"`
	Audited bool      `json:"audited,omitempty"`
	Fixed   bool      `json:"fixed,omitempty"`
	Commit  string    `json:"commit,omitempty"`
	How     string    `json:"how,omitempty"`
	Tokens  int64     `json:"tokens,omitempty"`
	Used    []Use     `json:"used,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`

	store *Store
	mu    sync.Mutex
}

// Store 是存需求的目录。
type Store struct{ Root string }

var idRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{6}(-[0-9]+)?$`)

func (s *Store) Dir(id string) string { return filepath.Join(s.Root, "orders", id) }

// New 记一条新需求。
func (s *Store) New(dir, main, text string) (*Order, error) {
	now := time.Now()
	for n := 1; ; n++ {
		id := now.Format("0102-150405")
		if n > 1 {
			id = fmt.Sprintf("%s-%d", id, n)
		}
		if err := os.MkdirAll(filepath.Join(s.Root, "orders"), 0o700); err != nil {
			return nil, err
		}
		if err := os.Mkdir(s.Dir(id), 0o700); os.IsExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		o := &Order{ID: id, Project: filepath.Base(dir), Dir: dir, Text: text, State: Planning, Main: main, Started: now, Tasks: map[string]*Task{}, store: s}
		return o, o.Update(func() {})
	}
}

// Load 按编号读。
func (s *Store) Load(id string) (*Order, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("没有这条需求：%s（agentq ls 看编号）", id)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(id), "order.json"))
	if err != nil {
		return nil, fmt.Errorf("读需求 %s：%w", id, err)
	}
	o := &Order{store: s, Tasks: map[string]*Task{}}
	if err := json.Unmarshal(b, o); err != nil {
		return nil, fmt.Errorf("需求 %s 的记录读不懂：%w", id, err)
	}
	return o, nil
}

// All 按时间列出所有需求，读不懂的跳过。
func (s *Store) All() []*Order {
	ents, _ := os.ReadDir(filepath.Join(s.Root, "orders"))
	var out []*Order
	for _, e := range ents {
		if o, err := s.Load(e.Name()); err == nil {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Update 在锁里改并存盘：几个模块同时做，改的是同一份。
func (o *Order) Update(f func()) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	f()
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(o.store.Dir(o.ID), "order.json")
	if err := os.WriteFile(p+".tmp", append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// WorkDir 放这条需求的日志和会话输出。
func (o *Order) WorkDir() string { return o.store.Dir(o.ID) }

// Over：不用再动了。
func (o *Order) Over() bool { return o.State == Done || o.State == Stuck || o.State == Stopped }

// Lock 拿这条需求的运行锁，同一时间只有一个进程在做它。拿不到返回 nil。进程没了锁自己就没了。
func (o *Order) Lock() (*os.File, error) {
	return lock.Flock(filepath.Join(o.WorkDir(), "lock"), false)
}

// Running：有进程拿着运行锁。
func (o *Order) Running() bool {
	f, err := o.Lock()
	if f != nil {
		f.Close()
	}
	return err == nil && f == nil
}
