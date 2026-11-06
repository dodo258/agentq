package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/setup"
	statepkg "agentq/internal/state"
	"agentq/internal/testkit"
)

// 必须显式启用：消耗已授权 Claude 身份的额度，只操作新建临时 HOME/项目。
// 原始宿主记录留在临时目录，公开验证报告仅记录断言与提交号，不复制身份或对话。
func TestRealClaudeInstallationE2E(t *testing.T) {
	if os.Getenv("AGENTQ_REAL_E2E") != "1" {
		t.Skip("opt-in real Claude acceptance; set AGENTQ_REAL_E2E=1")
	}
	base, e := os.MkdirTemp("", "agentq-real-e2e-*")
	if e != nil {
		t.Fatal(e)
	}
	t.Log("private evidence:", base)
	home := filepath.Join(base, "home")
	if e = os.MkdirAll(home, 0700); e != nil {
		t.Fatal(e)
	}
	// 安装测试不读取真实 HOME；网关身份只在显式选择后从调用方现有环境做最小投影。
	if os.Getenv("AGENTQ_REAL_E2E_CONNECTION") == "1" {
		connection := map[string]string{}
		for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
			if value := os.Getenv(key); value != "" {
				connection[key] = value
			}
		}
		settings := filepath.Join(home, ".claude", "settings.json")
		if _, e := os.Lstat(settings); !os.IsNotExist(e) {
			t.Fatal("identity fixture must start empty")
		}
		if e := setup.SaveInstallJSON(settings, map[string]any{"env": connection}); e != nil {
			t.Fatal(e)
		}
		defer os.Remove(settings)
	}
	cli, e := exec.LookPath("claude")
	if e != nil {
		t.Fatal(e)
	}
	env := setup.InstallEnv(home)
	env = append(env, "PATH="+filepath.Join(home, ".local", "bin")+":"+os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"))
	run := func(dir, name string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
		defer cancel()
		c := exec.CommandContext(ctx, name, args...)
		c.Dir = dir
		c.Env = env
		b, e := c.CombinedOutput()
		if e != nil {
			os.WriteFile(filepath.Join(base, "failure.log"), b, 0600)
			t.Fatalf("%s failed: %v; output in private failure.log", filepath.Base(name), e)
		}
		return b
	}
	b := run(home, agentqBin, "setup", "--yes", "--claude", cli)
	os.WriteFile(filepath.Join(base, "install.log"), b, 0600)
	bin := filepath.Join(home, ".local", "bin", "agentq")
	run(home, bin, "doctor")
	cfg, _, e := setup.ReadInstallConfig(home)
	if e != nil {
		t.Fatal(e)
	}
	if !cfg.Paused || cfg.NotifyEnabled || cfg.PermissionMode != "default" {
		t.Fatal("unsafe install defaults")
	}
	// 重复安装真实依赖不复制 vendor/cache/认证。
	run(home, bin, "setup", "--yes")
	cfg.ProjectsRoot = filepath.Join(home, "projects")
	cfg.TickAfterJob = true
	cfg.PermissionMode = "bypassPermissions" // 仅此新建无远程测试项目获得本地开发授权。
	if e = config.Save(filepath.Join(home, ".agentq", "config.json"), cfg); e != nil {
		t.Fatal(e)
	}
	project := filepath.Join(cfg.ProjectsRoot, "fixture")
	os.MkdirAll(project, 0700)
	run(project, "git", "init", "-b", "main")
	run(project, "git", "config", "user.name", "Acceptance Fixture")
	run(project, "git", "config", "user.email", fmt.Sprintf("fixture%c%s", '@', "example.invalid"))
	testkit.WriteInstall(t, filepath.Join(project, ".gitignore"), ".claude/worktrees/\n", 0644)
	testkit.WriteInstall(t, filepath.Join(project, "go.mod"), "module fixture\n\ngo 1.26.3\n", 0644)
	testkit.WriteInstall(t, filepath.Join(project, "greeting_test.go"), "package fixture\nimport \"testing\"\nfunc TestGreeting(t *testing.T){if Greeting()!=\"Hello, agentq.\"{t.Fatal(\"unexpected greeting\")}}\n", 0644)
	testkit.WriteInstall(t, filepath.Join(project, "AGENTS.md"), "# Acceptance fixture\nOnly this disposable repository is authorized. No remotes, notifications, release, global configuration, or external writes. Use installed agent-task via Skill. The engine runs the independent review; no Agent/Workflow calls. Implement Greeting in greeting.go; do not change greeting_test.go. Validation: go test ./.... Worktree ignored by .gitignore. Do not merge; write .agentq/ready.json on the task branch, the engine merges and updates BACKLOG and PROGRESS. All shell commands use this repository or its worktree.\n", 0644)
	testkit.WriteInstall(t, filepath.Join(project, "CLAUDE.md"), "@AGENTS.md\n", 0644)
	testkit.WriteInstall(t, filepath.Join(project, ".agentq", "verify"), "#!/bin/sh\nexec go test ./...\n", 0755)
	for _, name := range []string{"BACKLOG", "PROGRESS", "LESSONS", "DECISIONS"} {
		b, e := os.ReadFile(filepath.Join(home, ".local", "share", "agentq", "templates", name+".md"))
		if e != nil {
			t.Fatal(e)
		}
		p := filepath.Join(project, name+".md")
		if name == "BACKLOG" {
			p = filepath.Join(project, "docs", "BACKLOG.md")
		}
		testkit.WriteInstall(t, p, string(b), 0644)
	}
	run(project, "git", "add", ".")
	run(project, "git", "commit", "-m", "Initialize disposable acceptance fixture")
	prompt := "这是已授权的隔离验收项目，不是真实产品。请先调用 Skill dispatch，再按它派活：在 greeting.go 实现 Greeting() string，返回 Hello, agentq.，验收 go test ./...。不要改 greeting_test.go，不要直接实现，不要等待后台完成；dispatch 写 BACKLOG 后用 agentq run fixture 启动，回答任务ID。只允许此项目及其worktree必要本地操作、验证、一次medium独立check审查、本地合并和记录。没有远程与外部消息授权。"
	b = run(project, cli, "-p", prompt, "--permission-mode", "bypassPermissions", "--output-format", "stream-json", "--verbose", "--model", cfg.Model)
	os.WriteFile(filepath.Join(base, "dispatch.jsonl"), b, 0600)
	if !strings.Contains(string(b), `"name":"Skill"`) || !strings.Contains(string(b), `"skill":"dispatch"`) {
		t.Fatal("host did not invoke dispatch Skill; see private transcript")
	}
	deadline := time.Now().Add(9 * time.Minute)
	var job *Job
	for time.Now().Before(deadline) {
		state, e := statepkg.Load(filepath.Join(home, ".agentq", "state.json"))
		if e == nil && len(state.Jobs) > 0 {
			job = state.Jobs[len(state.Jobs)-1]
			// 任务只做到分支可合并，引擎过门之后才算走完
			if job != nil && job.Status != statepkg.StatusRunning && (job.Gate == nil || (job.Gate.State != statepkg.GateQueued && job.Gate.State != statepkg.GateRunning)) {
				break
			}
		}
		time.Sleep(time.Second)
	}
	if job == nil || job.Status != statepkg.StatusDone || job.Gate == nil || job.Gate.State != statepkg.GateMerged {
		if job != nil {
			t.Log("job", job.ID, job.Status, job.Gate)
		}
		t.Fatal("real pipeline did not finish successfully; private evidence retained")
	}
	// 等最后写出的汇总，不把 state 的中间更新当最终结果。
	report := filepath.Join(home, ".claude", "nightly", time.Now().Format("2006-01-02")+"-agentq.md")
	for time.Now().Before(deadline) {
		b, e = os.ReadFile(report)
		if e == nil && strings.Contains(string(b), job.ID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, e = os.ReadFile(report)
	if e != nil || !strings.Contains(string(b), job.ID) {
		t.Fatal("final local report missing")
	}
	run(project, "go", "test", "./...")
	backlog, _ := os.ReadFile(filepath.Join(project, "docs", "BACKLOG.md"))
	items := bl.ParseBacklog(string(backlog))
	if len(items) != 0 || !strings.Contains(string(backlog), "合并提交") {
		t.Fatal("BACKLOG not closed with merge evidence")
	}
	if len(run(project, "git", "status", "--porcelain")) != 0 {
		t.Fatal("fixture main not clean")
	}
	if branch := strings.TrimSpace(string(run(project, "git", "branch", "--show-current"))); branch != "main" {
		t.Fatal("fixture not on main")
	}
	// 核对独立 check 在真实宿主会话被发现与调用，而非仅声称审查。
	var transcript strings.Builder
	reviewCalls := map[string]bool{}
	reviewerCheck, reviewerTest := false, false
	e = filepath.WalkDir(filepath.Join(home, ".claude", "projects"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		transcript.Write(b)
		reviewer := strings.Contains(string(b), "审查门的独立审查进程")
		for _, line := range strings.Split(string(b), "\n") {
			var event struct {
				Message struct {
					Content []struct {
						Type, ID, Name string
						Input          struct {
							Skill, Command, Effort string
							Background             *bool `json:"run_in_background"`
						}
					}
				}
			}
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			for _, call := range event.Message.Content {
				if call.Type != "tool_use" {
					continue
				}
				if call.Name == "Agent" || call.Name == "Task" {
					reviewCalls[call.ID] = true
				}
				if reviewer {
					reviewerCheck = reviewerCheck || call.Name == "Skill" && call.Input.Skill == "check"
					reviewerTest = reviewerTest || call.Name == "Bash" && strings.Contains(call.Input.Command, "go test")
				}
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(transcript.String(), `"skill":"agent-task"`) || !reviewerCheck || !reviewerTest || len(reviewCalls) != 0 {
		t.Fatal("real host did not complete one non-recursive check with personal validation; private transcript retained")
	}
	if _, e = os.Stat(filepath.Join(home, ".agentq", "secrets", "telegram.env")); !os.IsNotExist(e) {
		t.Fatal("unexpected Telegram configuration")
	}
	sha := strings.TrimSpace(string(run(project, "git", "rev-parse", "HEAD")))
	summary := map[string]any{"installed": true, "telegram": false, "dispatch": true, "agent_task": true, "independent_check": true, "validation": "go test ./...", "main_clean": true, "head": sha, "job_status": job.Status, "local_report": true}
	data, _ := json.MarshalIndent(summary, "", "  ")
	os.WriteFile(filepath.Join(base, "summary.json"), append(data, '\n'), 0600)
	fmt.Fprintln(os.Stdout, string(data))
}
