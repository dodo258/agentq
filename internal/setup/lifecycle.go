package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/envlayer"
	"agentq/internal/fsx"
)

func (i *Installer) activate(r InstallReceipt, old InstallReceipt) error {
	if e := i.validateReceipt(r); e != nil {
		return e
	}
	if e := i.checkExternal(r); e != nil {
		return e
	}
	if e := i.CheckOwned(old); e != nil {
		return e
	}
	for p, want := range r.Digests {
		got, e := treeDigest(p)
		if e != nil {
			return e
		}
		if got != want {
			return fmt.Errorf("restore target modified: %s", p)
		}
	}
	var changes []InstallChange
	keys := map[string]bool{}
	for p := range old.Owned {
		keys[p] = true
	}
	for p := range r.Owned {
		keys[p] = true
	}
	var paths []string
	for p := range keys {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		before, e := Image(p)
		if e != nil {
			return e
		}
		after, ok := r.Owned[p]
		if !ok {
			after = FileImage{Kind: "absent"}
		}
		if _, managed := old.Owned[p]; !managed && before.Kind != "absent" {
			return fmt.Errorf("restore conflict: %s", p)
		}
		if !SameImage(before, after) {
			changes = append(changes, InstallChange{p, before, after})
		}
	}
	before, e := Image(i.ReceiptPath())
	if e != nil {
		return e
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	changes = append(changes, InstallChange{i.ReceiptPath(), before, FileImage{Kind: "file", Data: append(b, '\n'), Mode: 0600}})
	return i.transaction(changes, old.Services, r.Services)
}

func (i *Installer) Rollback(yes bool) error {
	if !yes {
		return errors.New("rollback requires --yes; code/skills/services change, data and configuration stay")
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
	if old.Version == 0 {
		return errors.New("no active installation")
	}
	var r InstallReceipt
	b, e := os.ReadFile(filepath.Join(i.Share(), "previous.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r.Version != 1 {
		return errors.New("invalid previous receipt")
	}
	if e = i.activate(r, old); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(i.Share(), "previous.json")); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Rolled back complete generation to %s; data/config unchanged\n", r.Generation)
	return nil
}

func (i *Installer) Remove(yes bool) error {
	if !yes {
		return errors.New("remove requires --yes; removes owned entries/services only, keeps data/config and recovery generations")
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
	r, e := i.Receipt()
	if e != nil {
		return e
	}
	if r.Version == 0 {
		// 上次卸载到改 shell 配置文件时失败了：回执已经没了，接着把那一行清掉。
		notes, e := RemovePathLine(i.Home)
		for _, n := range notes {
			i.notef("%s\n", n)
		}
		touched, envErr := i.removeEnvLayer()
		if e != nil {
			return fmt.Errorf("shell 配置文件没改回去：%w", e)
		}
		if envErr != nil {
			return fmt.Errorf("通用工程规则没清掉：%w", envErr)
		}
		if len(notes) == 0 && !touched {
			return errors.New("no managed installation to remove")
		}
		return nil
	}
	if e = i.CheckOwned(r); e != nil {
		return e
	}
	if e = SaveInstallJSON(filepath.Join(i.Share(), "removed.json"), r); e != nil {
		return e
	}
	var changes []InstallChange
	var keys []string
	for p := range r.Owned {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		changes = append(changes, InstallChange{p, r.Owned[p], FileImage{Kind: "absent"}})
	}
	before, e := Image(i.ReceiptPath())
	if e != nil {
		return e
	}
	changes = append(changes, InstallChange{i.ReceiptPath(), before, FileImage{Kind: "absent"}})
	if e = i.transaction(changes, r.Services, nil); e != nil {
		return e
	}
	restore := fsx.ShQuote(filepath.Join(r.Generation, "bin", "agentq")) + " restore --yes"
	if r.Scope == "skills" {
		restore = "sh scripts/bootstrap.sh restore --yes (from the source checkout)"
	}
	fmt.Fprintf(i.Out, "Removed owned entries/services. Keep data/config/generations. Restore using: %s\n", restore)
	if i.Notes != nil {
		i.notef("反悔了可以恢复：%s\n", restore)
	}
	notes, e := RemovePathLine(i.Home)
	for _, n := range notes {
		i.notef("%s\n", n)
	}
	_, envErr := i.removeEnvLayer()
	if e != nil {
		return fmt.Errorf("程序和服务已卸载，但 shell 配置文件没改回去：%w", e)
	}
	if envErr != nil {
		return fmt.Errorf("程序和服务已卸载，但通用工程规则没清掉：%w", envErr)
	}
	return nil
}

func (i *Installer) Restore(yes bool) error {
	if !yes {
		return errors.New("restore requires --yes; first review installation backup and preserved conflicts")
	}
	lock, e := i.lock()
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = InstallerIdle(i.Home); e != nil {
		return e
	}
	b, e := os.ReadFile(i.PendingPath())
	if e == nil {
		var j InstallJournal
		if e = json.Unmarshal(b, &j); e != nil {
			return e
		}
		if e = i.recover(&j); e != nil {
			return e
		}
		fmt.Fprintln(i.Out, "Recovered interrupted transaction; previous installation restored")
		return nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	old, e := i.Receipt()
	if e != nil {
		return e
	}
	if old.Version != 0 {
		return errors.New("active installation exists; use rollback, not uninstall restore")
	}
	b, e = os.ReadFile(filepath.Join(i.Share(), "removed.json"))
	if e != nil {
		return e
	}
	var r InstallReceipt
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r.Version != 1 {
		return errors.New("invalid removed receipt")
	}
	if e = i.activate(r, old); e != nil {
		return e
	}
	fmt.Fprintln(i.Out, "Restored removed installation; data and user config unchanged")
	if _, e := i.envLayer().Restore(); e != nil {
		i.notef("通用工程规则没恢复（程序已恢复），可以运行 agentq env on：%v\n", e)
	}
	return nil
}

func (i *Installer) Doctor() error {
	if cfg, _, e := ReadInstallConfig(i.Home); e != nil || usesClaude(cfg) { // 和安装时一样：只用别的后端时不查 Claude 的设置文件
		if e := validateHostSettings(i.Home); e != nil {
			return e
		}
	}
	if e := i.NoPending(); e != nil {
		return e
	}
	cfg, _, e := ReadInstallConfig(i.Home)
	if e != nil {
		return e
	}
	r, e := i.Receipt()
	if e != nil {
		return e
	}
	if r.Version == 0 {
		return errors.New("no managed installation; run bootstrap setup before doctor")
	}
	if e = i.CheckOwned(r); e != nil {
		return e
	}
	if e = i.checkExternal(r); e != nil {
		return e
	}
	fmt.Fprintf(i.Out, "Installation: scope=%s generation=%s; settings unchanged; Telegram optional\n", r.Scope, r.Generation)
	if r.Scope == "skills" {
		fmt.Fprintln(i.Out, "Skills-only integrity checked; background CLI/authentication not required")
		return nil
	}
	if NeedsConsent(cfg) {
		fmt.Fprintln(i.Out, PermissionDefaultHint)
	}
	if note := consentNote(i.Home); note != "" {
		fmt.Fprintln(i.Out, note)
	}
	if pinned := cfg.PinnedModels(); len(pinned) > 0 {
		fmt.Fprintf(i.Out, "Model IDs pinned to a version (%s): new models will not be picked up; use an alias such as opus or sonnet\n", strings.Join(pinned, ", "))
	}
	notes, e := i.Svc(cfg).Svc().Doctor(r.Services)
	for _, n := range notes {
		fmt.Fprintln(i.Out, n)
	}
	if e != nil {
		return e
	}
	report, e := ProbeBackendEnv(cfg.Backend, i.Home, cfg.BackendPath(cfg.Backend), InstallEnv(i.Home))
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Fprintln(i.Out, string(b))
	fmt.Fprint(i.Out, backend.Describe(backend.Get(cfg.Backend, backend.Options{Home: i.Home}), report.Probe, e))
	if pe := i.doctorProjectBackends(cfg); e == nil {
		e = pe
	}
	if se := i.doctorSkillDirs(r, cfg); e == nil {
		e = se
	}
	return e
}

// doctorSkillDirs：改了配置换后端之后，技能还没投影到新后端读的目录，任务会读不到 agent-task。
func (i *Installer) doctorSkillDirs(r InstallReceipt, cfg config.Config) error {
	have := i.receiptSkillDirs(r)
	for _, dir := range i.skillDirs(cfg) {
		if !slices.Contains(have, dir) {
			return fmt.Errorf("skills are not projected to %s for the configured backend; run agentq setup --yes", dir)
		}
	}
	return nil
}

// doctorProjectBackends 查 project_backends 的每个值：配置决定了接不了活的项目说明白，默认后端以外的后端逐个探测。
// 后端做不了要合并的任务只说明，不算错误：这是后端的能力，不是配置写错。写错和没装才返回错误。
func (i *Installer) doctorProjectBackends(cfg config.Config) error {
	for _, p := range backend.ConfigProblems(cfg.Backend, cfg.ProjectBackends) {
		fmt.Fprintln(i.Out, "配置问题："+p)
	}
	seen := map[string]bool{cfg.Backend: true}
	if cfg.Backend == "" {
		seen[backend.Default] = true
	}
	i.doctorPermissions(cfg)
	var names []string
	for _, name := range cfg.ProjectBackends {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var first error
	for _, name := range names {
		report, e := ProbeBackendEnv(name, i.Home, cfg.BackendPath(name), InstallEnv(i.Home))
		fmt.Fprintf(i.Out, "project_backends 用到的后端：\n%s", backend.Describe(backend.Get(name, backend.Options{Home: i.Home}), report.Probe, e))
		if e != nil && first == nil {
			first = fmt.Errorf("project_backends 的后端 %s：%w", name, e)
		}
	}
	return first
}

// doctorPermissions：权限没配的后端任务不起，说明白要写什么。不算错误：agentq start 就是来放开权限的，它也跑这份检查。
func (i *Installer) doctorPermissions(cfg config.Config) {
	for _, why := range cfg.PermissionProblems() {
		fmt.Fprintln(i.Out, "配置问题："+why)
	}
}

func checkInstallParents(home, path string) error { return fsx.CheckParents(home, path) }

// envLayer 是安装引擎看环境层用的实例。配置读不了时按默认后端：卸载、升级都认记录里的目录，不靠它。
func (i *Installer) envLayer() *envlayer.Layer {
	cfg, _, e := ReadInstallConfig(i.Home)
	if e != nil {
		cfg = config.Default()
	}
	return envlayer.New(i.Home, i.Assets, backend.Get(cfg.Backend, backend.Options{Home: i.Home}))
}

// refreshEnvLayer：升级时让装着的规则跟上新版本。规则是附带的东西，这里失败不算升级失败，说一句。
func (i *Installer) refreshEnvLayer() {
	rep, e := i.envLayer().Refresh()
	if e != nil {
		i.notef("通用工程规则没更新（程序已升级）：%v\n", e)
	}
	for _, n := range rep.Kept {
		i.notef("%s 你改过，这次升级没动它（%s）\n", n, rep.Dir)
	}
	if rep.AllRemoved {
		i.notef("通用工程规则都被删了，当作不要了，升级不写回。要加回来：agentq env on\n")
	}
	for _, n := range rep.Declined {
		i.notef("%s 你删了，升级不写回。要加回来：agentq env on\n", n)
	}
}

// removeEnvLayer：卸载时只删没改过的规则文件，改过的留着并报告。返回这次有没有动过什么。
func (i *Installer) removeEnvLayer() (bool, error) {
	rep, e := i.envLayer().Uninstall()
	if len(rep.Removed) > 0 {
		i.notef("通用工程规则已删：%s\n", strings.Join(rep.Removed, "、"))
	}
	for _, n := range rep.Kept {
		i.notef("%s 你改过，留着：%s\n", n, filepath.Join(rep.Dir, n))
	}
	return len(rep.Removed)+len(rep.Kept) > 0, e
}
