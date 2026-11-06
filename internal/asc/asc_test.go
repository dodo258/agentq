package asc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backlog"
)

func TestParseASCEnv(t *testing.T) {
	c, err := parseASCEnv("# 注释\nexport ASC_KEY_ID=\"AB12\"\nASC_ISSUER_ID='is-1' \nASC_KEY_PATH=$HOME/keys/a.p8 # 行尾注释\nOTHER=x\n", "/h")
	if err != nil || c.KeyID != "AB12" || c.IssuerID != "is-1" || c.KeyPath != "/h/keys/a.p8" {
		t.Fatalf("%+v %v", c, err)
	}
	for in, want := range map[string]string{"~/a.p8": "/h/a.p8", "${HOME}/a.p8": "/h/a.p8", "/abs/a.p8": "/abs/a.p8", "a.p8": "/h/.appstoreconnect/a.p8"} {
		c, err := parseASCEnv("ASC_KEY_ID=k\nASC_ISSUER_ID=i\nASC_KEY_PATH="+in+"\n", "/h")
		if err != nil || c.KeyPath != want {
			t.Errorf("%s → %q %v", in, c.KeyPath, err)
		}
	}
	// 缺键：报键名，不带值
	if _, err := parseASCEnv("ASC_KEY_ID=SECRETVALUE\n", "/h"); err == nil || !strings.Contains(err.Error(), "ASC_ISSUER_ID") || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("缺键：%v", err)
	}
	// 不执行 shell：命令替换、变量都拒绝，不会被当路径用
	for _, in := range []string{"$(touch /tmp/x)", "`id`", "$OTHER/a.p8", "~/$(id).p8"} {
		if c, err := parseASCEnv("ASC_KEY_ID=k\nASC_ISSUER_ID=i\nASC_KEY_PATH="+in+"\n", "/h"); err == nil {
			t.Errorf("%s 应拒绝，得到 %+v", in, c)
		}
	}
}

func TestBundleIDsIn(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	write("App.xcodeproj/project.pbxproj", `PRODUCT_BUNDLE_IDENTIFIER = test.example.app;
		PRODUCT_BUNDLE_IDENTIFIER = "test.example.app.widget";
		PRODUCT_BUNDLE_IDENTIFIER = "$(inherited)";
		PRODUCT_BUNDLE_IDENTIFIER = test.example.$(SUFFIX);`)
	write("ios/project.yml", "settings:\n  PRODUCT_BUNDLE_IDENTIFIER: test.example.yml\n")
	write(".claude/worktrees/x/App.xcodeproj/project.pbxproj", "PRODUCT_BUNDLE_IDENTIFIER = test.example.worktree;")
	write("build/App.xcodeproj/project.pbxproj", "PRODUCT_BUNDLE_IDENTIFIER = test.example.built;")
	write("a/b/c/d/project.yml", "PRODUCT_BUNDLE_IDENTIFIER: test.example.deep\n")
	write("README.md", "PRODUCT_BUNDLE_IDENTIFIER = test.example.readme;")
	got := strings.Join(bundleIDsIn(dir), ",")
	if got != "test.example.app,test.example.app.widget,test.example.yml" {
		t.Fatalf("%s", got)
	}
	if ids := bundleIDsIn(filepath.Join(dir, "nonexistent")); len(ids) != 0 {
		t.Fatalf("不存在的目录：%v", ids)
	}
}

func TestMapApps(t *testing.T) {
	root := t.TempDir()
	mk := func(name, id string) backlog.Project {
		dir := filepath.Join(root, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "project.yml"), []byte("PRODUCT_BUNDLE_IDENTIFIER: "+id+"\n"), 0o644)
		return backlog.Project{Name: name, Dir: dir}
	}
	ps := []backlog.Project{mk("a", "x.a"), mk("b", "x.dup"), mk("c", "x.dup")}
	m, un := MapApps([]App{{ID: "1", Name: "A", BundleID: "x.a"}, {ID: "2", Name: "Dup", BundleID: "x.dup"}, {ID: "3", Name: "None", BundleID: "x.none"}, {ID: "4", Name: "Pre", BundleID: "x"}}, ps)
	if len(m) != 1 || m[0].Project.Name != "a" || m[0].ID != "1" {
		t.Fatalf("%+v", m)
	}
	if len(un) != 3 || !strings.Contains(un[0], "说不清") || !strings.Contains(un[1], "没有工程") {
		t.Fatalf("%v", un)
	}
}
