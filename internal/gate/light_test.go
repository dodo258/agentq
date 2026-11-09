package gate_test

import (
	"fmt"
	"strings"
	"testing"

	"agentq/internal/config"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 11-08 官网那条小改动的样子：一个 CSS 文件十几行，加一个测试文件，加记录文件。
const smallDiff = `diff --git a/src/styles.css b/src/styles.css
index 1..2 100644
--- a/src/styles.css
+++ b/src/styles.css
@@ -10,2 +10,6 @@
-.facts { grid-template-columns: 1fr 1fr; }
+.facts { grid-template-columns: 1fr 1fr; }
+@media (max-width: 720px) {
+  .facts { grid-template-columns: 1fr; }
+}
diff --git a/test/facts.test.js b/test/facts.test.js
new file mode 100644
--- /dev/null
+++ b/test/facts.test.js
@@ -0,0 +1,3 @@
+test("一栏", () => {})
+--- 不是文件头
+++ 也不是
diff --git a/CHANGELOG.md b/CHANGELOG.md
--- a/CHANGELOG.md
+++ b/CHANGELOG.md
@@ -1 +1,2 @@
+- 手机一栏
`

func TestChangedLines(t *testing.T) {
	if n := gate.ChangedLines(smallDiff); n != 5 {
		t.Errorf("只数 styles.css 的 5 行，测试和记录文件不算：%d", n)
	}
	del := "diff --git a/old.go b/old.go\ndeleted file mode 100644\n--- a/old.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-package x\n--- 删掉的一行内容\n"
	if n := gate.ChangedLines(del); n != 2 {
		t.Errorf("删掉的文件按 --- 那行算，内容里的 --- 是改动：%d", n)
	}
}

func TestLightFlow(t *testing.T) {
	small := gate.Candidate{Files: []string{"src/styles.css", "test/facts.test.js", "CHANGELOG.md", gate.ReadyFile}}
	if ok, why := gate.LightFlow(small, gate.RiskLocal, smallDiff); !ok || !strings.Contains(why, "改了 1 个文件、5 行") {
		t.Errorf("小改动走轻量：%v %s", ok, why)
	}
	var big strings.Builder
	big.WriteString("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1,61 @@\n")
	for i := range 61 {
		fmt.Fprintf(&big, "+line %d\n", i)
	}
	for _, c := range []struct {
		name string
		c    gate.Candidate
		risk gate.Risk
		diff string
		why  string
	}{
		{"影响面广", small, gate.RiskBroad, smallDiff, "影响面广"},
		{"难撤回", small, gate.RiskIrreversible, smallDiff, "难撤回"},
		{"rewrite", gate.Candidate{Files: small.Files, Type: "rewrite"}, gate.RiskLocal, smallDiff, "rewrite"},
		{"行数多", gate.Candidate{Files: []string{"a.go"}}, gate.RiskLocal, big.String(), "改了 61 行"},
		{"文件多", gate.Candidate{Files: []string{"a.go", "b.go", "c.go", "d.go"}}, gate.RiskLocal, "", "改了 4 个文件"},
		{"门脚本", gate.Candidate{Files: []string{".agentq/verify"}}, gate.RiskLocal, "", "门脚本"},
		{"指令文件", gate.Candidate{Files: []string{"AGENTS.md"}}, gate.RiskLocal, "", "指令文件"},
	} {
		if ok, why := gate.LightFlow(c.c, c.risk, c.diff); ok || !strings.Contains(why, c.why) {
			t.Errorf("%s 走完整流程：%v %s", c.name, ok, why)
		}
	}
}

func TestLightReviewModel(t *testing.T) {
	cfg := config.Config{Model: "opus", LightReviewModel: "sonnet"}
	c := gate.Candidate{Item: "小改动"}
	if r := gate.ReviewRequest(cfg, c); r.Model != "opus" || r.Effort != "medium" {
		t.Errorf("完整流程照 model、medium：%s %s", r.Model, r.Effort)
	}
	c.Light = true
	if r := gate.ReviewRequest(cfg, c); r.Model != "sonnet" || r.Effort != "medium" {
		t.Errorf("轻量只换便宜一档的模型，强度不降：%s %s", r.Model, r.Effort)
	}
	if r := gate.UIReviewRequest(cfg, c, "/shots", []string{"a.png"}, nil); r.Model != "sonnet" || r.Effort != "medium" {
		t.Errorf("界面审查也只换模型：%s %s", r.Model, r.Effort)
	}
	cfg.LightReviewModel = ""
	if r := gate.ReviewRequest(cfg, c); r.Model != "opus" || r.Effort != "medium" {
		t.Errorf("没配便宜模型就和完整流程一样：%s %s", r.Model, r.Effort)
	}
}

// 走一遍门：小改动的审查进程拿到便宜模型，报告写「流程：轻量」；改多了写「流程：完整」和原因。
func TestLightFlowThroughGates(t *testing.T) {
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), reply("pass"))
	f.a.Cfg.Backend, f.a.Cfg.Model, f.a.Cfg.LightReviewModel = f.name, "opus", "sonnet" // 假后端当默认后端，模型才不被清掉
	j := f.job(newFiles())
	res := f.run(j)
	if res.Outcome != "merged" || res.Risk == nil || !res.Risk.Light {
		t.Fatalf("%+v", res)
	}
	args := testkit.ReadFile(t, f.args)
	if !strings.Contains(args, "--mdl\nsonnet\n") { // 假后端不记强度，强度见 TestLightReviewModel
		t.Errorf("审查要用 sonnet：%s", args)
	}
	f.collect(j, res)
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := f.a.CmdGate([]string{"report", j.ID}); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); !strings.Contains(out, "流程：轻量") || !strings.Contains(out, "改了 1 个文件、1 行") {
		t.Errorf("报告要写轻量和理由：%s", out)
	}

	g := newReviewFix(t, fixBacklog, testkit.AllCaps(), reply("pass"))
	g.a.Cfg.Backend, g.a.Cfg.Model, g.a.Cfg.LightReviewModel = g.name, "opus", "sonnet"
	many := map[string]string{}
	for i := range 4 {
		many[fmt.Sprintf("f%d.txt", i)] = "x\n"
	}
	k := g.job(many)
	if res := g.run(k); res.Outcome != "merged" || res.Risk.Light {
		t.Fatalf("%+v", res)
	}
	if args := testkit.ReadFile(t, g.args); !strings.Contains(args, "--mdl\nopus\n") {
		t.Errorf("完整流程照 opus：%s", args)
	}
}
