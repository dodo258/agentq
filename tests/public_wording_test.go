package tests

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// publicFiles 是公开候选里会有的文件：整个仓库去掉 config/clean-source.json 的 private_paths。文档、脚本、配置都算，不只代码。
func publicFiles(t *testing.T) []string {
	t.Helper()
	var manifest struct {
		PrivatePaths []string `json:"private_paths"`
	}
	if err := json.Unmarshal(testkit.ReadBytes(t, filepath.Join("config", "clean-source.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	private := func(p string) bool {
		for _, x := range manifest.PrivatePaths {
			if p == strings.TrimSuffix(x, "/") || (strings.HasSuffix(x, "/") && strings.HasPrefix(p, x)) {
				return true
			}
		}
		return false
	}
	var out []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		p = filepath.ToSlash(p)
		if p == ".git" || p == "dist" || private(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// 面向公开的字样：整个公开候选（代码、技能、脚本、提示词、文档、配置）都不用旧称呼。这个文件自己要写旧写法做对照，不扫。
func TestNoOwnerWordingInPublicSources(t *testing.T) {
	files := publicFiles(t)
	if !slices.Contains(files, "AGENTS.md") || !slices.Contains(files, "docs/reference/scheduling.md") || !slices.Contains(files, "internal/backlog/backlog.go") {
		t.Fatalf("公开文件列表不对：%d 个", len(files))
	}
	for _, path := range files {
		if path == "tests/public_wording_test.go" {
			continue
		}
		for i, line := range strings.Split(testkit.ReadFile(t, path), "\n") {
			if strings.Contains(line, "主人") {
				t.Errorf("%s:%d 还有旧称呼：%s", path, i+1, textfmt.TruncRunes(strings.TrimSpace(line), 60))
			}
		}
	}
}

// 公开候选里不带指向维护者记录的引用：这些文件对外不存在。普通项目自己的 docs/specs、docs/BACKLOG.md 是另一回事，查的是带日期的具体文件和维护者才有的文件。
func TestNoPrivateRecordRefsInPublicSources(t *testing.T) {
	ref := regexp.MustCompile(`docs/(specs|research)/20\d\d-|docs/verification/|docs/VISION|docs/ASSETS|docs/audit-|（方案 [P0-9]`)
	for _, path := range publicFiles(t) {
		if path == "tests/public_wording_test.go" || path == "config/clean-source.json" {
			continue
		}
		for i, line := range strings.Split(testkit.ReadFile(t, path), "\n") {
			if m := ref.FindString(line); m != "" {
				t.Errorf("%s:%d 引用了不公开的 %s", path, i+1, m)
			}
		}
	}
}

// docRow 取 Markdown 表格里第一列含 key 的那一行的其余内容。
func docRow(t *testing.T, path, key string) string {
	t.Helper()
	for _, line := range strings.Split(testkit.ReadFile(t, path), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) >= 3 && strings.Contains(cells[1], key) {
			return strings.Join(cells[2:], "|")
		}
	}
	t.Fatalf("%s 里没有「%s」这一行", path, key)
	return ""
}

// 文档里关于权限默认值的说法要和代码一致：期望值取自 defaultConfig 和 fresh，不写死。
func TestSecurityDocsMatchDefaults(t *testing.T) {
	fresh, legacy := config.Default().Fresh(), config.Default()
	wantFresh := func(path, key string) {
		t.Helper()
		row := docRow(t, path, key)
		for _, want := range []string{fresh.PermissionMode, "paused=" + strconv.FormatBool(fresh.Paused)} {
			if !strings.Contains(row, want) {
				t.Errorf("%s「%s」应写 %s：%s", path, key, want, row)
			}
		}
		for _, wrong := range []string{legacy.PermissionMode, "paused=" + strconv.FormatBool(legacy.Paused)} {
			if strings.Contains(row, wrong) {
				t.Errorf("%s「%s」不该写 %s：%s", path, key, wrong, row)
			}
		}
	}
	wantFresh("SECURITY.md", "配置文件不存在")
	wantFresh(filepath.Join("docs", "reference", "config.md"), "配置文件不存在")
	wantFresh(filepath.Join("docs", "reference", "config.md"), "安装器新建配置")
	if row := docRow(t, "SECURITY.md", "安装器新建配置"); !strings.Contains(row, fresh.PermissionMode) || strings.Contains(row, legacy.PermissionMode) {
		t.Errorf("SECURITY.md 安装器新建配置：%s", row)
	}
	if row := docRow(t, "SECURITY.md", "配置里缺这个键"); !strings.Contains(row, legacy.PermissionMode) {
		t.Errorf("SECURITY.md 缺键：%s", row)
	}
	if row := docRow(t, filepath.Join("docs", "reference", "config.md"), "`permission_mode`"); !strings.Contains(row, legacy.PermissionMode) {
		t.Errorf("config.md 缺键默认：%s", row)
	}
}

// 老项目的 BACKLOG 不用改：旧节标题和卡住、进行中、等条件标记照常认。
const legacyBacklog = `# 待办

## 待做

- [ ] 能接的活 —— 细节
- [ ] 认领了的活（进行中：agent/a 2026-10-10 10:00）
- [ ] 做不下去的活（卡住：agent/b 两次没过）
- [ ] 正文提到卡住这个词的活 —— 验收：不再卡住也算

## 等主人拍板

- [ ] 选哪个方案 —— 要定（选项：A 做法一｜B 做法二；推荐 A）
- [ ] 还要等的 —— 说明（等条件：前面都合并）

## 以后再说

## 已完成

- [x] 旧活 —— 合并提交 abc1234
`

func TestLegacyBacklogStillParses(t *testing.T) {
	items := backlog.ParseBacklog(legacyBacklog)
	if len(items) != 4 {
		t.Fatalf("待做应有 4 条：%+v", items)
	}
	if !items[0].Eligible() || !items[1].InProgress || !items[2].Stuck || items[3].Stuck {
		t.Fatalf("标记解析不对：%+v", items)
	}

	root := t.TempDir()
	dir := filepath.Join(root, "old-project")
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), legacyBacklog, 0o644)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	decs, fails := notify.ScanDecisions(root)
	if len(fails) != 0 || len(decs) != 2 {
		t.Fatalf("旧节标题里的决定项应认出来：%+v %v", decs, fails)
	}
	if ask := notify.Askable(decs); len(ask) != 1 || ask[0].Title != "选哪个方案" {
		t.Fatalf("等条件的不该问：%+v", ask)
	}

	moved, ok := chat.MoveDecision(legacyBacklog, "选哪个方案", "选 A")
	if !ok {
		t.Fatal("旧节标题里的条目应能移回待做")
	}
	if first := backlog.ParseBacklog(moved)[0]; first.Title != "选哪个方案" || !strings.Contains(first.Text, "选 A") {
		t.Fatalf("应移到待做第一条：%+v", first)
	}

	sug := suggest.InsertSuggestions(legacyBacklog, []string{"- [ ] 一条建议"})
	if iOld, iSug, iLater := strings.Index(sug, "## 等主人拍板"), strings.Index(sug, "## 建议"), strings.Index(sug, "## 以后再说"); !(iOld < iSug && iSug < iLater) {
		t.Fatalf("建议一节应放在旧节标题之后：\n%s", sug)
	}
	if !backlog.IsDecisionSection("等主人拍板") || !backlog.IsDecisionSection("等用户决定") || backlog.IsDecisionSection("待做") {
		t.Fatal("isDecisionSection 不对")
	}
	if backlog.DoneMergeCommit(legacyBacklog, "旧活") != "abc1234" {
		t.Fatal("已完成的合并提交应认出来")
	}
}
