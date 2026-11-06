package envlayer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agentq/internal/backend"
)

// stubBase 是接口要的方法，什么也不做。
type stubBase struct{}

func (stubBase) Name() string                      { return "blk" }
func (stubBase) Caps() backend.Caps                { return backend.Caps{} }
func (stubBase) EnvKeep() []string                 { return nil }
func (stubBase) Auth(context.Context) backend.Auth { return backend.Auth{} }
func (stubBase) Probe(context.Context, backend.ProbeEnv) (backend.Probe, error) {
	return backend.Probe{}, nil
}
func (stubBase) Build(backend.Request) (backend.Invocation, []backend.Degradation, error) {
	return backend.Invocation{}, nil, nil
}
func (stubBase) Parse(backend.JobRef, backend.Exit) backend.Result { return backend.Result{} }
func (stubBase) LastActivity(backend.JobRef) (time.Time, bool)     { return time.Time{}, false }
func (stubBase) CanResume(string) bool                             { return false }
func (stubBase) RecordedModel([]string) string                     { return "" }

// blockBackend 只有用户级说明文件，没有规则目录（Codex 的样子）。
type blockBackend struct {
	stubBase
	file, shadow string
}

func (b blockBackend) RulesFile(string) (string, string) { return b.file, b.shadow }

func blockAssets(core string) fstest.MapFS {
	return fstest.MapFS{
		"rules/core.md": {Data: []byte(core)},
		"rules/go.md":   {Data: []byte("---\npaths:\n  - \"**/*.go\"\n---\n# Go\n")},
	}
}

func newBlockLayer(t *testing.T, core string) (*Layer, string, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	return New(home, blockAssets(core), blockBackend{file: filepath.Join(dir, "AGENTS.md"), shadow: filepath.Join(dir, "AGENTS.override.md")}), home, filepath.Join(dir, "AGENTS.md")
}

func writeBlock(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readBlock(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 已有内容逐字节保留：装上、卸掉回到原样，换行、CRLF、结尾没换行都一样。
func TestBlockPreservesBytes(t *testing.T) {
	for name, original := range map[string]string{
		"有换行结尾": "我的规则\n第二行\n",
		"没换行结尾": "我的规则",
		"CRLF":  "line1\r\nline2\r\n",
		"空白":    "\n\n",
		"多个空行":  "a\n\n\n",
		"中文标记样": "<!-- 我自己的注释 -->\n",
	} {
		t.Run(name, func(t *testing.T) {
			l, _, file := newBlockLayer(t, "# core\nrule\n")
			writeBlock(t, file, original, 0o640)
			rep, err := l.Enable()
			if err != nil || len(rep.Written) != 1 {
				t.Fatalf("%+v %v", rep, err)
			}
			got := readBlock(t, file)
			if !strings.HasPrefix(got, original) {
				t.Fatalf("开头不是原来的内容：%q", got)
			}
			if !strings.Contains(got, blockBegin+"\n# core\nrule\n"+blockEnd+"\n") {
				t.Fatalf("%q", got)
			}
			if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
				t.Errorf("权限变了：%v", fi.Mode())
			}
			rep, err = l.Disable()
			if err != nil || len(rep.Removed) != 1 {
				t.Fatalf("%+v %v", rep, err)
			}
			if got := readBlock(t, file); got != original {
				t.Fatalf("卸掉没回到原样：%q，应为 %q", got, original)
			}
			if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
				t.Errorf("权限变了：%v", fi.Mode())
			}
		})
	}
}

func TestBlockBacksUpBeforeChange(t *testing.T) {
	l, home, file := newBlockLayer(t, "# core\n")
	writeBlock(t, file, "我的规则\n", 0o600)
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local", "share", "agentq", "env-layer-backups")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("%v %v", ents, err)
	}
	if got := readBlock(t, filepath.Join(dir, ents[0].Name())); got != "我的规则\n" {
		t.Errorf("备份内容 %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, ents[0].Name())); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("备份带用户内容，只能自己读：%v", fi.Mode())
	}
}

// 文件是 agentq 建的：卸掉就删，不留空文件。用户后来加了内容的留着。
func TestBlockCreatedFile(t *testing.T) {
	l, _, file := newBlockLayer(t, "# core\n")
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(readBlock(t, file), blockBegin) {
		t.Fatal(readBlock(t, file))
	}
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("应该删掉：%v", err)
	}
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	writeBlock(t, file, readBlock(t, file)+"后来加的\n", 0o644)
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if got := readBlock(t, file); got != "后来加的\n" {
		t.Errorf("%q", got)
	}
}

// 用户在块后面、前面加的内容都留着。
func TestBlockKeepsLaterEdits(t *testing.T) {
	l, _, file := newBlockLayer(t, "# core\n")
	writeBlock(t, file, "开头\n", 0o644)
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	writeBlock(t, file, "最前面\n"+readBlock(t, file)+"结尾\n", 0o644)
	if _, err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if got := readBlock(t, file); got != "最前面\n开头\n结尾\n" {
		t.Errorf("%q", got)
	}
}

func TestBlockRefresh(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".codex", "AGENTS.md")
	be := blockBackend{file: file}
	writeBlock(t, file, "我的\n", 0o644)
	if _, err := New(home, blockAssets("# v1\n"), be).Enable(); err != nil {
		t.Fatal(err)
	}
	// 新版本：没改过的跟着更新，别的字节不动
	rep, err := New(home, blockAssets("# v2\n"), be).Refresh()
	if err != nil || len(rep.Updated) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if got := readBlock(t, file); !strings.HasPrefix(got, "我的\n\n"+blockBegin) || !strings.Contains(got, "# v2\n") || strings.Contains(got, "# v1") {
		t.Fatalf("%q", got)
	}
	// 用户改了块：升级不动，关闭也不动，报告出来
	edited := strings.Replace(readBlock(t, file), "# v2", "# 我改的", 1)
	writeBlock(t, file, edited, 0o644)
	rep, err = New(home, blockAssets("# v3\n"), be).Refresh()
	if err != nil || len(rep.Kept) != 1 || readBlock(t, file) != edited {
		t.Fatalf("%+v %v %q", rep, err, readBlock(t, file))
	}
	st, err := New(home, blockAssets("# v3\n"), be).Status()
	if err != nil || len(st.Files) != 1 || st.Files[0].State != FileModified {
		t.Fatalf("%+v %v", st, err)
	}
	rep, err = New(home, blockAssets("# v3\n"), be).Disable()
	if err != nil || len(rep.Kept) != 1 || readBlock(t, file) != edited {
		t.Fatalf("%+v %v", rep, err)
	}
}

// 用户把块删了：升级不写回。env on 才补上。
func TestBlockDeletedByUser(t *testing.T) {
	l, _, file := newBlockLayer(t, "# core\n")
	writeBlock(t, file, "我的\n", 0o644)
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	writeBlock(t, file, "我的\n", 0o644)
	rep, err := l.Refresh()
	if err != nil || len(rep.Declined) != 1 || readBlock(t, file) != "我的\n" {
		t.Fatalf("%+v %v", rep, err)
	}
	rep, err = l.Refresh() // 再升级一次也不写回
	if err != nil || len(rep.Written) != 0 || readBlock(t, file) != "我的\n" {
		t.Fatalf("%+v %v", rep, err)
	}
	st, _ := l.Status()
	if len(st.Files) != 1 || st.Files[0].State != FileDeclined {
		t.Fatalf("%+v", st)
	}
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readBlock(t, file), blockBegin) {
		t.Error("env on 要补上")
	}
	// 整个文件都删了：当作不要了
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	rep, err = l.Refresh()
	if err != nil || !rep.AllRemoved {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("不写回")
	}
}

// 标记残缺或重复：判断不了哪段是自己的，一个字节都不动。
func TestBlockBrokenMarkersUntouched(t *testing.T) {
	for name, body := range map[string]string{
		"只有开头": "x\n" + blockBegin + "\ny\n",
		"只有结尾": "x\n" + blockEnd + "\n",
		"反了":   blockEnd + "\n" + blockBegin + "\n",
		"两段":   blockBegin + "\n" + blockEnd + "\n" + blockBegin + "\n" + blockEnd + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			l, _, file := newBlockLayer(t, "# core\n")
			writeBlock(t, file, body, 0o644)
			rep, err := l.Enable()
			if err != nil || len(rep.Kept) != 1 || readBlock(t, file) != body {
				t.Fatalf("%+v %v %q", rep, err, readBlock(t, file))
			}
			rep, err = l.Disable()
			if err != nil || readBlock(t, file) != body {
				t.Fatalf("%+v %v", rep, err)
			}
		})
	}
}

// 软链接是用户的安排，不跟着写。
func TestBlockSymlinkRefused(t *testing.T) {
	l, home, file := newBlockLayer(t, "# core\n")
	target := filepath.Join(home, "elsewhere.md")
	writeBlock(t, target, "别处\n", 0o644)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Enable(); err == nil {
		t.Error("应该报错")
	}
	if readBlock(t, target) != "别处\n" {
		t.Error("动了别处的文件")
	}
}

func TestBlockUninstallRestore(t *testing.T) {
	l, _, file := newBlockLayer(t, "# core\n")
	writeBlock(t, file, "我的", 0o644)
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if got := readBlock(t, file); got != "我的" {
		t.Fatalf("%q", got)
	}
	if st, _ := l.Status(); st.State != StateRemoved {
		t.Fatalf("%+v", st)
	}
	rep, err := l.Restore()
	if err != nil || len(rep.Written) != 1 || !strings.HasPrefix(readBlock(t, file), "我的\n\n"+blockBegin) {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if got := readBlock(t, file); got != "我的" {
		t.Fatalf("恢复再卸载也回到原样：%q", got)
	}
}

func TestBlockStatusAndAutoEnable(t *testing.T) {
	l, _, file := newBlockLayer(t, "# core\n")
	st, err := l.Status()
	if err != nil || !st.Supported || st.Dir != file || !st.ShouldAutoEnable() {
		t.Fatalf("%+v %v", st, err)
	}
	writeBlock(t, file, "我的\n", 0o644)
	st, _ = l.Status()
	if st.ShouldAutoEnable() || len(st.UserRules) != 1 {
		t.Fatalf("用户有自己的说明就不自动装：%+v", st)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	// 只有自己的块不算用户的规则
	if _, err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	st, _ = l.Status()
	if len(st.UserRules) != 0 || st.State != StateOn || st.Files[0].State != FileIntact {
		t.Fatalf("%+v", st)
	}
}

// AGENTS.override.md 存在时 Codex 不读 AGENTS.md：装了不生效，要说。
func TestBlockShadowReported(t *testing.T) {
	l, _, file := newBlockLayer(t, "# core\n")
	shadow := filepath.Join(filepath.Dir(file), "AGENTS.override.md")
	writeBlock(t, shadow, "优先的\n", 0o644)
	st, err := l.Status()
	if err != nil || st.Shadow != shadow || st.ShouldAutoEnable() {
		t.Fatalf("%+v %v", st, err)
	}
	rep, err := l.Enable()
	if err != nil || rep.Shadow != shadow {
		t.Fatalf("%+v %v", rep, err)
	}
	if readBlock(t, shadow) != "优先的\n" {
		t.Error("动了 override")
	}
}

// 从目录方式的后端切过来：记录里还有文件时不覆盖记录，免得目录里的文件没人认。
func TestBlockRefusesWhenInstalledElsewhere(t *testing.T) {
	home := t.TempDir()
	assets := blockAssets("# core\n")
	if _, err := New(home, assets, dirRulesBackend{}).Enable(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".codex", "AGENTS.md")
	_, err := New(home, assets, blockBackend{file: file}).Enable()
	if err == nil || !strings.Contains(err.Error(), "env off") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("不该写")
	}
	// 关掉靠记录，不靠当前后端
	rep, err := New(home, assets, blockBackend{file: file}).Disable()
	if err != nil || len(rep.Removed) == 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := New(home, assets, blockBackend{file: file}).Enable(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(home, assets, dirRulesBackend{}).Enable(); err == nil {
		t.Error("反过来也不覆盖")
	}
}

type dirRulesBackend struct{ stubBase }

func (dirRulesBackend) RulesPaths(home string) (string, []string) {
	return filepath.Join(home, ".dir", "rules"), nil
}
