package gate_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func testResult(t *testing.T, res gate.RunResult) gate.Result {
	t.Helper()
	for _, r := range res.Results {
		if r.Gate == "test" {
			return r
		}
	}
	t.Fatalf("没跑测试门：%+v", res)
	return gate.Result{}
}

// verifyOnMain 把 main 上的门脚本换成 script。
func (f *fix) verifyOnMain(script string) {
	f.t.Helper()
	p := filepath.Join(f.dir, gate.VerifyScript)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", "门脚本")
}

func TestTestGatePassAndFail(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\npwd\necho 查过了\n")
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	res := f.run(j)
	r := testResult(t, res)
	if res.Outcome != "merged" || r.Outcome != gate.Pass || !strings.Contains(strings.Join(r.Evidence, " "), "test.log") {
		t.Fatalf("%+v", res)
	}
	log := testkit.ReadFile(t, filepath.Join(f.a.Dir(j.ID), "test.log"))
	if !strings.Contains(log, "查过了") || !strings.Contains(log, filepath.Join("gate", j.ID, "wt")) {
		t.Errorf("要在候选 worktree 里跑：%s", log)
	}

	g := newFix(t, fixBacklog)
	g.verifyOnMain("#!/bin/sh\necho 坏了\nexit 3\n")
	j = g.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	before := g.main()
	res = g.run(j)
	r = testResult(t, res)
	if res.Outcome != "fail" || r.Outcome != gate.Fail || !strings.Contains(r.Summary, "退出码 3") || g.main() != before {
		t.Fatalf("%+v", res)
	}
}

// 验收：改门脚本无效。任务把门脚本改成永远过，跑的还是 main 上的；任务把它改坏，也不影响。
func TestChangingGateScriptHasNoEffect(t *testing.T) {
	t.Run("改成永远过", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\nexit 1\n")
		j := f.claim("做事", "agent/x", map[string]string{gate.VerifyScript: "#!/bin/sh\nexit 0\n"}, ready("agent/x"))
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "改门\n\n"+gate.NoteMark+"脚本太严")
		j.Gate.Head = testkit.GitT(t, f.home, wt, "rev-parse", "HEAD")
		res := f.run(j)
		if res.Outcome != "fail" || !strings.Contains(testResult(t, res).Summary, "没过") {
			t.Fatalf("写了说明也跑 main 上的：%+v", res)
		}
	})
	t.Run("没写说明直接拦", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\nexit 0\n")
		j := f.claim("做事", "agent/x", map[string]string{gate.VerifyScript: "#!/bin/sh\necho x\n"}, ready("agent/x"))
		res := f.run(j)
		r := testResult(t, res)
		if res.Outcome != "fail" || !strings.Contains(strings.Join(r.Evidence, " "), "改了 "+gate.VerifyScript) {
			t.Fatalf("%+v", res)
		}
		if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), "test.log")); err == nil {
			t.Error("标记没说明的不用再跑脚本")
		}
	})
	t.Run("改坏了也跑 main 上的", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\nexit 0\n")
		j := f.claim("做事", "agent/x", map[string]string{gate.VerifyScript: "#!/bin/sh\nexit 9\n"}, ready("agent/x"))
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "门说明：要加严")
		res := f.run(j)
		if res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
		// 合进去的是任务的版本，下一次才生效
		if got := testkit.ReadFile(t, filepath.Join(f.dir, gate.VerifyScript)); !strings.Contains(got, "exit 9") {
			t.Errorf("%s", got)
		}
	})
	t.Run("helper 也用 main 上的", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		os.MkdirAll(filepath.Join(f.dir, ".agentq"), 0o755)
		os.WriteFile(filepath.Join(f.dir, ".agentq", "helper.sh"), []byte("exit 1\n"), 0o755)
		f.verifyOnMain("#!/bin/sh\nsh .agentq/helper.sh\n")
		j := f.claim("做事", "agent/x", map[string]string{".agentq/helper.sh": "exit 0\n", ".agentq/added.sh": "x"}, ready("agent/x"))
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "门说明：改 helper")
		if res := f.run(j); res.Outcome != "fail" {
			t.Fatalf("%+v", res)
		}
	})
}

// 没有检查命令算判不了；任务新增的（首次）用任务的版本，并标给审查门；软链接、没有可执行权限的不算有。
func TestTestGateScriptPresence(t *testing.T) {
	t.Run("都没有", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.git("rm", "-q", "-f", gate.VerifyScript)
		f.git("commit", "-q", "-m", "没有门脚本")
		j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
		res := f.run(j)
		if r := testResult(t, res); res.Outcome != "unknown" || r.Outcome != gate.Unknown || !strings.Contains(r.Summary, gate.VerifyScript) {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("首次新增", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.git("rm", "-q", "-f", gate.VerifyScript)
		f.git("commit", "-q", "-m", "没有门脚本")
		j := f.claim("做事", "agent/x", map[string]string{gate.VerifyScript: "#!/bin/sh\nexit 0\n"}, ready("agent/x"))
		res := f.run(j)
		r := testResult(t, res)
		if res.Outcome != "merged" || !strings.Contains(strings.Join(r.Evidence, " "), "首次由任务新增") {
			t.Fatalf("不用写门说明，标给审查：%+v", res)
		}
	})
	t.Run("首次新增跑不过", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.git("rm", "-q", "-f", gate.VerifyScript)
		f.git("commit", "-q", "-m", "没有门脚本")
		j := f.claim("做事", "agent/x", map[string]string{gate.VerifyScript: "#!/bin/sh\nexit 2\n"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "fail" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("main 上是软链接", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		p := filepath.Join(f.dir, gate.VerifyScript)
		os.Remove(p)
		os.Symlink("/bin/true", p)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "软链接")
		j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "unknown" {
			t.Fatalf("软链接可能指到项目外，不跑：%+v", res)
		}
	})
	t.Run("main 上没有可执行权限", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		p := filepath.Join(f.dir, gate.VerifyScript)
		os.Chmod(p, 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "不可执行")
		j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "unknown" {
			t.Fatalf("%+v", res)
		}
	})
}

// 验收：加 skip 没说明会失败。写了说明放行，交给审查门判断（Prior 里看得到）。
func TestDiffFlags(t *testing.T) {
	add := func(path, text string) map[string]string { return map[string]string{path: text} }
	cases := []struct {
		name  string
		seed  map[string]string
		files map[string]string
		del   []string
		want  string
	}{
		{name: "t.Skip", files: add("a_test.go", "package a\nfunc TestA(t *testing.T) { t.Skip(\"x\") }\n"), want: "新增跳过"},
		{name: "it.skip", files: add("a.test.ts", "it.skip('x', () => {})\n"), want: "新增跳过"},
		{name: "it.only", files: add("a.spec.js", "it.only('x', () => {})\n"), want: "新增跳过"},
		{name: "xit", files: add("a.test.js", "xit('x', () => {})\n"), want: "新增跳过"},
		{name: "ignore", files: add("tests/a.rs", "#[ignore]\nfn a() {}\n"), want: "新增跳过"},
		{name: "pytest", files: add("test_a.py", "@pytest.mark.skip\ndef test_a(): pass\n"), want: "新增跳过"},
		{name: "删测试文件", seed: add("a_test.go", "package a\n"), del: []string{"a_test.go"}, want: "删了测试文件 a_test.go"},
		{name: "删用例", seed: add("a_test.go", "package a\nfunc TestGone(t *testing.T) {}\nfunc TestStay(t *testing.T) {}\n"), files: add("a_test.go", "package a\nfunc TestStay(t *testing.T) {}\n"), want: "删了测试用例 TestGone"},
		{name: "删 python 用例", seed: add("test_a.py", "def test_gone(): pass\n"), files: add("test_a.py", "x = 1\n"), want: "删了测试用例 test_gone"},
		{name: "改 gates.json", files: add(gate.GatesFile, "{\"levels\":{}}\n"), want: "改了 " + gate.GatesFile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := func(note string) gate.RunResult {
				f := newFix(t, fixBacklog)
				for p, text := range c.seed {
					os.WriteFile(filepath.Join(f.dir, p), []byte(text), 0o644)
				}
				f.git("add", "-A")
				f.git("commit", "-q", "--allow-empty", "-m", "种子")
				j := f.claim("做事", "agent/x", c.files, gate.Ready{})
				wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
				for _, p := range c.del {
					testkit.GitT(t, f.home, wt, "rm", "-q", p)
				}
				msg := "改测试"
				if note != "" {
					msg += "\n\n" + gate.NoteMark + note
				}
				f.commit(wt, nil, ready("agent/x"))
				testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", msg)
				return f.run(j)
			}
			res := run("")
			r := testResult(t, res)
			if res.Outcome != "fail" || !strings.Contains(strings.Join(r.Evidence, "；"), c.want) || !strings.Contains(r.Summary, gate.NoteMark) {
				t.Fatalf("没说明要失败：%+v", res)
			}
			res = run("这个用例依赖外网，先跳过")
			r = testResult(t, res)
			ev := strings.Join(r.Evidence, "；")
			if res.Outcome != "merged" || !strings.Contains(ev, c.want) || !strings.Contains(ev, "依赖外网") {
				t.Fatalf("写了说明交给审查门：%+v", res)
			}
		})
	}
}

// 不算的：普通文件里提到 t.Skip（比如这个门自己的源码）、改名、加用例、正文里提到门说明但不是开头。
func TestDiffNotFlagged(t *testing.T) {
	t.Run("非测试文件", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		j := f.claim("做事", "agent/x", map[string]string{"gate.go": "var re = `t.Skip(`\n"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("改名", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		body := "package a\n" + strings.Repeat("// 内容够长才认得出改名\n", 5) + "func TestA(t *testing.T) {}\n"
		os.WriteFile(filepath.Join(f.dir, "a_test.go"), []byte(body), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "种子")
		j := f.claim("做事", "agent/x", nil, gate.Ready{})
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "mv", "a_test.go", "b_test.go")
		f.commit(wt, nil, ready("agent/x"))
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("说明不在行首", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		j := f.claim("做事", "agent/x", map[string]string{"a_test.go": "t.Skip(\"x\")\n"}, gate.Ready{})
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		f.commit(wt, nil, ready("agent/x"))
		testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "这里提到门说明：不算")
		if res := f.run(j); res.Outcome != "fail" {
			t.Fatalf("%+v", res)
		}
	})
}

const fixVerify = "#!/bin/sh\n[ ! -f tests/check.sh ] || sh tests/check.sh\n"

// 验收：撤修复还通过的假修复被拦下。真修复撤掉会失败，放行。
func TestFixMustFailWithoutFix(t *testing.T) {
	fixReady := gate.Ready{Branch: "agent/x", Type: "fix", Acceptance: "看", Progress: "修好了"}
	t.Run("真修复", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(fixVerify)
		os.WriteFile(filepath.Join(f.dir, "src.txt"), []byte("坏的\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "有 bug")
		j := f.claim("做事", "agent/x", map[string]string{"src.txt": "好的\n", "tests/check.sh": "grep -q 好的 src.txt\n"}, fixReady)
		res := f.run(j)
		r := testResult(t, res)
		if res.Outcome != "merged" || !strings.Contains(strings.Join(r.Evidence, " "), "撤掉修复后测试失败") {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("假修复", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(fixVerify)
		os.WriteFile(filepath.Join(f.dir, "src.txt"), []byte("坏的\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "有 bug")
		before := f.main()
		j := f.claim("做事", "agent/x", map[string]string{"src.txt": "好的\n", "tests/check.sh": "exit 0\n"}, fixReady)
		mainBefore := f.main()
		res := f.run(j)
		r := testResult(t, res)
		if res.Outcome != "fail" || !strings.Contains(r.Summary, "撤掉修复测试还是过") || f.main() != mainBefore {
			t.Fatalf("%+v %s", res, before)
		}
	})
	t.Run("新增文件的修复也撤", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(fixVerify)
		j := f.claim("做事", "agent/x", map[string]string{"fix.txt": "有了\n", "tests/check.sh": "test -f fix.txt\n"}, fixReady)
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("带测试的 Go 用例算新测试", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\nexit 0\n")
		os.WriteFile(filepath.Join(f.dir, "a_test.go"), []byte("package a\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "种子")
		j := f.claim("做事", "agent/x", map[string]string{"a.go": "package a\n", "a_test.go": "package a\nfunc TestFix(t *testing.T) {}\n"}, fixReady)
		if res := f.run(j); res.Outcome != "fail" {
			t.Fatalf("脚本永远过，撤了修复也过：%+v", res)
		}
	})
	t.Run("修复没带测试只记一笔", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\nexit 0\n")
		j := f.claim("做事", "agent/x", map[string]string{"src.txt": "好的\n"}, fixReady)
		res := f.run(j)
		if res.Outcome != "merged" || !strings.Contains(strings.Join(testResult(t, res).Evidence, " "), "没带新测试") {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("feature 不做这个检查", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\nexit 0\n")
		j := f.claim("做事", "agent/x", map[string]string{"src.txt": "x\n", "tests/check.sh": "exit 0\n"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("撤了之后判不了", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\n[ -f src.txt ] && [ \"$(cat src.txt)\" = 好的 ] && exit 0\nkill -9 $$\n")
		os.WriteFile(filepath.Join(f.dir, "src.txt"), []byte("坏的\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "有 bug")
		j := f.claim("做事", "agent/x", map[string]string{"src.txt": "好的\n", "tests/check.sh": "x\n"}, fixReady)
		if res := f.run(j); res.Outcome != "unknown" {
			t.Fatalf("%+v", res)
		}
	})
}

// 不一致的结果连跑两次确认：两次都过标 flaky 放行留痕；有一次没过算失败也标 flaky；三次都失败不提 flaky。
func TestFlaky(t *testing.T) {
	counting := func(t *testing.T, plan string) string {
		count := filepath.Join(t.TempDir(), "count")
		t.Setenv("AGENTQ_TEST_COUNT", count)
		t.Setenv("AGENTQ_TEST_PLAN", plan)
		return "#!/bin/sh\nn=$(cat \"$AGENTQ_TEST_COUNT\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"$AGENTQ_TEST_COUNT\"\ncase \"$AGENTQ_TEST_PLAN\" in *\"[$n]\"*) exit 0 ;; esac\nexit 1\n"
	}
	for _, c := range []struct {
		name, plan string
		want       string
		flaky      bool
	}{
		{"两次都过", "[2][3]", "merged", true},
		{"过一次", "[2]", "fail", true},
		{"都没过", "", "fail", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			f.verifyOnMain(counting(t, c.plan))
			j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
			res := f.run(j)
			r := testResult(t, res)
			if res.Outcome != c.want {
				t.Fatalf("%+v", res)
			}
			if strings.Contains(r.Summary, "flaky") != c.flaky || !strings.Contains(strings.Join(r.Evidence, " "), "test-again2.log") {
				t.Errorf("%+v", r)
			}
		})
	}
}

// 超时、被信号杀了：判不了，不是失败，不退回。
func TestTestGateUnknown(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\nkill -9 $$\n")
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	if res := f.run(j); res.Outcome != "unknown" {
		t.Fatalf("%+v", res)
	}
	g := newFix(t, fixBacklog)
	g.verifyOnMain("#!/bin/sh\nsleep 60\n")
	j = g.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	dir := g.a.Dir(j.ID)
	os.MkdirAll(dir, 0o700)
	res := g.a.Process(t.Context(), dir, gate.PlanOf(g.a, j), 300*time.Millisecond)
	if res.Outcome != "unknown" || !strings.Contains(testResult(t, res).Summary, "没跑完") {
		t.Fatalf("%+v", res)
	}
}

// main 中途动过：测试门重跑，用的是新 main 上的脚本。
func TestTestGateRerunsAfterMainMoved(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv("AGENTQ_TEST_COUNT", count)
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\necho x >> \"$AGENTQ_TEST_COUNT\"\n")
	moved := false
	register(t, fakeGate{name: "捣乱一次", applies: true, run: func(c gate.Candidate) gate.Result {
		if !moved {
			moved = true
			os.WriteFile(filepath.Join(f.dir, "other.txt"), []byte("x"), 0o644)
			f.git("add", "other.txt")
			f.git("commit", "-q", "-m", "别人的")
		}
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	if res := f.run(j); res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	b, _ := os.ReadFile(count)
	if n := strings.Count(string(b), "x"); n != 2 {
		t.Errorf("测试门要重跑一次：%s", strconv.Itoa(n))
	}
}

// 测试门换过 .agentq、跑脚本弄脏 worktree，下一道门看到的还是候选。
func TestWorktreeResetBetweenGates(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\ntouch 脚本留下的\necho 脏 >> a.txt\n")
	var seen, extra string
	register(t, fakeGate{name: "看 worktree", applies: true, run: func(c gate.Candidate) gate.Result {
		b, _ := os.ReadFile(filepath.Join(c.Dir, ".agentq", "verify"))
		seen = string(b)
		if _, err := os.Stat(filepath.Join(c.Dir, "脚本留下的")); err == nil {
			extra = "脚本留下的文件还在"
		}
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{gate.VerifyScript: "#!/bin/sh\necho 任务的\n"}, gate.Ready{})
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	f.commit(wt, nil, ready("agent/x"))
	testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "门说明：要改")
	if res := f.run(j); res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(seen, "任务的") || extra != "" {
		t.Errorf("%q %q", seen, extra)
	}
}

// 中文文件名：撤修复要撤得掉，删中文名的测试要认得出（git 默认把路径输出成带引号的八进制）。
func TestNonASCIIPaths(t *testing.T) {
	fixReady := gate.Ready{Branch: "agent/x", Type: "fix", Acceptance: "看", Progress: "修好了"}
	t.Run("撤修复", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(fixVerify)
		os.WriteFile(filepath.Join(f.dir, "说明.txt"), []byte("坏的\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "有 bug")
		j := f.claim("做事", "agent/x", map[string]string{"说明.txt": "好的\n", "tests/check.sh": "grep -q 好的 说明.txt\n"}, fixReady)
		res := f.run(j)
		if res.Outcome != "merged" || !strings.Contains(strings.Join(testResult(t, res).Evidence, " "), "撤掉修复后测试失败") {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("新增的文件也撤", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(fixVerify)
		j := f.claim("做事", "agent/x", map[string]string{"修复.txt": "有了\n", "tests/check.sh": "test -f 修复.txt\n"}, fixReady)
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("删中文名的测试", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		os.MkdirAll(filepath.Join(f.dir, "tests"), 0o755)
		os.WriteFile(filepath.Join(f.dir, "tests", "登录.sh"), []byte("exit 0\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "种子")
		j := f.claim("做事", "agent/x", nil, gate.Ready{})
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "rm", "-q", "tests/登录.sh")
		f.commit(wt, nil, ready("agent/x"))
		res := f.run(j)
		if res.Outcome != "fail" || !strings.Contains(strings.Join(testResult(t, res).Evidence, " "), "删了测试文件 tests/登录.sh") {
			t.Fatalf("%+v", res)
		}
	})
}

// 文档不算测试文件：方案放在 docs/specs/，提到 it.skip 很正常，修 bug 多写一份方案也不算新测试。
func TestDocsAreNotTests(t *testing.T) {
	t.Run("方案提到跳过", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		j := f.claim("做事", "agent/x", map[string]string{"docs/specs/x.md": "查 it.skip 和 t.Skip(\n"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("修 bug 带方案", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		j := f.claim("做事", "agent/x", map[string]string{"src.txt": "好的\n", "docs/specs/x.md": "方案\n"}, gate.Ready{Branch: "agent/x", Type: "fix", Acceptance: "看", Progress: "修好了"})
		res := f.run(j)
		if res.Outcome != "merged" || !strings.Contains(strings.Join(testResult(t, res).Evidence, " "), "没带新测试") {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("删方案", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		os.MkdirAll(filepath.Join(f.dir, "docs", "specs"), 0o755)
		os.WriteFile(filepath.Join(f.dir, "docs", "specs", "x.md"), []byte("旧方案\n"), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "种子")
		j := f.claim("做事", "agent/x", nil, gate.Ready{})
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "rm", "-q", "docs/specs/x.md")
		f.commit(wt, nil, ready("agent/x"))
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
}

// Rust 的测试写在代码文件里：#[ignore] 也要查，但不算新测试（撤修复会连测试一起撤）。
func TestRustInlineIgnore(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"src/lib.rs": "#[ignore]\n#[test]\nfn a() {}\n"}, ready("agent/x"))
	res := f.run(j)
	if r := testResult(t, res); res.Outcome != "fail" || !strings.Contains(strings.Join(r.Evidence, " "), "新增跳过 src/lib.rs") {
		t.Fatalf("%+v", res)
	}
	g := newFix(t, fixBacklog)
	g.verifyOnMain("#!/bin/sh\nexit 0\n")
	j = g.claim("做事", "agent/x", map[string]string{"src/lib.rs": "#[test]\nfn a() {}\n"}, gate.Ready{Branch: "agent/x", Type: "fix", Acceptance: "看", Progress: "修好了"})
	if res := g.run(j); res.Outcome != "merged" {
		t.Fatalf("同一个文件里的修复和测试不做撤修复检查：%+v", res)
	}
}

// 脚本退 125 说这次跑不了：判不了，不退回不烧额度，也不连跑确认。
func TestVerifyCannotRunIsUnknown(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv("AGENTQ_TEST_COUNT", count)
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\necho x >> \"$AGENTQ_TEST_COUNT\"\nexit 125\n")
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	res := f.run(j)
	if r := testResult(t, res); res.Outcome != "unknown" || r.Outcome != gate.Unknown || !strings.Contains(r.Summary, "跑不了") {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 1 {
		t.Errorf("跑不了不用连跑确认：%s", b)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck || j.Status != state.StatusDone || j.ResumePending {
		t.Errorf("不退回：%+v %s", j.Gate, j.Status)
	}
	// 撤修复时跑不了也是判不了，不算「撤掉还过」或「撤掉失败」
	g := newFix(t, fixBacklog)
	g.verifyOnMain("#!/bin/sh\n[ -f fixed.txt ] && exit 0\nexit 125\n")
	j2 := g.claim("做事", "agent/x", map[string]string{"fixed.txt": "x", "tests/a.sh": "x"}, gate.Ready{Branch: "agent/x", Type: "fix", Acceptance: "看", Progress: "修好了"})
	if res := g.run(j2); res.Outcome != "unknown" {
		t.Fatalf("%+v", res)
	}
}
