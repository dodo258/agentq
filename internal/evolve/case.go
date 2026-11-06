// Package evolve 是自己进化的基准：评测题、判分、和基线比。
//
// 题目编译进二进制，评判用运行中这一代的原件；任务跑的是被评判的那个二进制，在临时 HOME 和临时项目里。
// 判分不问模型，看默认分支的 git 历史和文件、门留下的文件。任务有本机权限，这防偷懒和误操作，防不住存心把本机的证据造齐。
// tick 不会调用这里的任何东西。
package evolve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
)

// 题目的种类。
const (
	KindTask    = "task"    // 项目里一条待做，引擎交给模型做到合并
	KindNew     = "new"     // agentq new：一句话从零建项目
	KindProgram = "program" // 不调模型：check.sh 在临时 HOME 布置现场，退出码判分
)

// 题目目录里约定的文件。
const (
	caseFile    = "case.json"
	checkFile   = "check.sh" // 判分脚本：任务跑完才放到磁盘，任务分支上没有它
	projectDir  = "_project" // 初始项目，下划线开头 go 工具不当包；go.mod 写成 go.mod.tmpl，否则 go:embed 收不进来
	fakeFile    = "fake.sh"  // 假模型的做法，只有 --fake 用
	fakeHomeDir = "fake-home"
	tmplSuffix  = ".tmpl"
	verifyPath  = ".agentq/verify"
	defaultWait = 20 * time.Minute
)

var caseDirRe = regexp.MustCompile(`^[0-9]+-[a-z0-9][a-z0-9-]*$`)

// Expect 是一道题期望的结果，没写的不判。至少写一项。
type Expect struct {
	Merged       *bool    `json:"merged,omitempty"`        // 进没进 main
	Item         []string `json:"item,omitempty"`          // 条目最后的状态，任一：todo、doing、stuck、decision、done、gone
	MainHas      []string `json:"main_has,omitempty"`      // main 上要有的文件
	MainLacks    []string `json:"main_lacks,omitempty"`    // main 上不能有的文件
	Unchanged    []string `json:"unchanged,omitempty"`     // main 上要和题目原件逐字节一样的文件
	VerifyExit   *int     `json:"verify_exit,omitempty"`   // 题目原件的 .agentq/verify 在 main 上的退出码
	GatesRan     []string `json:"gates_ran,omitempty"`     // 真的跑过的门
	GatesBlocked []string `json:"gates_blocked,omitempty"` // 拦下过的门
	Evidence     []string `json:"evidence,omitempty"`      // 门的报告里要出现的原文
	Shots        *bool    `json:"shots,omitempty"`         // 界面门留下了非空截图
	Delivery     *bool    `json:"delivery,omitempty"`      // 有交付单
	MaxRounds    *int     `json:"max_rounds,omitempty"`    // 最多退回几轮
	Human        *bool    `json:"human,omitempty"`         // 最后要不要人定
}

func (e Expect) empty() bool {
	return e.Merged == nil && len(e.Item) == 0 && len(e.MainHas) == 0 && len(e.MainLacks) == 0 && len(e.Unchanged) == 0 && e.VerifyExit == nil &&
		len(e.GatesRan) == 0 && len(e.GatesBlocked) == 0 && len(e.Evidence) == 0 && e.Shots == nil && e.Delivery == nil && e.MaxRounds == nil && e.Human == nil
}

// Case 是一道题。
type Case struct {
	ID       string   `json:"-"`
	Held     bool     `json:"-"` // 留出题：不进仓库，list 只显示数量
	Domain   string   `json:"domain"`
	Kind     string   `json:"kind"`
	Request  string   `json:"request"`
	Expect   Expect   `json:"expect"`
	Requires []string `json:"requires,omitempty"` // 要 PATH 里有的命令，没有就跳过这题
	// TimeoutMinutes：这题最长跑多久，0 用默认。
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`

	fsys fs.FS // 这道题目录的原件
}

// Short 是题号后面的短名。
func (c Case) Short() string {
	_, s, _ := strings.Cut(c.ID, "-")
	return s
}

// HasFake：这题能用 --fake 跑。
func (c Case) HasFake() bool { return exists(c.fsys, fakeFile) }

func (c Case) timeout() time.Duration {
	if c.TimeoutMinutes > 0 {
		return time.Duration(c.TimeoutMinutes) * time.Minute
	}
	return defaultWait
}

func exists(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}

// LoadCases 读 fsys 根目录下的每道题。下划线、点开头的目录不是题。一道题写坏了就报错：判不了分比少一道题糟。
func LoadCases(fsys fs.FS, held bool) ([]Case, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("读题目目录：%w", err)
	}
	var out []Case
	for _, d := range entries {
		name := d.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || !d.IsDir() {
			continue
		}
		if !caseDirRe.MatchString(name) {
			return nil, fmt.Errorf("题目目录 %s：名字要是「题号-短名」，小写字母、数字和连字符", name)
		}
		sub, err := fs.Sub(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("题目 %s：%w", name, err)
		}
		c, err := loadCase(sub, name)
		if err != nil {
			return nil, fmt.Errorf("题目 %s：%w", name, err)
		}
		c.Held = held
		out = append(out, c)
	}
	return out, nil
}

func loadCase(fsys fs.FS, id string) (Case, error) {
	raw, err := fs.ReadFile(fsys, caseFile)
	if err != nil {
		return Case{}, fmt.Errorf("读 %s：%w", caseFile, err)
	}
	c := Case{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // 期望拼错了会变成「不判」，等于白送分
	if err := dec.Decode(&c); err != nil {
		return Case{}, fmt.Errorf("解析 %s：%w", caseFile, err)
	}
	c.ID, c.fsys = id, fsys
	if c.Domain == "" {
		return Case{}, errors.New("没写 domain")
	}
	switch c.Kind {
	case KindProgram:
		if !exists(fsys, checkFile) {
			return Case{}, errors.New("程序题要有 " + checkFile)
		}
	case KindTask:
		if !exists(fsys, projectDir+"/"+verifyPath) {
			return Case{}, errors.New("模型题要有 " + projectDir + "/" + verifyPath)
		}
		fallthrough
	case KindNew:
		if strings.TrimSpace(c.Request) == "" {
			return Case{}, errors.New("没写 request")
		}
		if strings.ContainsAny(c.Request, "\r\n") {
			return Case{}, errors.New("request 只能一行：它就是待做里的一条")
		}
		if c.Expect.empty() && !exists(fsys, checkFile) {
			return Case{}, errors.New("没写 expect，也没有 " + checkFile + "：什么结果都算对")
		}
	default:
		return Case{}, fmt.Errorf("kind %q 不认识，只有 task、new、program", c.Kind)
	}
	return c, nil
}

// Select 按题号、短名或全名挑题。没给就是全部。认不出的报错，不悄悄少跑。
func Select(cases []Case, names []string) ([]Case, error) {
	if len(names) == 0 {
		return cases, nil
	}
	var out []Case
	seen := map[string]bool{}
	for _, n := range names {
		var found []Case
		for _, c := range cases {
			num, _, _ := strings.Cut(c.ID, "-")
			if n == c.ID || n == num || n == c.Short() {
				found = append(found, c)
			}
		}
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("没有这道题：%s（agentq eval list 看有哪些）", n)
		case 1:
		default:
			return nil, fmt.Errorf("%s 对上了 %d 道题，写全名", n, len(found))
		}
		if !seen[found[0].ID] {
			seen[found[0].ID] = true
			out = append(out, found[0])
		}
	}
	return out, nil
}
