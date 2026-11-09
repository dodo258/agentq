package gate_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/state"
)

// Classify 只看文件、新增行和条目标记。
func TestClassify(t *testing.T) {
	many := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("internal/x/f%d.go", i))
		}
		return out
	}
	rules := gate.RiskRules{Irreversible: []string{"secrets/**"}, Broad: []string{"internal/api/**"}}
	cases := []struct {
		name    string
		files   []string
		deleted []string
		added   map[string]string
		item    string
		want    gate.Risk
		reason  string
	}{
		{name: "普通改动", files: []string{"internal/x/a.go", "internal/x/a_test.go"}, want: gate.RiskLocal},
		{name: "迁移目录", files: []string{"db/migrations/001_init.go"}, want: gate.RiskIrreversible, reason: "**/migrations/**"},
		{name: "顶层 migrate 目录", files: []string{"migrate/up.go"}, want: gate.RiskIrreversible, reason: "**/migrate/**"},
		{name: "sql 文件", files: []string{"schema.sql"}, want: gate.RiskIrreversible, reason: "*.sql"},
		{name: "部署目录", files: []string{"ops/deploy/prod.yaml"}, want: gate.RiskIrreversible, reason: "**/deploy/**"},
		{name: "目录名只是前缀不算", files: []string{"deployment.md", "internal/migrations.go"}, want: gate.RiskLocal},
		{name: "新增行 DROP TABLE（小写也算）", files: []string{"internal/store/s.go"}, added: map[string]string{"internal/store/s.go": `db.Exec("drop table users")`}, want: gate.RiskIrreversible, reason: "DROP TABLE"},
		{name: "新增行 DROP COLUMN", files: []string{"a.py"}, added: map[string]string{"a.py": "ALTER TABLE t DROP COLUMN c"}, want: gate.RiskIrreversible, reason: "DROP COLUMN"},
		{name: "新增行 DELETE FROM", files: []string{"a.py"}, added: map[string]string{"a.py": "DELETE FROM users WHERE 1"}, want: gate.RiskIrreversible, reason: "DELETE FROM"},
		{name: "新增行 TRUNCATE TABLE", files: []string{"a.py"}, added: map[string]string{"a.py": "TRUNCATE TABLE logs;"}, want: gate.RiskIrreversible, reason: "TRUNCATE"},
		{name: "md 里写 DROP TABLE 不算", files: []string{"README.md"}, added: map[string]string{"README.md": "不要手动 DROP TABLE users"}, want: gate.RiskLocal},
		{name: "txt 里写 DELETE FROM 不算", files: []string{"notes.txt"}, added: map[string]string{"notes.txt": "DELETE FROM t"}, want: gate.RiskLocal},
		{name: "docs 下的文件不算", files: []string{"docs/guide.html"}, added: map[string]string{"docs/guide.html": "DROP TABLE t"}, want: gate.RiskLocal},
		{name: "_test.go 里写 DROP TABLE 不算", files: []string{"internal/store/s_test.go"}, added: map[string]string{"internal/store/s_test.go": `db.Exec("DROP TABLE users")`}, want: gate.RiskLocal},
		{name: "tests 目录不算", files: []string{"tests/e2e.py"}, added: map[string]string{"tests/e2e.py": "TRUNCATE TABLE logs"}, want: gate.RiskLocal},
		{name: "test 目录不算", files: []string{"pkg/test/helper.go"}, added: map[string]string{"pkg/test/helper.go": "DROP COLUMN c"}, want: gate.RiskLocal},
		{name: "testdata 目录不算", files: []string{"internal/x/testdata/in.go"}, added: map[string]string{"internal/x/testdata/in.go": "DROP TABLE t"}, want: gate.RiskLocal},
		{name: ".spec. 和 .test. 文件不算", files: []string{"web/a.spec.ts", "web/b.test.js"}, added: map[string]string{"web/a.spec.ts": "DROP TABLE t", "web/b.test.js": "DELETE FROM t"}, want: gate.RiskLocal},
		{name: "产品代码照样算（同一次也改了测试）", files: []string{"internal/store/s.go", "internal/store/s_test.go"}, added: map[string]string{"internal/store/s.go": `db.Exec("DROP TABLE users")`, "internal/store/s_test.go": "DROP TABLE x"}, want: gate.RiskIrreversible, reason: "internal/store/s.go（DROP TABLE）"},
		{name: "测试目录里的 sql 文件按路径规则照样算", files: []string{"tests/fixtures/seed.sql"}, want: gate.RiskIrreversible, reason: "*.sql"},
		{name: "代码里的 Truncate( 不算", files: []string{"a.go"}, added: map[string]string{"a.go": "f.Truncate(0)\nos.Truncate(p, 0)"}, want: gate.RiskLocal},
		{name: "条目标了（难撤回）", files: []string{"a.go"}, item: "（难撤回）清理旧数据 —— 删掉过期记录", want: gate.RiskIrreversible, reason: "（难撤回）"},
		{name: "条目标题结尾标（难撤回）", files: []string{"a.go"}, item: "清理旧数据（难撤回） —— 删掉过期记录", want: gate.RiskIrreversible, reason: "（难撤回）"},
		{name: "正文顺带提到难撤回不算", files: []string{"a.go"}, item: "改提示 —— 这事不算难撤回，照常合", want: gate.RiskLocal},
		{name: "正文中间的括号也不算", files: []string{"a.go"}, item: "改提示 —— 文档里解释（难撤回）是什么意思，再补例子", want: gate.RiskLocal},
		{name: "项目规则难撤回", files: []string{"secrets/key.txt"}, want: gate.RiskIrreversible, reason: "secrets/**"},
		{name: "依赖清单", files: []string{"go.mod", "go.sum"}, want: gate.RiskBroad, reason: "go.mod"},
		{name: "子目录的 package.json", files: []string{"web/package.json"}, want: gate.RiskBroad, reason: "web/package.json"},
		{name: "接口定义", files: []string{"api/openapi.yaml"}, want: gate.RiskBroad, reason: "接口"},
		{name: "proto", files: []string{"proto/a.proto"}, want: gate.RiskBroad, reason: "a.proto"},
		{name: "项目规则影响面广", files: []string{"internal/api/h.go"}, want: gate.RiskBroad, reason: "internal/api/**"},
		{name: "删了文件", files: []string{"a.go", "old.go"}, deleted: []string{"old.go"}, want: gate.RiskBroad, reason: "删了文件：old.go"},
		{name: "15 个文件不算", files: many(15), want: gate.RiskLocal},
		{name: "16 个文件", files: many(16), want: gate.RiskBroad, reason: "16 个文件"},
		{name: "3 个顶层目录加根目录文件不算", files: []string{"a/x.go", "b/x.go", "c/x.go", "README.md", "CHANGELOG.md"}, want: gate.RiskLocal},
		{name: "4 个顶层目录", files: []string{"a/x.go", "b/x.go", "c/x.go", "d/x.go"}, want: gate.RiskBroad, reason: "跨 4 个顶层目录"},
		{name: "难撤回压过影响面广", files: []string{"go.mod", "db/migrations/1.sql"}, want: gate.RiskIrreversible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reasons := gate.Classify(c.files, c.deleted, func(p string) string { return c.added[p] }, c.item, rules)
			if got != c.want {
				t.Fatalf("要 %s，得到 %s：%v", c.want, got, reasons)
			}
			if c.want == gate.RiskLocal && len(reasons) > 0 {
				t.Errorf("局部不该有依据：%v", reasons)
			}
			if c.reason != "" && !strings.Contains(strings.Join(reasons, "；"), c.reason) {
				t.Errorf("依据要写 %s：%v", c.reason, reasons)
			}
		})
	}
}

func TestParseRiskRules(t *testing.T) {
	if r, err := gate.ParseRiskRules([]byte(`{"irreversible":["data/**"],"broad":["*.proto2"]}`)); err != nil || len(r.Irreversible) != 1 || len(r.Broad) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{`{`, `{"irreversible":["["]}`, `{"broad":[""]}`} {
		if _, err := gate.ParseRiskRules([]byte(bad)); err == nil {
			t.Errorf("%s 要报错", bad)
		}
	}
}

// commitMain 在主目录的 main 上提交文件。
func (f *fix) commitMain(files map[string]string) {
	f.t.Helper()
	for name, text := range files {
		p := filepath.Join(f.dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil {
			f.t.Fatal(err)
		}
		f.git("add", name)
	}
	f.git("commit", "-q", "-m", "main 上的配置")
}

// 局部：照现有门合并，报告不多一行。
func TestRiskLocalMerges(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "merged" || res.Risk == nil || res.Risk.Level != gate.RiskLocal {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := f.a.CmdGate([]string{"report", j.ID}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.out.String(), "改错的代价") {
		t.Errorf("局部不写：%s", f.out.String())
	}
}

// 难撤回：门全过、审查（这里用 error 档的替身）也过，照样不合并，等用户拍板；gate pass 之后才合。
func TestRiskIrreversibleWaitsForPass(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := 0
	register(t, fakeGate{name: "审查替身", outcome: gate.Pass, applies: true, calls: &calls}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"db/migrations/001_init.sql": "CREATE TABLE t (id int);\n"}, ready("agent/x"))
	before := f.main()
	res := f.run(j)
	if res.Outcome != "hold" || calls != 1 || f.main() != before {
		t.Fatalf("难撤回要等用户：%+v（替身跑了 %d 次）", res, calls)
	}
	if strings.Contains(f.backlog(), "- [x] 做事") || f.git("branch", "--list", "agent/x") == "" {
		t.Fatalf("不该合并：%s", f.backlog())
	}
	if _, ran := resultOf(res, "test"); !ran {
		t.Errorf("门照跑：%+v", res.Results)
	}
	for _, r := range res.Results {
		if r.Outcome != gate.Pass {
			t.Errorf("门都该过：%+v", r)
		}
	}
	if res.Risk == nil || res.Risk.Level != gate.RiskIrreversible || !strings.Contains(res.Reason, "**/migrations/**") || !strings.Contains(res.Reason, "db/migrations/001_init.sql") {
		t.Fatalf("依据要写命中的规则和文件：%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck || !strings.Contains(j.Summary, "等你拍板") || !strings.Contains(j.StuckPending, "等你拍板") {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range s.Pending {
		if n.Card != nil && n.Card.GateJob == j.ID {
			found = true
			if n.Card.Recommend != "" || !strings.Contains(n.Text, "等你拍板") || !strings.Contains(strings.Join(n.Lines, " "), "难撤回") {
				t.Errorf("卡片：%+v", n)
			}
		}
	}
	if !found {
		t.Errorf("要发决定卡片：%+v", s.Pending)
	}
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := f.a.CmdGate([]string{"report", j.ID}); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); !strings.Contains(out, "改错的代价：难撤回（路径命中 **/migrations/**：db/migrations/001_init.sql") {
		t.Errorf("报告要写档位和依据：%s", out)
	}
	// 再审一次还是等，不会自己合
	j.Gate.Rerun = true
	if res := f.run(j); res.Outcome != "hold" || f.main() != before {
		t.Fatalf("再审也不自动合：%+v", res)
	}
	j.Gate.Rerun = false
	if err := f.a.CmdGate([]string{"pass", j.ID, "--reason", "迁移我看过了"}); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	res = f.run(j)
	f.collect(j, res)
	if res.Outcome != "merged" || j.Gate.State != state.GateMerged || !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Fatalf("放行后要合：%+v %+v", res, j.Gate)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "db/migrations/001_init.sql")); err != nil {
		t.Errorf("迁移要进 main：%v", err)
	}
}

// 影响面广：测试门关着就判不了，等用户；测试门开着照常合。局部的同样配置照常合。
func TestRiskBroadNeedsTestGate(t *testing.T) {
	cases := []struct {
		name   string
		levels string
		file   string
		want   string
	}{
		{"测试门关着，改依赖清单", `{"levels":{"review":"off","test":"off"}}`, "go.mod", "hold"},
		{"测试门关着，局部改动", `{"levels":{"review":"off","test":"off"}}`, "a.txt", "merged"},
		{"测试门开着，改依赖清单", `{"levels":{"review":"off","test":"error"}}`, "go.mod", "merged"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			f.commitMain(map[string]string{gate.GatesFile: c.levels})
			j := f.claim("做事", "agent/x", map[string]string{c.file: "module x\n"}, ready("agent/x"))
			res := f.run(j)
			if res.Outcome != c.want {
				t.Fatalf("要 %s：%+v", c.want, res)
			}
			if c.want == "hold" && (res.Risk.Level != gate.RiskBroad || !strings.Contains(res.Reason, "没有确定性检查") || !strings.Contains(res.Reason, "go.mod")) {
				t.Errorf("%+v", res)
			}
		})
	}
}

// 影响面广：测试门是 warn 档也按 error 算，没过就不合。局部的照 warn 合。
func TestRiskBroadWarnCountsAsError(t *testing.T) {
	for file, want := range map[string]string{"go.mod": "fail", "a.txt": "merged"} {
		t.Run(file, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			f.commitMain(map[string]string{gate.GatesFile: `{"levels":{"review":"off","test":"warn"}}`, gate.VerifyScript: "#!/bin/sh\nexit 1\n"})
			j := f.claim("做事", "agent/x", map[string]string{file: "module x\n"}, ready("agent/x"))
			res := f.run(j)
			if res.Outcome != want {
				t.Fatalf("要 %s：%+v", want, res)
			}
			if r, _ := resultOf(res, "test"); want == "fail" && r.Level != gate.LevelError {
				t.Errorf("测试门要按 error 记：%+v", r)
			}
		})
	}
}

// 规则读 main 上的：任务分支改 risk.json 去掉规则没用。
func TestRiskRulesFromMainOnly(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{gate.RiskFile: `{"irreversible":["secrets/**"]}`})
	j := f.claim("做事", "agent/x", map[string]string{"secrets/key.txt": "k", gate.RiskFile: `{}`}, ready("agent/x"))
	res := f.run(j)
	// 动了 .agentq/ 的测试门也会挡，这里只看档位：按 main 上的规则定
	if res.Outcome == "merged" || res.Risk == nil || res.Risk.Level != gate.RiskIrreversible || !strings.Contains(strings.Join(res.Risk.Reasons, "；"), "secrets/**") {
		t.Fatalf("任务分支改规则不算：%+v", res)
	}
}

// main 上的 risk.json 写坏了（JSON 不对、glob 写错）：项目列的难撤回路径不能悄悄丢掉。局部改动、门全过也不合，等用户拍板，gate pass 后合。
func TestRiskRulesBrokenHolds(t *testing.T) {
	for name, raw := range map[string]string{"JSON 不对": `{"irreversible":[`, "glob 写错": `{"irreversible":["db/["]}`} {
		t.Run(name, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			f.commitMain(map[string]string{gate.RiskFile: raw})
			j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
			before := f.main()
			res := f.run(j)
			if res.Outcome != "hold" || f.main() != before || res.Risk == nil || res.Risk.Level != gate.RiskIrreversible {
				t.Fatalf("写坏了要等用户：%+v", res)
			}
			if !strings.Contains(res.Reason, gate.RiskFile+" 有问题：") || !strings.Contains(res.Reason, "修好之前一律等你拍板") {
				t.Errorf("依据要写原因：%s", res.Reason)
			}
			for _, r := range res.Results {
				if r.Outcome != gate.Pass {
					t.Errorf("门都该过：%+v", r)
				}
			}
			f.collect(j, res)
			if j.Gate.State != state.GateStuck {
				t.Fatalf("%+v", j.Gate)
			}
			if err := state.Save(f.a.StatePath(), f.st); err != nil {
				t.Fatal(err)
			}
			if err := f.a.CmdGate([]string{"pass", j.ID, "--reason", "规则我回头修"}); err != nil {
				t.Fatal(err)
			}
			f.a.Collect(f.st)
			res = f.run(j)
			f.collect(j, res)
			if res.Outcome != "merged" || j.Gate.State != state.GateMerged {
				t.Fatalf("放行后要合：%+v %+v", res, j.Gate)
			}
		})
	}
}
