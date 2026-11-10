// Package setup 是安装引擎：候选自检、事务写文件、回滚、卸载、恢复、doctor 和维护命令。编译进二进制的资产由 main 传入。
package setup

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/service"
	"agentq/internal/state"
)

const PermissionDefaultHint = "permission_mode=default：任务写不了文件。运行 agentq start 放开（会先讲清风险，由你确认）。"

// NeedsConsent：默认后端的权限还没放开。Claude 没写这个键的是老配置，默认就是完全权限；别的后端没写就是没放开。
func NeedsConsent(cfg config.Config) bool {
	if cfg.PermissionMode == "default" {
		return true
	}
	return cfg.PermissionMode == "" && cfg.Backend != "" && cfg.Backend != backend.Default
}

type InstallFile struct {
	Data []byte
	Mode fs.FileMode
}

type FileImage struct {
	Kind string      `json:"kind"`
	Data []byte      `json:"data,omitempty"`
	Mode fs.FileMode `json:"mode,omitempty"`
	Link string      `json:"link,omitempty"`
}

type InstallReceipt struct {
	Extras     bool                 `json:"extras,omitempty"`
	Version    int                  `json:"version"`
	Generation string               `json:"generation"`
	Scope      string               `json:"scope"`
	Owned      map[string]FileImage `json:"owned"`
	Digests    map[string]string    `json:"digests"`
	External   map[string]string    `json:"external_skills,omitempty"`
	// KeptSkills 是用户自己的同名技能，内容和我们带的不一样：不替换也不接管，随用户更新，只查还在不在。
	KeptSkills []string `json:"kept_skills,omitempty"`
	// SkillDirs 是技能投影到的目录。空表示只有 ~/.claude/skills（老回执和只用 Claude 的安装）。
	SkillDirs []string `json:"skill_dirs,omitempty"`
	Services  []string `json:"services,omitempty"`
	// ConfigCreated：config.json 是安装时新建的，里面关着的开关是默认值，不是用户选的。
	ConfigCreated bool `json:"config_created,omitempty"`
}

type InstallChange struct {
	Path   string    `json:"path"`
	Before FileImage `json:"before"`
	After  FileImage `json:"after"`
}

type InstallJournal struct {
	Changes     []InstallChange `json:"changes"`
	Completed   bool            `json:"completed"`
	Applied     int             `json:"applied"`
	OldServices []string        `json:"old_services,omitempty"`
	NewServices []string        `json:"new_services,omitempty"`
}

type SetupOptions struct {
	Scope, Claude string
	// Backend 是新安装要用的后端，写进新建的配置。已有配置不改：和配置不一致就报错。
	Backend                                               string
	Yes, Plan, Services, Backup, Watchdog, Extras, Listen bool
	// DeferEngine：一键脚本用。CLI 没装或没登录也先装 agentq，这件事交给 start --welcome。--claude 指定的路径、配置里自定义的路径找不到时仍然报错。
	DeferEngine bool
}

type Installer struct {
	Assets     fs.FS // 编译进二进制的 skills、templates、support，由 main 传入
	Home       string
	Out        io.Writer
	Notes      io.Writer // 一键脚本把计划和探测细节收起来时（--quiet），警告和提醒走这里；nil 就走 Out
	Fetch      func(bool) (map[string]InstallFile, error)
	Check      func(string) error
	Launchctl  func(...string) ([]byte, error)
	Systemctl  func(...string) ([]byte, error)
	Loginctl   func(...string) ([]byte, error)
	AfterWrite func(int) error // 事务故障注入，仅测试使用
	TickWait   time.Duration   // 等后台 tick 放开 tick.lock 的上限，零值用 defaultTickWait
}

// defaultTickWait：tick 定时在跑，升级撞上它是常事，等它跑完就好。一次 tick 最长的是闲时判断（45 秒），留些余量。
const defaultTickWait = 90 * time.Second

var errInstallBusy = errors.New("another installation is running")

// notef 写用户该看到的提醒：--quiet 时也要留下。
func (i *Installer) notef(format string, args ...any) {
	w := i.Notes
	if w == nil {
		w = i.Out
	}
	fmt.Fprintf(w, format, args...)
}

func (i *Installer) Share() string { return filepath.Join(i.Home, ".local", "share", "agentq") }

func (i *Installer) ReceiptPath() string { return filepath.Join(i.Share(), "receipt.json") }

func (i *Installer) PendingPath() string { return filepath.Join(i.Share(), "pending.json") }

func Image(path string) (FileImage, error) {
	st, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return FileImage{Kind: "absent"}, nil
	}
	if e != nil {
		return FileImage{}, e
	}
	if st.Mode()&os.ModeSymlink != 0 {
		s, e := os.Readlink(path)
		return FileImage{Kind: "link", Link: s}, e
	}
	if !st.Mode().IsRegular() {
		return FileImage{}, fmt.Errorf("conflict: %s is not a regular file or managed link", path)
	}
	b, e := os.ReadFile(path)
	return FileImage{Kind: "file", Data: b, Mode: st.Mode().Perm()}, e
}

func SameImage(a, b FileImage) bool {
	return a.Kind == b.Kind && a.Link == b.Link && a.Mode == b.Mode && string(a.Data) == string(b.Data)
}

func PutImage(path string, v FileImage) error {
	if v.Kind == "absent" {
		e := os.Remove(path)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".agentq-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if v.Kind == "link" {
		f.Close()
		os.Remove(tmp)
		if e = os.Symlink(v.Link, tmp); e != nil {
			return e
		}
	} else if v.Kind == "file" {
		if _, e = f.Write(v.Data); e != nil {
			f.Close()
			return e
		}
		if e = f.Sync(); e != nil {
			f.Close()
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
		if e = os.Chmod(tmp, v.Mode); e != nil {
			return e
		}
	} else {
		f.Close()
		return fmt.Errorf("unknown image kind %q", v.Kind)
	}
	return os.Rename(tmp, path)
}

func SaveInstallJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'))
}

func (i *Installer) Receipt() (InstallReceipt, error) {
	var r InstallReceipt
	b, e := os.ReadFile(i.ReceiptPath())
	if errors.Is(e, os.ErrNotExist) {
		return r, nil
	}
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	if e == nil {
		e = i.validateReceipt(r)
	}
	return r, e
}

func (i *Installer) validateReceipt(r InstallReceipt) error {
	parent := filepath.Join(i.Share(), "generations")
	if r.Version != 1 || (r.Scope != "full" && r.Scope != "skills") || len(r.Owned) == 0 || len(r.Digests) == 0 || filepath.Dir(r.Generation) != parent {
		return errors.New("invalid install receipt")
	}
	if digest, e := hex.DecodeString(r.Digests[r.Generation]); e != nil || len(digest) != sha256.Size {
		return errors.New("invalid generation digest in install receipt")
	}
	current := r.Owned[filepath.Join(i.Share(), "current")]
	if current.Kind != "link" || current.Link != r.Generation {
		return errors.New("invalid generation entry in install receipt")
	}
	required := map[string]string{}
	for _, name := range []string{"templates", "support", "licenses", "dependencies"} {
		required[filepath.Join(i.Share(), name)] = filepath.Join(r.Generation, name)
	}
	if r.Scope == "full" {
		required[filepath.Join(i.Home, ".local", "bin", "agentq")] = filepath.Join(r.Generation, "bin", "agentq")
	}
	skills, e := os.ReadDir(filepath.Join(r.Generation, "skills"))
	if e != nil || len(skills) == 0 {
		return errors.New("invalid skill generation in install receipt")
	}
	for _, dir := range i.receiptSkillDirs(r) {
		for _, skill := range skills {
			p := filepath.Join(dir, skill.Name())
			if digest, external := r.External[p]; external {
				if b, e := hex.DecodeString(digest); e != nil || len(b) != sha256.Size {
					return errors.New("invalid external skill digest in install receipt")
				}
			} else if !slices.Contains(r.KeptSkills, p) {
				required[p] = filepath.Join(r.Generation, "skills", skill.Name())
			}
		}
	}
	for p, target := range required {
		v := r.Owned[p]
		if v.Kind != "link" || v.Link != target {
			return fmt.Errorf("incomplete install receipt: %s", p)
		}
	}
	for p, v := range r.Owned {
		if e := checkInstallParents(i.Home, p); e != nil {
			return e
		}
		if v.Kind != "file" && v.Kind != "link" {
			return errors.New("invalid owned entry in install receipt")
		}
	}
	return nil
}

// claudeSkillDir 是没有回执记录、后端也没声明时的技能目录。
func (i *Installer) claudeSkillDir() string { return filepath.Join(i.Home, ".claude", "skills") }

func (i *Installer) receiptSkillDirs(r InstallReceipt) []string {
	if len(r.SkillDirs) > 0 {
		return r.SkillDirs
	}
	return []string{i.claudeSkillDir()}
}

// skillDirs 是配置用到的后端要的技能目录（backend.SkillHome），去重排好序。没声明的后端不投影；一个都没有时是 Claude 的，和以前一样。
func (i *Installer) skillDirs(cfg config.Config) []string {
	var dirs []string
	for _, name := range cfg.BackendsInUse() {
		if h, ok := backend.Get(name, backend.Options{Home: i.Home}).(backend.SkillHome); ok {
			if d := h.SkillsDir(i.Home); !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}
	if len(dirs) == 0 {
		return []string{i.claudeSkillDir()}
	}
	slices.Sort(dirs)
	return dirs
}

// installSkillDirs 是这次要装技能的全部目录：配置用到的后端要的（任务靠它们），加上本机用着的前台工具的（用户在 Claude Code、Codex 里说需求，派活技能才在）。
func (i *Installer) installSkillDirs(cfg config.Config) []string {
	dirs := i.skillDirs(cfg)
	for _, d := range i.frontSkillDirs() {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// frontSkillDirs 是本机用着的前台工具读用户级技能的目录。用着 = 它的用户级配置目录在（~/.claude、$CODEX_HOME 这类），
// 从后端声明的规则位置推出来；没声明规则位置的后端不算。没用过的工具不给它建目录。
func (i *Installer) frontSkillDirs() []string {
	var dirs []string
	for _, name := range backend.Names() {
		b := backend.Get(name, backend.Options{Home: i.Home})
		h, ok := b.(backend.SkillHome)
		if !ok {
			continue
		}
		var toolHome string
		switch r := b.(type) {
		case backend.GlobalRules:
			dir, _ := r.RulesPaths(i.Home)
			toolHome = filepath.Dir(dir)
		case backend.RulesBlock:
			file, _ := r.RulesFile(i.Home)
			toolHome = filepath.Dir(file)
		default:
			continue
		}
		if st, e := os.Stat(toolHome); e != nil || !st.IsDir() {
			continue
		}
		if d := h.SkillsDir(i.Home); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// usesClaude：配置用到了 Claude 后端。~/.claude 下的检查只对它有意义。
func usesClaude(cfg config.Config) bool {
	return slices.Contains(cfg.BackendsInUse(), backend.Default)
}

// contractSkills 是任务合同靠得住的技能：引擎按它们的约定收尾，用户的同名版本顶替不了。别的技能（审查用的 check 这类）用用户自己的就行。
var contractSkills = []string{"agent-task", "dispatch", "new-project", "nightly-review"}

func (i *Installer) checkExternal(r InstallReceipt) error {
	for _, p := range r.KeptSkills {
		if _, e := os.Stat(p); e != nil {
			return fmt.Errorf("your skill %s is gone; run setup again to install agentq's copy", p)
		}
	}
	for p, want := range r.External {
		got, e := treeDigest(p)
		if e != nil || got != want {
			return fmt.Errorf("external required skill missing or changed: %s; preserved, not owned by agentq", p)
		}
	}
	return nil
}

func treeDigest(path string) (string, error) {
	h := sha256.New()
	e := filepath.WalkDir(path, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("unexpected link or special file: %s", p)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(path, p)
		fmt.Fprintf(h, "%s\x00%o\x00%x\n", filepath.ToSlash(rel), st.Mode().Perm(), sha256.Sum256(b))
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), e
}

func (i *Installer) CheckOwned(r InstallReceipt) error {
	for p, want := range r.Owned {
		got, e := Image(p)
		if e != nil {
			return e
		}
		if !SameImage(got, want) {
			return fmt.Errorf("conflict: local modification at %s; preserved, no changes made", p)
		}
	}
	for p, want := range r.Digests {
		got, e := treeDigest(p)
		if e != nil {
			return e
		}
		if got != want {
			return fmt.Errorf("conflict: vendor generation modified at %s; preserved", p)
		}
	}
	return nil
}

type installLocks struct{ install, tick *fsx.FileLock }

func (l *installLocks) Close() { l.tick.Close(); l.install.Close() }

func (i *Installer) lock() (*installLocks, error) {
	if e := checkInstallParents(i.Home, filepath.Join(i.Share(), "install.lock")); e != nil {
		return nil, e
	}
	if e := checkInstallParents(i.Home, filepath.Join(i.Home, ".agentq", "tick.lock")); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(i.Share(), 0700); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Join(i.Home, ".agentq"), 0700); e != nil {
		return nil, e
	}
	one, e := fsx.AcquireLock(filepath.Join(i.Share(), "install.lock"), 0)
	if e != nil {
		return nil, e
	}
	if one == nil {
		return nil, errInstallBusy
	}
	wait := i.TickWait
	if wait == 0 {
		wait = defaultTickWait
	}
	two, e := fsx.AcquireLock(filepath.Join(i.Home, ".agentq", "tick.lock"), wait)
	if e != nil {
		one.Close()
		return nil, e
	}
	if two == nil {
		one.Close()
		return nil, fmt.Errorf("background tick still running after %s; retry in a moment", wait)
	}
	return &installLocks{one, two}, nil
}

// Svc 是安装引擎看服务用的实例：cfg 决定服务名，假命令照传。
func (i *Installer) Svc(cfg config.Config) *service.Service {
	return service.ForPaths(i.Home, cfg, i.Launchctl, i.Systemctl, i.Loginctl)
}

func (i *Installer) mgr() service.ServiceManager { return i.Svc(config.Config{}).Svc() }

func (i *Installer) Loaded(label string) bool {
	if i.Svc(config.Config{}).CheckRealLaunchd() != nil {
		return false
	}
	return i.mgr().IsLoaded(label)
}

func (i *Installer) unload(labels []string) error {
	m := i.mgr()
	for _, s := range labels {
		if i.Loaded(s) {
			out, e := m.Unload(s)
			if e != nil {
				return fmt.Errorf("%s %s: %w %s", m.Words().UnloadVerb, s, e, out)
			}
		}
	}
	return nil
}

func (i *Installer) load(labels []string) error {
	m := i.mgr()
	for _, s := range labels {
		if e := i.Svc(config.Config{}).CheckRealLaunchd(); e != nil {
			return fmt.Errorf("%s %s: %w ", m.Words().LoadVerb, s, e)
		}
		out, e := m.Load(s)
		if e != nil {
			return fmt.Errorf("%s %s: %w %s", m.Words().LoadVerb, s, e, out)
		}
	}
	return nil
}

// reloadAfterFiles：事务改完服务文件之后调用，这次没动服务就什么也不做。没选服务的安装不碰 systemctl。
func (i *Installer) reloadAfterFiles(oldServices, newServices []string) error {
	if len(oldServices)+len(newServices) == 0 {
		return nil
	}
	if e := i.Svc(config.Config{}).CheckRealLaunchd(); e != nil {
		return e
	}
	return i.mgr().Reload()
}

func (i *Installer) recover(j *InstallJournal) error {
	if e := i.unload(j.NewServices); e != nil {
		return e
	}
	// 已写的条目只在仍等于事务前/后像时恢复，避免吞掉中断后的用户修改。
	for n := len(j.Changes) - 1; n >= 0; n-- {
		c := j.Changes[n]
		if e := checkInstallParents(i.Home, c.Path); e != nil {
			return e
		}
		v, e := Image(c.Path)
		if e != nil {
			return e
		}
		if SameImage(v, c.Before) {
			continue
		}
		if !SameImage(v, c.After) {
			return fmt.Errorf("recovery conflict: %s; backup retained", c.Path)
		}
		if e = PutImage(c.Path, c.Before); e != nil {
			return e
		}
	}
	if e := i.reloadAfterFiles(j.OldServices, j.NewServices); e != nil {
		return e
	}
	if e := i.load(j.OldServices); e != nil {
		return e
	}
	return os.Remove(i.PendingPath())
}

func (i *Installer) transaction(changes []InstallChange, oldServices, newServices []string) error {
	for _, c := range changes {
		if e := checkInstallParents(i.Home, c.Path); e != nil {
			return e
		}
	}
	j := InstallJournal{Changes: changes, OldServices: oldServices, NewServices: newServices}
	if e := SaveInstallJSON(i.PendingPath(), j); e != nil {
		return e
	}
	backup := filepath.Join(i.Share(), "backups", time.Now().UTC().Format("20060102T150405.000000000")+".json")
	if e := SaveInstallJSON(backup, j); e != nil {
		return e
	}
	apply := func() error {
		if e := i.unload(oldServices); e != nil {
			return e
		}
		for n, c := range changes {
			now, e := Image(c.Path)
			if e != nil {
				return e
			}
			if !SameImage(now, c.Before) {
				return fmt.Errorf("conflict during install: %s", c.Path)
			}
			if e = PutImage(c.Path, c.After); e != nil {
				return e
			}
			j.Applied = n + 1
			if e = SaveInstallJSON(i.PendingPath(), j); e != nil {
				return e
			}
			if i.AfterWrite != nil {
				if e = i.AfterWrite(n + 1); e != nil {
					return e
				}
			}
		}
		if e := i.reloadAfterFiles(oldServices, newServices); e != nil {
			return e
		}
		return i.load(newServices)
	}
	if e := apply(); e != nil {
		if r := i.recover(&j); r != nil {
			return fmt.Errorf("%w; rollback failed: %v; run agentq restore", e, r)
		}
		return fmt.Errorf("%w; installation rolled back", e)
	}
	j.Completed = true
	if e := SaveInstallJSON(backup, j); e != nil {
		return e
	}
	if e := os.Remove(i.PendingPath()); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Installed transaction backup: %s\n", backup)
	return nil
}

func (i *Installer) NoPending() error {
	if _, e := os.Stat(i.PendingPath()); e == nil {
		return errors.New("interrupted installation: run agentq restore before continuing")
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return nil
}

func validateHostSettings(home string) error {
	for _, p := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude", "settings.local.json")} {
		b, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		var v map[string]any
		if json.Unmarshal(b, &v) != nil || v == nil || !validSettingsShape(v) {
			return fmt.Errorf("invalid Claude settings: %s (preserved)", p)
		}
	}
	return nil
}

func ReadInstallConfig(home string) (config.Config, bool, error) {
	c := config.Default()
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "config.json"))
	if errors.Is(e, os.ErrNotExist) {
		return c, false, nil
	}
	if e != nil {
		return c, true, e
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return c, true, errors.New("invalid agentq config: expected object (preserved)")
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, true, fmt.Errorf("invalid agentq config (preserved): %w", e)
	}
	if e = c.ClearClaudeDefaults(b); e != nil {
		return c, true, fmt.Errorf("invalid agentq config (preserved): %w", e)
	}
	return c, true, c.Validate()
}

func InstallerIdle(home string) error {
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "state.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return errors.New("state unknown: expected object")
	}
	var st state.State
	if e = json.Unmarshal(b, &st); e != nil {
		return fmt.Errorf("state unknown: %w", e)
	}
	for _, j := range st.Jobs {
		if j == nil {
			return errors.New("state unknown: null job")
		}
		if j.Status == "running" {
			return errors.New("jobs still running; wait for completion before replacing installation")
		}
	}
	return nil
}

func BundledFiles(assets fs.FS) (map[string]InstallFile, error) {
	files := map[string]InstallFile{}
	e := fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := fs.ReadFile(assets, p)
		if e != nil {
			return e
		}
		mode := fs.FileMode(0644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0755
		}
		files[p] = InstallFile{b, mode}
		return nil
	})
	return files, e
}

type dependencyLock struct {
	ThirdPartySources []struct {
		URL, Commit string
		Modules     []struct {
			Name, InstallPolicy string
			Files               []struct{ Path, BlobSha string }
		}
		RequiredArtifacts []struct{ SourcePath, BlobSha, Target string }
	}
}

// defaultFetch 是没有注入 Fetch 时取第三方依赖的办法。只有带 agentq_fake_deps 构建标记的测试程序会换掉它（见 fetch_fake_deps.go），发行版里没有这条路。
var defaultFetch = fetchDependencies

func fetchDependencies(assets fs.FS, extras bool) (map[string]InstallFile, error) {
	b, _ := fs.ReadFile(assets, "dependencies/claude.lock.json")
	var lock dependencyLock
	if e := json.Unmarshal(b, &lock); e != nil {
		return nil, e
	}
	files := map[string]InstallFile{}
	for _, s := range lock.ThirdPartySources {
		dir, e := os.MkdirTemp("", "agentq-dependency-*")
		if e != nil {
			return nil, e
		}
		defer os.RemoveAll(dir)
		run := func(args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-C", dir}, args...)...)
			cmd.Env = append(InstallEnv(os.Getenv("HOME")), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
			return cmd.Output()
		}
		if _, e = run("init", "-q"); e != nil {
			return nil, e
		}
		// 镜像只是换个来源：下面每个文件都按锁定的 blob 哈希核对，改过的进不来。镜像不通再走原地址。
		_, e = run("fetch", "--depth=1", viaMirror(os.Getenv("AGENTQ_MIRROR"), s.URL), s.Commit)
		if e != nil && os.Getenv("AGENTQ_MIRROR") != "" {
			_, e = run("fetch", "--depth=1", s.URL, s.Commit)
		}
		if e != nil {
			return nil, fmt.Errorf("fetch locked third party: %w", e)
		}
		add := func(path, sha, target string) error {
			data, e := run("show", s.Commit+":"+path)
			if e != nil {
				return e
			}
			h := sha1.New()
			fmt.Fprintf(h, "blob %d%c", len(data), 0)
			h.Write(data)
			if hex.EncodeToString(h.Sum(nil)) != sha {
				return fmt.Errorf("locked blob mismatch: %s", path)
			}
			mode := fs.FileMode(0644)
			if strings.HasSuffix(path, ".sh") {
				mode = 0755
			}
			files[target] = InstallFile{data, mode}
			return nil
		}
		for _, m := range s.Modules {
			if m.InstallPolicy != "required" && !extras {
				continue
			}
			for _, f := range m.Files {
				if e = add(f.Path, f.BlobSha, f.Path); e != nil {
					return nil, e
				}
			}
		}
		for _, a := range s.RequiredArtifacts {
			if e = add(a.SourcePath, a.BlobSha, "licenses/"+filepath.Base(a.Target)); e != nil {
				return nil, e
			}
		}
	}
	return files, nil
}

func (i *Installer) stage(o SetupOptions) (string, []string, error) {
	files, e := BundledFiles(i.Assets)
	if e != nil {
		return "", nil, e
	}
	fetch := i.Fetch
	if fetch == nil {
		fetch = func(extras bool) (map[string]InstallFile, error) { return defaultFetch(i.Assets, extras) }
	}
	deps, e := fetch(o.Extras)
	if e != nil {
		return "", nil, e
	}
	for p, v := range deps {
		files[p] = v
	}
	if o.Scope == "full" {
		self, e := os.Executable()
		if e != nil {
			return "", nil, e
		}
		b, e := os.ReadFile(self)
		if e != nil {
			return "", nil, e
		}
		files["bin/agentq"] = InstallFile{b, 0755}
	}
	generation, e := os.MkdirTemp(filepath.Join(i.Share(), "generations"), "gen-")
	if e != nil {
		return "", nil, e
	}
	for p, v := range files {
		if filepath.IsAbs(p) || strings.Contains(p, "..") {
			return "", nil, fmt.Errorf("invalid asset path: %s", p)
		}
		dst := filepath.Join(generation, p)
		if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
			return "", nil, e
		}
		if e = os.WriteFile(dst, v.Data, v.Mode); e != nil {
			return "", nil, e
		}
		if e = os.Chmod(dst, v.Mode); e != nil {
			return "", nil, e
		}
	}
	entries, e := os.ReadDir(filepath.Join(generation, "skills"))
	if e != nil {
		return "", nil, e
	}
	var names []string
	for _, d := range entries {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	if o.Scope == "full" {
		check := i.Check
		if check == nil {
			check = checkCandidate
		}
		if e = check(filepath.Join(generation, "bin", "agentq")); e != nil {
			return "", nil, e
		}
	}
	return generation, names, nil
}

func checkCandidate(bin string) error {
	home, e := os.MkdirTemp("", "agentq-selfcheck-*")
	if e != nil {
		return e
	}
	defer os.RemoveAll(home)
	if e = config.Save(filepath.Join(home, ".agentq", "config.json"), config.Default().Fresh()); e != nil {
		return e
	}
	for _, arg := range []string{"status", "tick"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, bin, arg)
		cmd.Env = InstallEnv(home)
		cmd.Dir = home
		out, e := cmd.CombinedOutput()
		cancel()
		if e != nil {
			return fmt.Errorf("candidate selfcheck %s: %w %s", arg, e, out)
		}
	}
	return nil
}

func (i *Installer) Setup(o SetupOptions) error {
	if o.Scope != "full" && o.Scope != "skills" {
		return errors.New("scope must be full or skills")
	}
	if o.Scope == "skills" && (o.Services || o.Backup || o.Watchdog || o.Listen) {
		return errors.New("services require full scope")
	}
	cfg, exists, e := ReadInstallConfig(i.Home)
	if e != nil {
		if ve := validateHostSettings(i.Home); ve != nil {
			return ve
		}
		return e
	}
	if o.Backend != "" {
		if !backend.Known(o.Backend) {
			return fmt.Errorf("unknown backend %q; available: %v", o.Backend, backend.Names())
		}
		cur := cfg.Backend
		if cur == "" {
			cur = backend.Default
		}
		if exists && cur != o.Backend {
			return fmt.Errorf("--backend %s differs from the preserved configuration (%s); update your configuration explicitly, then retry", o.Backend, cur)
		}
		if !exists {
			cfg.Backend = o.Backend
			if o.Backend == backend.Default {
				cfg.Backend = ""
			}
		}
	}
	if usesClaude(cfg) {
		if e := validateHostSettings(i.Home); e != nil {
			return e
		}
	}
	engine := cfg.Backend
	explicit := o.Claude
	if explicit == "" && exists {
		explicit = cfg.BackendPath(engine)
	}
	if o.Scope == "full" {
		r, e := ProbeBackendEnv(engine, i.Home, explicit, InstallEnv(i.Home))
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(i.Out, string(b))
		if e != nil && !deferrable(o, explicit, e) {
			return e
		}
		if exists && o.Claude != "" {
			configured, err := exec.LookPath(fsx.ExpandHome(i.Home, cfg.BackendPath(engine)))
			if err != nil {
				return fmt.Errorf("configured %s unavailable; update your configuration explicitly before selecting another engine", pathKey(engine))
			}
			configured, err = filepath.EvalSymlinks(configured)
			selected, selectedErr := filepath.EvalSymlinks(r.Path)
			if err != nil || selectedErr != nil || configured != selected {
				return fmt.Errorf("--claude differs from preserved %s; update your configuration explicitly, then retry", pathKey(engine))
			}
		}
		if !exists && r.Path != "" {
			cfg.SetBackendPath(engine, r.Path)
		}
	}
	lock, e := i.lock()
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = i.NoPending(); e != nil {
		return e
	}
	if e = InstallerIdle(i.Home); e != nil {
		return e
	}
	old, e := i.Receipt()
	if e != nil {
		return e
	}
	if e = i.CheckOwned(old); e != nil {
		return e
	}
	if old.Version != 0 && old.Scope != o.Scope {
		return errors.New("scope change requires remove then setup; existing install preserved")
	}
	if old.Extras {
		o.Extras = true
	}
	for _, label := range old.Services {
		switch label {
		case cfg.LaunchdLabel:
			o.Services = true
		case cfg.LaunchdLabel + ".backup":
			o.Backup = true
		case cfg.LaunchdLabel + ".watchdog":
			o.Watchdog = true
		case cfg.LaunchdLabel + ".listen":
			o.Listen = true
		}
	}
	if o.Listen && !o.Services {
		return errors.New("--listen requires --services: incoming Telegram messages become tasks that the tick service runs and finishes")
	}
	if o.Services || o.Backup || o.Watchdog {
		if e = i.mgr().Available(); e != nil {
			return e
		}
	}
	if o.Services {
		a := i.Svc(cfg)
		r, e := ProbeBackendEnv(engine, i.Home, cfg.BackendPath(engine), a.LaunchdEnv())
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintf(i.Out, "Service environment (no inherited shell identity/proxy):\n%s\n", b)
		if e != nil {
			return fmt.Errorf("service environment unavailable: %w; configure durable CLI authentication/network settings explicitly, then retry; no secrets are written to %s", e, a.Svc().Words().UnitFile)
		}
	}
	if o.Backup {
		path, e := exec.LookPath("gh")
		if e != nil {
			return errors.New("optional backup requires gh; not installed, no changes made")
		}
		version, e := probeCommand(i.Home, path, "--version")
		if e != nil {
			return fmt.Errorf("optional backup gh version check failed: %w", e)
		}
		fmt.Fprintf(i.Out, "Optional backup tool: %s (%s); remote authorization is checked at backup time\n", path, strings.TrimSpace(string(version)))
	}
	if python, e := probeCommand(i.Home, "python3", "--version"); e != nil {
		i.notef("%s\n", "Warning: python3 not found. The scheduler and required skills work without it; only optional Waza helper scripts (read, write, health, check) cannot run until you install Python 3.")
	} else {
		fmt.Fprintf(i.Out, "Support interpreter: %s\n", strings.TrimSpace(string(python)))
	}
	if e = os.MkdirAll(filepath.Join(i.Share(), "generations"), 0700); e != nil {
		return e
	}
	gen, names, e := i.stage(o)
	if e != nil {
		return e
	}
	desired := map[string]FileImage{}
	link := func(path, target string) { desired[path] = FileImage{Kind: "link", Link: target} }
	link(filepath.Join(i.Share(), "current"), gen)
	for _, p := range []string{"templates", "support", "licenses", "dependencies"} {
		if _, e := os.Stat(filepath.Join(gen, p)); e == nil {
			link(filepath.Join(i.Share(), p), filepath.Join(gen, p))
		}
	}
	required := i.skillDirs(cfg)
	skillDirs := i.installSkillDirs(cfg)
	for _, dir := range skillDirs {
		for _, n := range names {
			link(filepath.Join(dir, n), filepath.Join(gen, "skills", n))
		}
	}
	if o.Scope == "full" {
		link(filepath.Join(i.Home, ".local", "bin", "agentq"), filepath.Join(gen, "bin", "agentq"))
	}
	var labels []string
	if len(old.Services) > 0 && !o.Services && !o.Backup && !o.Watchdog {
		return errors.New("existing services: repeat original --services/--backup/--watchdog selection, or remove first")
	}
	if o.Services || o.Backup || o.Watchdog {
		a := i.Svc(cfg)
		if e = a.CheckRealLaunchd(); e != nil {
			return e
		}
		m := a.Svc()
		var svcs []service.ServiceSpec
		if o.Services {
			svcs = m.Specs()[:3]
			desired[a.GuardPath()] = FileImage{Kind: "file", Data: []byte(i.GenerationGuard(old.Generation)), Mode: 0700}
		}
		if o.Listen {
			svcs = append(svcs, m.Specs()[3])
		}
		if o.Backup {
			svcs = append(svcs, m.AuxSpec("backup", 86400))
		}
		if o.Watchdog {
			svcs = append(svcs, m.AuxSpec("watchdog", 1800))
		}
		for _, s := range svcs {
			known := false
			for _, label := range old.Services {
				if label == s.Label {
					known = true
				}
			}
			if !known && i.Loaded(s.Label) {
				return fmt.Errorf("service conflict: %s already loaded outside this installer", s.Label)
			}
			labels = append(labels, s.Label)
			for _, f := range s.Files {
				desired[f.Path] = FileImage{Kind: "file", Data: []byte(f.Content), Mode: 0600}
			}
		}
	}
	r := InstallReceipt{Version: 1, Generation: gen, Scope: o.Scope, Extras: o.Extras, Owned: map[string]FileImage{}, Digests: map[string]string{}, External: map[string]string{}, Services: labels}
	if !slices.Equal(skillDirs, []string{i.claudeSkillDir()}) {
		r.SkillDirs = skillDirs
	}
	var changes []InstallChange
	keys := make([]string, 0, len(desired))
	for p := range desired {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		after := desired[p]
		if e := checkInstallParents(i.Home, p); e != nil {
			return e
		}
		before, e := Image(p)
		_, owned := old.Owned[p]
		_, lerr := os.Lstat(p) // 路径本身在才算同名技能；父目录读不了之类的错下面原样报
		if slices.Contains(skillDirs, filepath.Dir(p)) && !owned && lerr == nil {
			if _, se := os.Stat(p); se != nil {
				return fmt.Errorf("conflict: %s is a broken link; remove or fix it, then retry", p)
			}
			a, ae := treeDigest(p)
			b, be := treeDigest(after.Link)
			if ae == nil && be == nil && a == b {
				fmt.Fprintf(i.Out, "Keep identical external skill: %s\n", p)
				r.External[p] = a
				continue
			}
			name := filepath.Base(p)
			if slices.Contains(contractSkills, name) && slices.Contains(required, filepath.Dir(p)) { // 只给前台用的目录里，用户自己的版本留着，任务不读它
				return fmt.Errorf("conflict: your skill %s differs from the one agentq needs to run tasks; preserved. Rename it, then retry", p)
			}
			fmt.Fprintf(i.Out, "Keep your own skill (not replaced): %s\n", p)
			r.KeptSkills = append(r.KeptSkills, p)
			continue
		}
		if e != nil {
			return e
		}
		if before.Kind != "absent" && !owned {
			return fmt.Errorf("conflict: existing %s is not owned by agentq; preserved", p)
		}
		r.Owned[p] = after
		if !SameImage(before, after) {
			changes = append(changes, InstallChange{p, before, after})
		}
	}
	for p, before := range old.Owned {
		if _, ok := desired[p]; !ok {
			changes = append(changes, InstallChange{p, before, FileImage{Kind: "absent"}})
		}
	}
	digest, e := treeDigest(gen)
	if e != nil {
		return e
	}
	r.Digests[gen] = digest
	r.ConfigCreated = old.ConfigCreated
	if !exists && o.Scope == "full" {
		r.ConfigCreated = true
		cfg = cfg.Fresh()
		b, _ := json.MarshalIndent(cfg, "", "  ")
		changes = append(changes, InstallChange{filepath.Join(i.Home, ".agentq", "config.json"), FileImage{Kind: "absent"}, FileImage{Kind: "file", Data: append(b, '\n'), Mode: 0600}})
	}
	before, e := Image(i.ReceiptPath())
	if e != nil {
		return e
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	changes = append(changes, InstallChange{i.ReceiptPath(), before, FileImage{Kind: "file", Data: append(b, '\n'), Mode: 0600}})
	fmt.Fprintf(i.Out, "Plan: scope=%s; generation=%s; %d skills; %d changed paths; services=%v\nKeep: authentication, permissions, hooks, statusline, user settings, jobs, secrets, existing scheduler switches. Telegram: optional, not requested.\nRollback: agentq rollback; interrupted transaction: agentq restore.\n", o.Scope, gen, len(names), len(changes), labels)
	for _, change := range changes {
		fmt.Fprintf(i.Out, "  %s: %s -> %s\n", change.Path, change.Before.Kind, change.After.Kind)
	}
	fmt.Fprintf(i.Out, "Before activation: restricted transaction backup in %s; user data/settings are not part of code rollback\n", filepath.Join(i.Share(), "backups"))
	if o.Plan {
		fmt.Fprintln(i.Out, "Plan only: no activation")
		return nil
	}
	if !o.Yes {
		return errors.New("review plan then repeat with --yes to authorize these installation changes")
	}
	if old.Version != 0 {
		p := filepath.Join(i.Share(), "previous.json")
		v, e := Image(p)
		if e != nil {
			return e
		}
		data, _ := json.MarshalIndent(old, "", "  ")
		changes = append(changes, InstallChange{p, v, FileImage{Kind: "file", Data: append(data, '\n'), Mode: 0600}})
	}
	if e = i.transaction(changes, old.Services, labels); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Installed %s. Existing scheduler switches preserved. Tell %s what to build in any project folder; agentq status/run/logs are management commands.\n", o.Scope, cliName(cfg))
	i.refreshEnvLayer()
	if notes, e := i.Svc(cfg).Svc().Doctor(labels); e == nil {
		for _, n := range notes {
			i.notef("%s\n", n)
		}
	}
	return nil
}

// pathKey 是这个后端的路径写在配置的哪个键。
func pathKey(name string) string {
	if name == "" || name == backend.Default {
		return "claude_path"
	}
	return "backends." + name + ".path"
}

// cliName 是用户开会话用的命令。
func cliName(cfg config.Config) string {
	if cfg.Backend == "" {
		return backend.Default
	}
	return cfg.Backend
}

// deferrable：这次探测失败能不能留给 start --welcome。服务环境那次探测不走这里，已选的服务照样要求登录。
func deferrable(o SetupOptions, explicit string, e error) bool {
	if !o.DeferEngine || o.Claude != "" {
		return false
	}
	if errors.Is(e, backend.ErrNotLoggedIn) {
		return true
	}
	return errors.Is(e, backend.ErrNoCLI) && (explicit == "" || explicit == config.Default().ClaudePath || backend.Known(explicit))
}

func (i *Installer) GenerationGuard(previous string) string {
	recovery := filepath.Join(previous, "bin", "agentq")
	if previous == "" {
		recovery = filepath.Join(i.Home, ".local", "bin", "agentq")
	}
	return "#!/bin/sh\n" + "bin=" + fsx.ShQuote(filepath.Join(i.Home, ".local", "bin", "agentq")) + "\nfails=" + fsx.ShQuote(filepath.Join(i.Home, ".agentq", "guard-fails")) + "\n\"$bin\" tick\nrc=$?\n[ \"$rc\" = 0 ] && { rm -f \"$fails\"; exit 0; }\nn=0; [ ! -f \"$fails\" ] || read n < \"$fails\"\ncase $n in ''|*[!0-9]*) n=0;; esac\nn=$((n+1)); printf '%s\\n' \"$n\" > \"$fails\"\nif [ \"$n\" -ge 2 ]; then " + fsx.ShQuote(recovery) + " rollback --yes && rm -f \"$fails\"; fi\nexit \"$rc\"\n"
}

// CheckClaudeConfigDir：安装投影只认默认的 $HOME/.claude。配置根本不用 Claude 时不用查。
func CheckClaudeConfigDir(home string) error {
	if cfg, _, e := ReadInstallConfig(home); e == nil && !usesClaude(cfg) {
		return nil
	}
	if custom := os.Getenv("CLAUDE_CONFIG_DIR"); custom != "" && filepath.Clean(fsx.ExpandHome(home, custom)) != filepath.Join(home, ".claude") {
		return errors.New("custom CLAUDE_CONFIG_DIR requires an explicit host migration; default projection not activated")
	}
	return nil
}

func RunInstaller(assets fs.FS, cmd string, args []string, out, errout io.Writer) int {
	home, e := os.UserHomeDir()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	i := &Installer{Assets: assets, Home: home, Out: out, Launchctl: service.TestLaunchctl()}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	f.SetOutput(errout)
	var o SetupOptions
	var quiet bool
	f.BoolVar(&o.Yes, "yes", false, "confirm planned changes")
	if cmd == "setup" || cmd == "remove" {
		f.BoolVar(&quiet, "quiet", false, "only warnings and notes; print the details when it fails")
	}
	if cmd == "setup" {
		f.StringVar(&o.Scope, "scope", "full", "full or skills")
		f.StringVar(&o.Claude, "claude", "", "independent CLI path")
		f.StringVar(&o.Claude, "backend-path", "", "independent CLI path (same as --claude)")
		f.StringVar(&o.Backend, "backend", "", "backend for a new install (claude by default); an existing configuration is not changed")
		f.BoolVar(&o.Plan, "plan", false, "prepare and print plan without activating")
		f.BoolVar(&o.Services, "services", false, "install inert tick/night/review "+i.mgr().Words().Name+" services")
		f.BoolVar(&o.Listen, "listen", false, "install the Telegram listener service (requires --services)")
		f.BoolVar(&o.Backup, "backup", false, "install opt-in private Git backup service")
		f.BoolVar(&o.Watchdog, "watchdog", false, "install opt-in local watchdog service")
		f.BoolVar(&o.Extras, "extras", false, "include optional locked Waza skills")
		f.BoolVar(&o.DeferEngine, "defer-engine", false, "install even if the CLI is missing or logged out; used by install.sh")
	}
	if e = f.Parse(args); e != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprintln(errout, "unexpected positional arguments")
		return 2
	}
	if o.Backend == "" || o.Backend == backend.Default {
		if e = CheckClaudeConfigDir(home); e != nil {
			fmt.Fprintln(errout, "agentq:", e)
			return 1
		}
	}
	var details bytes.Buffer
	if quiet {
		i.Notes, i.Out = out, &details
	}
	switch cmd {
	case "setup":
		e = i.Setup(o)
	case "doctor":
		e = i.Doctor()
	case "restore":
		e = i.Restore(o.Yes)
	case "rollback":
		e = i.Rollback(o.Yes)
	case "remove":
		e = i.Remove(o.Yes)
	}
	if e != nil {
		errout.Write(details.Bytes())
		fmt.Fprintln(errout, "agentq:", e)
		return 1
	}
	return 0
}
