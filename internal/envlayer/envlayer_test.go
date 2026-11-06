package envlayer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agentq/internal/backend"
)

// fakeBackend 只有规则目录这一个能力有用。
type fakeBackend struct{ noRules bool }

func (fakeBackend) Name() string       { return "fake" }
func (fakeBackend) Caps() backend.Caps { return backend.Caps{} }
func (fakeBackend) EnvKeep() []string  { return nil }
func (fakeBackend) Probe(context.Context, backend.ProbeEnv) (backend.Probe, error) {
	return backend.Probe{}, nil
}
func (fakeBackend) Auth(context.Context) backend.Auth { return backend.Auth{} }
func (fakeBackend) Build(backend.Request) (backend.Invocation, []backend.Degradation, error) {
	return backend.Invocation{}, nil, nil
}
func (fakeBackend) Parse(backend.JobRef, backend.Exit) backend.Result { return backend.Result{} }
func (fakeBackend) LastActivity(backend.JobRef) (time.Time, bool)     { return time.Time{}, false }
func (fakeBackend) CanResume(string) bool                             { return false }
func (fakeBackend) RecordedModel([]string) string                     { return "" }

type rulesBackend struct{ fakeBackend }

func (rulesBackend) RulesPaths(home string) (string, []string) {
	return filepath.Join(home, ".fake", "rules"), []string{filepath.Join(home, ".fake", "MAIN.md")}
}

func assetsV1() fstest.MapFS {
	return fstest.MapFS{
		"rules/core.md": {Data: []byte("core v1\n")},
		"rules/go.md":   {Data: []byte("go v1\n")},
	}
}

func newLayer(t *testing.T, assets fstest.MapFS) (*Layer, string) {
	t.Helper()
	home := t.TempDir()
	return New(home, assets, rulesBackend{}), home
}

func ruleFile(home, name string) string { return filepath.Join(home, ".fake", "rules", "agentq", name) }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// 空 HOME：该自动装，装上后两个文件都在，状态是 on。
func TestEnableOnEmptyHome(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	s, err := l.Status()
	if err != nil || !s.ShouldAutoEnable() || s.State != StateNone || len(s.Files) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
	rep, err := l.Enable()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Written, []string{"core.md", "go.md"}) || len(rep.Kept) != 0 {
		t.Fatalf("%+v", rep)
	}
	if got := mustRead(t, ruleFile(home, "core.md")); got != "core v1\n" {
		t.Fatal(got)
	}
	s, err = l.Status()
	if err != nil || s.State != StateOn || s.ShouldAutoEnable() || len(s.Files) != 2 || s.Files[0].State != FileIntact {
		t.Fatalf("%+v %v", s, err)
	}
	// 装的是用户读得到的文件，不是只有 owner 能读
	if fi, _ := os.Stat(ruleFile(home, "core.md")); fi.Mode().Perm()&0o400 == 0 {
		t.Fatal(fi.Mode())
	}
}

// 用户已有全局规则：各种形式都算，空文件、非 md、agentq 自己的不算。
func TestUserRulesDetection(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string)
		has   bool
	}{
		{"空目录", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".fake", "rules"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"全局说明文件有内容", func(t *testing.T, home string) { write(t, filepath.Join(home, ".fake", "MAIN.md"), "我的规则\n") }, true},
		{"全局说明文件只有空白", func(t *testing.T, home string) { write(t, filepath.Join(home, ".fake", "MAIN.md"), " \n\n") }, false},
		{"规则目录有 md", func(t *testing.T, home string) { write(t, filepath.Join(home, ".fake", "rules", "mine.md"), "x\n") }, true},
		{"规则目录深处有 md", func(t *testing.T, home string) {
			write(t, filepath.Join(home, ".fake", "rules", "a", "b", "mine.MD"), "x\n")
		}, true},
		{"非 md 不算", func(t *testing.T, home string) { write(t, filepath.Join(home, ".fake", "rules", ".DS_Store"), "x\n") }, false},
		{"空 md 不算", func(t *testing.T, home string) { write(t, filepath.Join(home, ".fake", "rules", "empty.md"), "\n") }, false},
		{"agentq 目录里的不算", func(t *testing.T, home string) { write(t, ruleFile(home, "core.md"), "core v1\n") }, false},
		{"软链接算", func(t *testing.T, home string) {
			target := filepath.Join(home, "dotfiles", "x.md")
			write(t, target, "x\n")
			link := filepath.Join(home, ".fake", "rules", "x.md")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"断了的软链接不算", func(t *testing.T, home string) {
			link := filepath.Join(home, ".fake", "rules", "x.md")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(home, "没有"), link); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"规则目录本身是软链接", func(t *testing.T, home string) {
			target := filepath.Join(home, "dotfiles", "rules")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(home, ".fake"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(home, ".fake", "rules")); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, home := newLayer(t, assetsV1())
			c.setup(t, home)
			s, err := l.Status()
			if err != nil {
				t.Fatal(err)
			}
			if got := len(s.UserRules) > 0; got != c.has {
				t.Fatalf("UserRules=%v", s.UserRules)
			}
			if s.ShouldAutoEnable() == c.has {
				t.Fatalf("ShouldAutoEnable=%v UserRules=%v", s.ShouldAutoEnable(), s.UserRules)
			}
		})
	}
}

// 用户明确 env on：已有规则也装，用户的文件一个字节不动。
func TestEnableExplicitKeepsUserRules(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	mine := filepath.Join(home, ".fake", "MAIN.md")
	theirs := filepath.Join(home, ".fake", "rules", "mine.md")
	write(t, mine, "我的\n")
	write(t, theirs, "他的\n")
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	if !exists(ruleFile(home, "core.md")) || mustRead(t, mine) != "我的\n" || mustRead(t, theirs) != "他的\n" {
		t.Fatal("装错了")
	}
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, mine) != "我的\n" || mustRead(t, theirs) != "他的\n" {
		t.Fatal("关闭动了用户的文件")
	}
}

// 关闭：没改过的删掉，改过的留着并报告，目录里还有东西就不删目录。
func TestDisableKeepsModified(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	write(t, ruleFile(home, "go.md"), "我改过\n")
	s, err := l.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Files, FileStatus{"go.md", FileModified}) || !slices.Contains(s.Files, FileStatus{"core.md", FileIntact}) {
		t.Fatalf("%+v", s.Files)
	}
	rep, err := l.Disable()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Removed, []string{"core.md"}) || !slices.Equal(rep.Kept, []string{"go.md"}) {
		t.Fatalf("%+v", rep)
	}
	if exists(ruleFile(home, "core.md")) || mustRead(t, ruleFile(home, "go.md")) != "我改过\n" {
		t.Fatal("文件不对")
	}
	s, _ = l.Status()
	if s.State != StateOff || s.ShouldAutoEnable() {
		t.Fatalf("%+v", s)
	}
	// 再打开：改过的还是不动
	rep, err = l.Enable()
	if err != nil || !slices.Equal(rep.Kept, []string{"go.md"}) || !slices.Equal(rep.Written, []string{"core.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
	if mustRead(t, ruleFile(home, "go.md")) != "我改过\n" {
		t.Fatal("被覆盖了")
	}
}

// 全删干净时目录也没了，父目录和用户的规则还在。
func TestDisableRemovesEmptyDir(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	theirs := filepath.Join(home, ".fake", "rules", "mine.md")
	write(t, theirs, "他的\n")
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Dir(ruleFile(home, "core.md"))) {
		t.Fatal("空目录该删")
	}
	if !exists(theirs) {
		t.Fatal("用户文件没了")
	}
}

// 用户往 agentq 目录里自己放的文件：不认识，不删。
func TestDisableLeavesUnknownFiles(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	extra := ruleFile(home, "mine.md")
	write(t, extra, "x\n")
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if !exists(extra) {
		t.Fatal("删了用户放的文件")
	}
}

// 这里本来就有同名的别的内容（没有记录）：不覆盖，也不当成自己的。
func TestEnableDoesNotAdoptForeignFile(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	write(t, ruleFile(home, "core.md"), "别人写的\n")
	rep, err := l.Enable()
	if err != nil || !slices.Equal(rep.Kept, []string{"core.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
	rep, err = l.Disable()
	if err != nil {
		t.Fatal(err)
	}
	if mustRead(t, ruleFile(home, "core.md")) != "别人写的\n" {
		t.Fatal("动了别人的文件")
	}
	if slices.Contains(rep.Removed, "core.md") {
		t.Fatalf("%+v", rep)
	}
}

// 内容一样的算自己的（上次写到一半断了、记录没存下的情况）。
func TestEnableAdoptsIdenticalFile(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	write(t, ruleFile(home, "core.md"), "core v1\n")
	rep, err := l.Enable()
	if err != nil || slices.Contains(rep.Kept, "core.md") {
		t.Fatalf("%+v %v", rep, err)
	}
	rep, err = l.Disable()
	if err != nil || !slices.Contains(rep.Removed, "core.md") {
		t.Fatalf("%+v %v", rep, err)
	}
}

// 升级：没改过的跟新版本走，改过的不动，新版本不带的文件没改过就删；没装的不加。
func TestRefresh(t *testing.T) {
	l, home := newLayer(t, fstest.MapFS{
		"rules/core.md": {Data: []byte("core v1\n")},
		"rules/go.md":   {Data: []byte("go v1\n")},
		"rules/old.md":  {Data: []byte("old\n")},
	})
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	write(t, ruleFile(home, "go.md"), "我改过\n")
	v2 := New(home, fstest.MapFS{
		"rules/core.md": {Data: []byte("core v2\n")},
		"rules/go.md":   {Data: []byte("go v2\n")},
		"rules/new.md":  {Data: []byte("new\n")},
	}, rulesBackend{})
	rep, err := v2.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Updated, []string{"core.md"}) || !slices.Equal(rep.Written, []string{"new.md"}) || !slices.Equal(rep.Removed, []string{"old.md"}) || !slices.Equal(rep.Kept, []string{"go.md"}) {
		t.Fatalf("%+v", rep)
	}
	if mustRead(t, ruleFile(home, "core.md")) != "core v2\n" || mustRead(t, ruleFile(home, "go.md")) != "我改过\n" || exists(ruleFile(home, "old.md")) {
		t.Fatal("文件不对")
	}
	// 新版本不带了、用户也删了的：记录清掉，不报告
	if err := os.Remove(ruleFile(home, "new.md")); err != nil {
		t.Fatal(err)
	}
	v3 := New(home, fstest.MapFS{"rules/core.md": {Data: []byte("core v2\n")}, "rules/go.md": {Data: []byte("go v2\n")}}, rulesBackend{})
	rep, err = v3.Refresh()
	if err != nil || slices.Contains(rep.Kept, "new.md") || slices.Contains(rep.Removed, "new.md") {
		t.Fatalf("%+v %v", rep, err)
	}
}

// 用户删掉的不写回：删一个，这次和以后的升级都不写，新版本才有的照写；env on 补全。
func TestRefreshKeepsUserDeletions(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ruleFile(home, "go.md")); err != nil {
		t.Fatal(err)
	}
	v2 := New(home, fstest.MapFS{
		"rules/core.md": {Data: []byte("core v2\n")},
		"rules/go.md":   {Data: []byte("go v2\n")},
		"rules/new.md":  {Data: []byte("new\n")},
	}, rulesBackend{})
	rep, err := v2.Refresh()
	if err != nil || !slices.Equal(rep.Declined, []string{"go.md"}) || !slices.Equal(rep.Written, []string{"new.md"}) || !slices.Equal(rep.Updated, []string{"core.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
	if exists(ruleFile(home, "go.md")) {
		t.Fatal("升级写回了用户删的文件")
	}
	s, err := v2.Status()
	if err != nil || !slices.Contains(s.Files, FileStatus{"go.md", FileDeclined}) {
		t.Fatalf("%+v %v", s, err)
	}
	// 再升级一次：记录里已经没有它了，也不能当成新文件写回
	v3 := New(home, fstest.MapFS{
		"rules/core.md": {Data: []byte("core v3\n")},
		"rules/go.md":   {Data: []byte("go v3\n")},
		"rules/new.md":  {Data: []byte("new\n")},
	}, rulesBackend{})
	rep, err = v3.Refresh()
	if err != nil || len(rep.Written) != 0 || len(rep.Declined) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if exists(ruleFile(home, "go.md")) {
		t.Fatal("第二次升级写回了")
	}
	if mustRead(t, ruleFile(home, "core.md")) != "core v3\n" {
		t.Fatal("别的文件还要跟新版本走")
	}
	// 卸载再恢复也不写回
	if _, err := v3.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := v3.Restore(); err != nil {
		t.Fatal(err)
	}
	if exists(ruleFile(home, "go.md")) || !exists(ruleFile(home, "core.md")) {
		t.Fatal("恢复卸载不该写回用户删的")
	}
	// 用户明确要：补全
	rep, err = v3.Enable()
	if err != nil || !slices.Equal(rep.Written, []string{"go.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
	if mustRead(t, ruleFile(home, "go.md")) != "go v3\n" {
		t.Fatal("env on 要补全")
	}
	if rep, err := v3.Refresh(); err != nil || len(rep.Declined)+len(rep.Written) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}

// 整个目录删了，或记录里的一个都不剩：当作不要了，新版本的新文件也不写；env on 能加回来。
func TestRefreshTreatsAllDeletedAsOff(t *testing.T) {
	for name, remove := range map[string]func(home string) error{
		"删目录": func(home string) error { return os.RemoveAll(filepath.Dir(ruleFile(home, "core.md"))) },
		"逐个删光": func(home string) error {
			for _, n := range []string{"core.md", "go.md"} {
				if err := os.Remove(ruleFile(home, n)); err != nil {
					return err
				}
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, home := newLayer(t, assetsV1())
			if _, err := l.Enable(); err != nil {
				t.Fatal(err)
			}
			if err := remove(home); err != nil {
				t.Fatal(err)
			}
			v2 := New(home, fstest.MapFS{
				"rules/core.md": {Data: []byte("core v2\n")},
				"rules/new.md":  {Data: []byte("new\n")},
			}, rulesBackend{})
			rep, err := v2.Refresh()
			if err != nil || !rep.AllRemoved || len(rep.Written) != 0 {
				t.Fatalf("%+v %v", rep, err)
			}
			if exists(ruleFile(home, "core.md")) || exists(ruleFile(home, "new.md")) {
				t.Fatal("不该写回")
			}
			s, err := v2.Status()
			if err != nil || s.State != StateOff || s.ShouldAutoEnable() {
				t.Fatalf("%+v %v", s, err)
			}
			if rep, err := v2.Refresh(); err != nil || rep.AllRemoved || len(rep.Written) != 0 {
				t.Fatalf("%+v %v", rep, err)
			}
			if rep, err := v2.Enable(); err != nil || !slices.Equal(rep.Written, []string{"core.md", "new.md"}) {
				t.Fatalf("%+v %v", rep, err)
			}
		})
	}
}

func TestRefreshDoesNotAdd(t *testing.T) {
	for _, state := range []string{"没装过", "关了"} {
		t.Run(state, func(t *testing.T) {
			l, home := newLayer(t, assetsV1())
			if state == "关了" {
				if _, err := l.Enable(); err != nil {
					t.Fatal(err)
				}
				if _, err := l.Disable(); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := l.Refresh()
			if err != nil || len(rep.Written)+len(rep.Updated) != 0 {
				t.Fatalf("%+v %v", rep, err)
			}
			if exists(ruleFile(home, "core.md")) {
				t.Fatal("升级不该加")
			}
		})
	}
}

// 卸载再恢复：装回来；env off 关的不恢复；卸载时没装的不恢复。
func TestUninstallRestore(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	write(t, ruleFile(home, "go.md"), "我改过\n")
	rep, err := l.Uninstall()
	if err != nil || !slices.Equal(rep.Kept, []string{"go.md"}) || !slices.Equal(rep.Removed, []string{"core.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
	if s, _ := l.Status(); s.State != StateRemoved {
		t.Fatalf("%+v", s)
	}
	rep, err = l.Restore()
	if err != nil || !slices.Equal(rep.Written, []string{"core.md"}) || !slices.Equal(rep.Kept, []string{"go.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
	if s, _ := l.Status(); s.State != StateOn {
		t.Fatalf("%+v", s)
	}
	if mustRead(t, ruleFile(home, "go.md")) != "我改过\n" {
		t.Fatal("恢复盖掉了用户改的")
	}

	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if rep, err := l.Uninstall(); err != nil || len(rep.Removed) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := l.Restore(); err != nil {
		t.Fatal(err)
	}
	if exists(ruleFile(home, "core.md")) {
		t.Fatal("关了的不该恢复")
	}

	fresh, freshHome := newLayer(t, assetsV1())
	if rep, err := fresh.Uninstall(); err != nil || len(rep.Removed) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := fresh.Restore(); err != nil {
		t.Fatal(err)
	}
	if exists(ruleFile(freshHome, "core.md")) {
		t.Fatal("没装过的不该恢复")
	}
}

// 后端没有规则目录：on 报不支持，别的都安静。记录里已有的文件换后端后还能关掉。
func TestUnsupportedBackend(t *testing.T) {
	home := t.TempDir()
	l := New(home, assetsV1(), fakeBackend{})
	s, err := l.Status()
	if err != nil || s.Supported || s.ShouldAutoEnable() {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := l.Enable(); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := New(home, assetsV1(), rulesBackend{}).Enable(); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Disable()
	if err != nil || !slices.Equal(rep.Removed, []string{"core.md", "go.md"}) {
		t.Fatalf("%+v %v", rep, err)
	}
}

// 记录坏了：当未知，什么都不动。
func TestBadRecordTouchesNothing(t *testing.T) {
	cases := map[string]string{
		"不是 JSON": "{",
		"版本不对":    `{"version":2,"state":"on"}`,
		"目录跑到外面":  `{"version":1,"state":"on","dir":"/etc","files":{"core.md":"x"}}`,
		"文件名带路径":  `{"version":1,"state":"on","dir":"","files":{"../../x":"y"}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			l, home := newLayer(t, assetsV1())
			write(t, RecordPath(home), content)
			write(t, ruleFile(home, "core.md"), "core v1\n")
			for what, call := range map[string]func() (Report, error){"Enable": l.Enable, "Disable": l.Disable, "Refresh": l.Refresh, "Uninstall": l.Uninstall, "Restore": l.Restore} {
				if _, err := call(); err == nil {
					t.Errorf("%s 应报错", what)
				}
			}
			if _, err := l.Status(); err == nil {
				t.Error("Status 应报错")
			}
			if mustRead(t, RecordPath(home)) != content || mustRead(t, ruleFile(home, "core.md")) != "core v1\n" {
				t.Fatal("动了文件")
			}
		})
	}
}

// 祖先目录是软链接：不跟着写到别处。
func TestEnableRefusesSymlinkAncestor(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	target := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".fake")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Enable(); err == nil {
		t.Fatal("应拒绝")
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatalf("写到软链接那边了：%v", entries)
	}
}

// 规则文件是软链接（用户换的）：不写穿，关闭时也不删。
func TestSymlinkFileIsNotOurs(t *testing.T) {
	l, home := newLayer(t, assetsV1())
	target := filepath.Join(home, "mine.md")
	write(t, target, "我的\n")
	if err := os.MkdirAll(filepath.Dir(ruleFile(home, "core.md")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ruleFile(home, "core.md")); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Enable()
	if err != nil || !slices.Contains(rep.Kept, "core.md") {
		t.Fatalf("%+v %v", rep, err)
	}
	if mustRead(t, target) != "我的\n" {
		t.Fatal("写穿了")
	}
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if !exists(ruleFile(home, "core.md")) {
		t.Fatal("删了用户的软链接")
	}
}

// 这个版本没带规则：Enable 报错不留半截，Status 也如实报。
func TestNoBundledRules(t *testing.T) {
	l, home := newLayer(t, fstest.MapFS{"skills/x": {Data: []byte("x")}})
	if _, err := l.Enable(); err == nil {
		t.Fatal("应报错")
	}
	if exists(filepath.Join(home, ".fake", "rules")) {
		t.Fatal("不该建目录")
	}
}

// 规则内容的硬要求：直接读仓库里的 rules/。
func TestBundledRulesContract(t *testing.T) {
	root := filepath.Join("..", "..", "rules")
	core, err := os.ReadFile(filepath.Join(root, "core.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(core) > 4096 {
		t.Errorf("core.md 常驻，不能超过 4KB：%d", len(core))
	}
	// 第一条就要说项目规则和用户规则优先
	first := ""
	for _, line := range strings.Split(string(core), "\n") {
		if strings.HasPrefix(line, "1.") {
			first = line
			break
		}
	}
	if !strings.Contains(first, "优先") || !strings.Contains(first, "项目") || !strings.Contains(first, "用户") {
		t.Errorf("第一条要写明优先级：%q", first)
	}
	if strings.HasPrefix(string(core), "---") {
		t.Error("core.md 常驻，不带 paths")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	glob := regexp.MustCompile(`(?m)^paths:\n(  - "\*\*/\*\.[a-z]+"\n)+---\n`)
	langs := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			t.Errorf("rules/ 只放 .md：%s", e.Name())
			continue
		}
		if e.Name() == "core.md" {
			continue
		}
		langs++
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "---\n") || !glob.Match(b) {
			t.Errorf("%s 要带 paths，用到才加载", e.Name())
		}
		if len(b) > 2048 {
			t.Errorf("%s 太长：%d", e.Name(), len(b))
		}
	}
	if langs < 5 {
		t.Errorf("语言规则只有 %d 个", langs)
	}
}
