package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/testkit"
)

// sgApp：带 projects_root 的实例，git 配置给出身份。
func sgApp(t *testing.T) (*App, string) {
	t.Helper()
	a, root := newRootApp(t)
	os.WriteFile(filepath.Join(a.Home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	return a, root
}

// sgRepo 在 root 下建一个带 BACKLOG 的 git 仓库，提交一次。
func sgRepo(t *testing.T, a *App, root, name, backlog string) bl.Project {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, dir, "add", "-A")
	testkit.GitT(t, a.Home, dir, "commit", "-q", "-m", "init")
	return bl.Project{Name: name, Dir: dir}
}

const sgBacklog = "# 待办\n\n## 待做\n\n## 等用户决定\n\n## 已完成\n\n- [x] 做过的事 —— 合并提交 abc1234\n"

func commits(t *testing.T, a *App, p bl.Project) int {
	t.Helper()
	var n int
	fmt.Sscan(testkit.GitT(t, a.Home, p.Dir, "rev-list", "--count", "HEAD"), &n)
	return n
}

func backlogOf(t *testing.T, p bl.Project) string {
	t.Helper()
	b, err := os.ReadFile(p.BacklogPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// atNoon：之后执行的 agentq 都拨到今天 12:00（夜里不推送，测推送的用例不能看运行时刻）。
func (e *testEnv) atNoon() {
	y, m, d := time.Now().Date()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, d, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
}

func (e *testEnv) claudeRuns() int {
	files, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	return len(files)
}

const sgUsageOK = "You are currently using your subscription to power your Claude Code usage\n\n" +
	"Current session: 10% used\nCurrent week (all models): 20% used\nCurrent week (Fable): 100% used\n"

// sgEnv：接 Telegram、拨到中午、给出 git 身份和宽裕额度的环境，默认不暂停。
func sgEnv(t *testing.T, cfg map[string]any) (*testEnv, *fakeTG) {
	t.Helper()
	full := map[string]any{"paused": false}
	for k, v := range cfg {
		full[k] = v
	}
	e, tg := threadEnv(t, full)
	e.atNoon()
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	e.setUsage(sgUsageOK)
	return e, tg
}

func (e *testEnv) sgProject(name, backlog string) bl.Project {
	e.t.Helper()
	dir := filepath.Join(e.root, name)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	testkit.GitT(e.t, e.home, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644)
	testkit.GitT(e.t, e.home, dir, "add", "-A")
	testkit.GitT(e.t, e.home, dir, "commit", "-q", "-m", "init")
	return bl.Project{Name: name, Dir: dir}
}

// tuningKeys 是写死、不再读 config.json 的键：测试里照老写法放在配置 map 里，由 splitTuning 改走 config.TestTuningEnv。
var tuningKeys = []string{"tick_after_job", "breaker_streak", "breaker_quick", "breaker_cooldown", "breaker_max", "stall_after",
	"night_timeout", "review_timeout", "night_budget_usd", "review_budget_usd", "patrol_budget_usd", "job_budget_usd",
	"verify_timeout", "discuss_wait"}

// splitTuning 把配置 map 里的写死项分出来，返回剩下的配置和 config.TestTuningEnv 的值（没有就是空串）。
func splitTuning(cfg map[string]any) (map[string]any, string) {
	rest, tune := map[string]any{}, map[string]any{}
	for k, v := range cfg {
		if slices.Contains(tuningKeys, k) {
			tune[k] = v
		} else {
			rest[k] = v
		}
	}
	if len(tune) == 0 {
		return rest, ""
	}
	b, err := json.Marshal(tune)
	if err != nil {
		panic(err)
	}
	return rest, string(b)
}
