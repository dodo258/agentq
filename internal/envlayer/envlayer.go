// Package envlayer 是环境层：把编译进二进制的通用工程规则放到后端读用户级规则的地方，判断该不该放、改过的留下。
// 后端差异走 backend.GlobalRules。自己的记录单独存一份，不进安装回执：规则是用户的选择，不随代码回滚，用户改过文件也不能挡住升级和卸载。
package envlayer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
)

// assetDir 是编译进二进制的规则目录，subdir 是装到后端规则目录下的子目录。
const (
	assetDir = "rules"
	subdir   = "agentq"
)

// State 是环境层现在的状态。
type State string

const (
	StateNone    State = ""        // 没装过
	StateOn      State = "on"      // 装着，升级时跟着更新
	StateOff     State = "off"     // agentq env off 关的，或用户把整个目录删了，升级不加回来
	StateRemoved State = "removed" // 卸载时清掉的，agentq restore 会装回来
)

// FileState 是一个规则文件和记录比的结果。
type FileState string

const (
	FileIntact   FileState = "intact"   // 和 agentq 写的一样
	FileModified FileState = "modified" // 用户改过，或这里本来就有别的内容
	FileMissing  FileState = "missing"  // 不在了，还没遇到升级
	FileDeclined FileState = "declined" // 用户删的，升级不写回
)

// FileStatus 是 Status 里的一行。
type FileStatus struct {
	Name  string
	State FileState
}

// Status 是 agentq env status 要说的全部。
type Status struct {
	Supported bool     // 这个后端有用户级规则目录
	State     State    // 记录里的状态
	Dir       string   // 规则装在哪；不支持时为空。块方式是那个说明文件
	UserRules []string // 用户自己的全局规则（agentq 之外的内容）
	Shadow    string   // 块方式：这个文件存在，后端不读放块的文件，装了也不生效
	Files     []FileStatus
}

// Report 是这次动了哪些文件，都是文件名。
type Report struct {
	Written, Updated, Removed []string
	Kept                      []string // 内容和 agentq 写的不一样，保留没动
	Declined                  []string // 这次升级发现用户删了，不写回
	AllRemoved                bool     // 这次升级发现整个目录都删了，当作不要了
	Dir                       string
	Shadow                    string // 块方式：这个文件存在，装了也不生效
}

// ErrUnsupported：这个后端没有规则目录。
var ErrUnsupported = errors.New("这个后端没有用户级规则目录")

// Layer 只有家目录、资产和后端，没有进程内状态，安装引擎和命令各建各的。
type Layer struct {
	home    string
	assets  fs.FS
	backend backend.Backend
}

// New 建一个 Layer。b 只用来问规则放哪；记录里的目录是装的时候定的，关闭和升级都认它。
func New(home string, assets fs.FS, b backend.Backend) *Layer {
	return &Layer{home: home, assets: assets, backend: b}
}

// record 是落盘的记录，文件名到 agentq 写下的内容的哈希。
type record struct {
	Version int               `json:"version"`
	State   State             `json:"state"`
	Dir     string            `json:"dir,omitempty"`
	Files   map[string]string `json:"files,omitempty"`
	// File、Sep、Created 是块方式（见 block.go）：放块的文件、加块时补的换行、文件是不是 agentq 建的。
	File    string `json:"file,omitempty"`
	Sep     string `json:"sep,omitempty"`
	Created bool   `json:"created,omitempty"`
	// Declined 是用户删掉的文件：记录里没有它们之后，靠这张表和「新版本才有的文件」分开，下次升级也不写回。agentq env on 清空。
	Declined []string `json:"declined,omitempty"`
}

// RecordPath 是记录文件，和安装回执在同一个目录。
func RecordPath(home string) string {
	return filepath.Join(home, ".local", "share", "agentq", "env-layer.json")
}

// loadRecord：没有文件是没装过。内容不对就报错：读不了当未知，什么文件都不动。
func (l *Layer) loadRecord() (record, error) {
	var r record
	b, err := os.ReadFile(RecordPath(l.home))
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("读环境层记录失败，规则文件没动：%w", err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return record{}, fmt.Errorf("环境层记录 %s 读不懂，规则文件没动：%w", RecordPath(l.home), err)
	}
	if r.Version != 1 {
		return record{}, fmt.Errorf("环境层记录 %s 版本不对，规则文件没动", RecordPath(l.home))
	}
	if rel, err := filepath.Rel(l.home, r.Dir); r.Dir != "" && (!filepath.IsAbs(r.Dir) || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return record{}, fmt.Errorf("环境层记录 %s 里的目录不对，规则文件没动", RecordPath(l.home))
	}
	if rel, err := filepath.Rel(l.home, r.File); r.File != "" && (!filepath.IsAbs(r.File) || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return record{}, fmt.Errorf("环境层记录 %s 里的文件不对，规则文件没动", RecordPath(l.home))
	}
	for name := range r.Files {
		if name != filepath.Base(name) || name == "." || name == ".." {
			return record{}, fmt.Errorf("环境层记录 %s 里的文件名不对，规则文件没动", RecordPath(l.home))
		}
	}
	return r, nil
}

func (l *Layer) saveRecord(r record) error {
	r.Version = 1
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("写环境层记录失败：%w", err)
	}
	if err := fsx.WriteFileAtomic(RecordPath(l.home), append(b, '\n')); err != nil {
		return fmt.Errorf("写环境层记录失败：%w", err)
	}
	return nil
}

// locked 串行地改记录和文件。安装引擎的锁在上层，这把锁和它不是一个文件，不会互相等。
func (l *Layer) locked(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(RecordPath(l.home)), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(RecordPath(l.home)+".lock", 10*time.Second)
	if err != nil {
		return err
	}
	if lock == nil {
		return errors.New("等了 10 秒还拿不到环境层的锁，另一个 agentq 正在改规则")
	}
	defer lock.Close()
	return fn()
}

// paths 是这个后端的规则目录和全局说明文件。
func (l *Layer) paths() (rulesDir string, instructions []string, ok bool) {
	g, ok := l.backend.(backend.GlobalRules)
	if !ok {
		return "", nil, false
	}
	rulesDir, instructions = g.RulesPaths(l.home)
	return rulesDir, instructions, true
}

// bundled 读编译进二进制的规则：文件名到内容。
func (l *Layer) bundled() (map[string][]byte, error) {
	entries, err := fs.ReadDir(l.assets, assetDir)
	if err != nil {
		return nil, fmt.Errorf("这个版本没带规则文件：%w", err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := fs.ReadFile(l.assets, assetDir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("读规则文件 %s 失败：%w", e.Name(), err)
		}
		out[e.Name()] = b
	}
	if len(out) == 0 {
		return nil, errors.New("这个版本没带规则文件")
	}
	return out, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// read 看文件现在的样子：absent 是不存在，regular 为假是软链接、目录这些 agentq 不碰的东西。
func read(path string) (data []byte, absent, regular bool, err error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, false, false, nil
	}
	data, err = os.ReadFile(path)
	return data, false, true, err
}

// Status 只读。记录读不懂时返回错误。
func (l *Layer) Status() (Status, error) {
	r, err := l.loadRecord()
	if err != nil {
		return Status{}, err
	}
	if file, shadow, block := l.rulesFile(); r.File != "" || (block && r.Dir == "") {
		if r.File != "" {
			file = r.File
		}
		return l.statusBlock(r, file, shadow)
	}
	s := Status{State: r.State, Dir: r.Dir}
	rulesDir, instructions, ok := l.paths()
	s.Supported = ok
	if ok {
		if s.Dir == "" {
			s.Dir = filepath.Join(rulesDir, subdir)
		}
		s.UserRules = userRules(rulesDir, instructions)
	}
	if s.Dir == "" {
		return s, nil
	}
	want, err := l.bundled()
	if err != nil {
		return s, err
	}
	names := make([]string, 0, len(want)+len(r.Files))
	for n := range want {
		names = append(names, n)
	}
	for n := range r.Files {
		if _, dup := want[n]; !dup {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		data, absent, regular, err := read(filepath.Join(s.Dir, n))
		switch {
		case err != nil:
			return s, fmt.Errorf("读 %s 失败：%w", n, err)
		case absent:
			if _, recorded := r.Files[n]; recorded {
				s.Files = append(s.Files, FileStatus{n, FileMissing})
			} else if slices.Contains(r.Declined, n) {
				s.Files = append(s.Files, FileStatus{n, FileDeclined})
			}
		case regular && (digest(data) == r.Files[n] || digest(data) == digest(want[n])):
			s.Files = append(s.Files, FileStatus{n, FileIntact})
		default:
			s.Files = append(s.Files, FileStatus{n, FileModified})
		}
	}
	return s, nil
}

// userRules 找用户自己的全局规则：非空的全局说明文件，规则目录里 agentq 之外的 .md。
// 读不了的算有：宁可不装，也不覆盖判断不了的东西。
func userRules(rulesDir string, instructions []string) []string {
	var found []string
	for _, p := range instructions {
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || len(strings.TrimSpace(string(b))) > 0 {
			found = append(found, p)
		}
	}
	fi, err := os.Lstat(rulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return found
	}
	if err != nil {
		return append(found, rulesDir)
	}
	if fi.Mode()&os.ModeSymlink != 0 { // 官方支持软链接：指向的东西都算用户的，断了的不算
		if _, err := os.Stat(rulesDir); err == nil {
			found = append(found, rulesDir)
		}
		return found
	}
	if !fi.IsDir() {
		return found
	}
	own := filepath.Join(rulesDir, subdir)
	err = filepath.WalkDir(rulesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			found = append(found, p)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == own {
			return filepath.SkipDir
		}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			if _, err := os.Stat(p); err == nil {
				found = append(found, p)
			}
		case !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".md"):
			b, err := os.ReadFile(p)
			if err != nil || len(strings.TrimSpace(string(b))) > 0 {
				found = append(found, p)
			}
		}
		return nil
	})
	if err != nil {
		found = append(found, rulesDir)
	}
	return found
}

// ShouldAutoEnable：第一次同意时要不要自动装。用户有自己的规则就不装，关过的也不装。
func (s Status) ShouldAutoEnable() bool {
	return s.Supported && len(s.UserRules) == 0 && (s.State == StateNone || s.State == StateRemoved)
}

// Enable 是 agentq env on，也是第一次同意时自动装：用户明确要，已有规则也装。改过的文件留着。
func (l *Layer) Enable() (Report, error) {
	if file, shadow, ok := l.rulesFile(); ok {
		return l.enableBlock(file, shadow)
	}
	rulesDir, _, ok := l.paths()
	if !ok {
		return Report{}, fmt.Errorf("%s：%w", l.backend.Name(), ErrUnsupported)
	}
	var rep Report
	err := l.locked(func() error {
		r, err := l.loadRecord()
		if err != nil {
			return err
		}
		if r.File != "" && len(r.Files) > 0 {
			return fmt.Errorf("规则已经装在 %s（别的后端），先 agentq env off", r.File)
		}
		r.File = ""
		r.Dir = filepath.Join(rulesDir, subdir)
		r.Declined = nil
		rep, err = l.apply(r, false)
		return err
	})
	return rep, err
}

func (l *Layer) enableBlock(file, shadow string) (Report, error) {
	var rep Report
	err := l.locked(func() error {
		r, err := l.loadRecord()
		if err != nil {
			return err
		}
		if r.Dir != "" && len(r.Files) > 0 {
			return fmt.Errorf("规则已经装在 %s（别的后端），先 agentq env off", r.Dir)
		}
		r.Dir, r.File, r.Declined = "", file, nil
		rep, err = l.applyBlock(r, false)
		return err
	})
	if rep.Dir == "" {
		return rep, err
	}
	if fi, statErr := os.Stat(shadow); shadow != "" && statErr == nil && !fi.IsDir() {
		rep.Shadow = shadow
	}
	return rep, err
}

// Refresh 是升级时调用：装着的跟着新版本走，没装的不加。用户删掉的不写回：删了一个，这个文件以后都不写；
// 记录里的一个都不剩（包括整个目录删了），当作不要了，转成关着，新版本才有的文件也不写。要加回来用 agentq env on。
func (l *Layer) Refresh() (Report, error) {
	var rep Report
	err := l.locked(func() error {
		r, err := l.loadRecord()
		if err != nil || r.State != StateOn {
			return err
		}
		if r.File != "" {
			if _, absent, _, err := read(r.File); err == nil && absent && len(r.Files) > 0 { // 文件都没了，和目录删了一样：当作不要了
				rep = Report{Dir: r.File, AllRemoved: true}
				r.State, r.Files, r.Declined = StateOff, nil, nil
				return l.saveRecord(r)
			}
			rep, err = l.applyBlock(r, true)
			return err
		}
		if allGone(r) {
			rep = Report{Dir: r.Dir, AllRemoved: true}
			r.State, r.Files, r.Declined = StateOff, nil, nil
			return l.saveRecord(r)
		}
		rep, err = l.apply(r, true)
		return err
	})
	return rep, err
}

// allGone：记录里有文件，磁盘上一个都不在了。
func allGone(r record) bool {
	if len(r.Files) == 0 {
		return false
	}
	for n := range r.Files {
		if _, absent, _, err := read(filepath.Join(r.Dir, n)); !absent || err != nil { // 读不了的算还在，不知道就不动
			return false
		}
	}
	return true
}

// Restore 是 agentq restore 恢复卸载时调用：卸载清掉的装回来，别的状态不动。
func (l *Layer) Restore() (Report, error) {
	var rep Report
	err := l.locked(func() error {
		r, err := l.loadRecord()
		if err != nil || r.State != StateRemoved || (r.Dir == "" && r.File == "") {
			return err
		}
		if r.File != "" {
			rep, err = l.applyBlock(r, true)
			return err
		}
		rep, err = l.apply(r, true)
		return err
	})
	return rep, err
}

// apply 让目录和这个版本的规则一致。只动 agentq 写过且没被改过的文件；记录随写随存，中途失败时已写的也算数。
// keepDeleted：记录里有、磁盘上没有的算用户删的，不写回（升级、恢复）。为假是用户明确要补全（agentq env on）。
func (l *Layer) apply(r record, keepDeleted bool) (Report, error) {
	rep := Report{Dir: r.Dir}
	want, err := l.bundled()
	if err != nil {
		return rep, err
	}
	if err := fsx.CheckParents(l.home, filepath.Join(r.Dir, "x")); err != nil {
		return rep, fmt.Errorf("没装规则：%w", err)
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return rep, fmt.Errorf("建规则目录失败：%w", err)
	}
	if r.Files == nil {
		r.Files = map[string]string{}
	}
	r.State = StateOn
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		path, sum := filepath.Join(r.Dir, n), digest(want[n])
		data, absent, regular, err := read(path)
		if err != nil {
			return rep, l.saveAfter(r, fmt.Errorf("读 %s 失败：%w", path, err))
		}
		_, recorded := r.Files[n]
		if !absent {
			r.Declined = slices.DeleteFunc(r.Declined, func(d string) bool { return d == n }) // 用户又放回来了
		}
		switch {
		case absent && slices.Contains(r.Declined, n):
		case absent && keepDeleted && recorded:
			delete(r.Files, n)
			r.Declined = append(r.Declined, n)
			rep.Declined = append(rep.Declined, n)
		case absent:
			if err := fsx.WriteFileAtomic(path, want[n]); err != nil {
				return rep, l.saveAfter(r, fmt.Errorf("写 %s 失败：%w", path, err))
			}
			r.Files[n] = sum
			rep.Written = append(rep.Written, n)
		case regular && digest(data) == sum:
			r.Files[n] = sum
		case regular && digest(data) == r.Files[n]:
			if err := fsx.WriteFileAtomic(path, want[n]); err != nil {
				return rep, l.saveAfter(r, fmt.Errorf("写 %s 失败：%w", path, err))
			}
			r.Files[n] = sum
			rep.Updated = append(rep.Updated, n)
		default:
			rep.Kept = append(rep.Kept, n)
		}
	}
	// 新版本不带了的文件：没改过就删，改过的留着，记录也留着，关闭时还能报告。
	for _, n := range sortedKeys(r.Files) {
		if _, still := want[n]; still {
			continue
		}
		if _, absent, _, _ := read(filepath.Join(r.Dir, n)); absent {
			delete(r.Files, n)
			continue
		}
		removed, err := removeIfSame(filepath.Join(r.Dir, n), r.Files[n])
		if err != nil {
			return rep, l.saveAfter(r, err)
		}
		if removed {
			delete(r.Files, n)
			rep.Removed = append(rep.Removed, n)
		} else {
			rep.Kept = append(rep.Kept, n)
		}
	}
	return rep, l.saveRecord(r)
}

// saveAfter：失败了也把已写的记下来，让之后的关闭和卸载认得它们。两个都失败时报原来的错。
func (l *Layer) saveAfter(r record, cause error) error {
	if err := l.saveRecord(r); err != nil {
		return fmt.Errorf("%w；%v", cause, err)
	}
	return cause
}

// Disable 是 agentq env off：只删哈希对得上的文件，改过的留着并报告。之后升级不再加回来。
func (l *Layer) Disable() (Report, error) { return l.clear(StateOff) }

// Uninstall 是卸载时调用：和 Disable 一样只删没改过的，记下是卸载清的，agentq restore 才知道要装回来。
func (l *Layer) Uninstall() (Report, error) {
	var rep Report
	err := l.locked(func() error {
		r, err := l.loadRecord()
		if err != nil || r.State != StateOn {
			return err
		}
		rep, err = l.clearLocked(r, StateRemoved)
		return err
	})
	return rep, err
}

func (l *Layer) clear(to State) (Report, error) {
	var rep Report
	err := l.locked(func() error {
		r, err := l.loadRecord()
		if err != nil {
			return err
		}
		if r.Dir == "" && r.File == "" {
			if file, _, ok := l.rulesFile(); ok {
				r.File = file
			} else if rulesDir, _, ok := l.paths(); ok {
				r.Dir = filepath.Join(rulesDir, subdir)
			}
		}
		rep, err = l.clearLocked(r, to)
		return err
	})
	return rep, err
}

func (l *Layer) clearLocked(r record, to State) (Report, error) {
	if r.File != "" {
		return l.clearBlock(r, to)
	}
	rep := Report{Dir: r.Dir}
	for _, n := range sortedKeys(r.Files) {
		removed, err := removeIfSame(filepath.Join(r.Dir, n), r.Files[n])
		if err != nil {
			return rep, l.saveAfter(r, err)
		}
		if removed {
			rep.Removed = append(rep.Removed, n)
		} else if _, absent, _, _ := read(filepath.Join(r.Dir, n)); !absent {
			rep.Kept = append(rep.Kept, n)
		}
		delete(r.Files, n)
	}
	if r.Dir != "" {
		_ = os.Remove(r.Dir) // 只有空目录才删得掉；里面还有东西就留着，不算错
	}
	r.State = to
	return rep, l.saveRecord(r)
}

// removeIfSame 只删内容哈希和记录一致的普通文件。文件不在算没删、不算错。
func removeIfSame(path, want string) (bool, error) {
	data, absent, regular, err := read(path)
	if err != nil {
		return false, fmt.Errorf("读 %s 失败：%w", path, err)
	}
	if absent || !regular || digest(data) != want {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("删 %s 失败：%w", path, err)
	}
	return true, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
