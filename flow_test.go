package main

import (
	"cmp"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentq/internal/order"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agq-test")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "agentq")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fake 是假 claude：按提示词认是哪一步，做什么由 $FAKE 目录里的脚本定，没写就用默认的。
const fake = `#!/bin/sh
prompt= ; schema= ; resume= ; model=
while [ $# -gt 0 ]; do
  case "$1" in -p) prompt=$2; shift 2;; -r) resume=$2; shift 2;; --model) model=$2; shift 2;; --json-schema) schema=1; shift 2;; *) shift;; esac
done
say() { printf '{"is_error":false,"result":"%s","session_id":"s","num_turns":9,"structured_output":%s,"modelUsage":{"opus":{"inputTokens":10,"outputTokens":5}}}' "$1" "${2:-null}"; }
commit() { git add -A && git -c user.name=t -c user.email=t@t commit -qm "$1"; }
case "$prompt" in
  *你是产品经理*) step=plan;;
  *唯一一次全面审计*) step=audit;;
  *审计找到下面这些大*) step=fix;;
  *"跑 .agentq/verify"*) step=red;;
  *别人的改动先合进来*) step=conflict;;
  *正在合入最新的*) step=land;;
  *还有没提交的东西*) step=tidy;;
  *你做的这一块*) step=mod-$(printf '%s' "$prompt" | sed -n 's/^你做的这一块：//p');;
  *手机上发来一条消息*) step=chat;;
  *接着*) step=again;;
  *) step=unknown;;
esac
echo "$step $(pwd) $model" >> "$FAKE/calls"
if [ -f "$FAKE/$step" ]; then . "$FAKE/$step"; exit; fi
case $step in
  mod-*) echo "${step#mod-}" > "${step#mod-}.txt"; commit "$step"; say "做好了。\n\n看 ${step#mod-}.txt";;
  audit) say 看完了 '{"summary":"好","bugs":[]}';;
  fix) echo ok > fix.txt; commit fix; say 修了 '{"results":[{"result":"fixed","note":"好了"}]}';;
  red|last|tidy) echo ok > "$step.txt"; commit "$step"; say "$step";;
  *) say "$step";;
esac
`

type rig struct {
	t               *testing.T
	home, dir, fake string
	exe             string // 跑哪个 agentq，默认 bin
	extra           []string
}

func newRig(t *testing.T, plan, verify string) *rig {
	t.Parallel()
	r := &rig{t: t, home: t.TempDir(), dir: filepath.Join(t.TempDir(), "demo"), fake: t.TempDir()}
	os.MkdirAll(filepath.Join(r.dir, ".agentq"), 0o755)
	r.write(filepath.Join(r.dir, ".agentq", "verify"), "#!/bin/sh\n"+verify+"\n", 0o755)
	r.write(filepath.Join(r.dir, ".gitignore"), ".claude/\n", 0o644)
	r.git(r.dir, "init", "-q", "-b", "main")
	r.git(r.dir, "config", "maintenance.auto", "false") // 后台整理会和清临时目录抢
	r.git(r.dir, "config", "user.name", "t")
	r.git(r.dir, "config", "user.email", "t@t")
	r.git(r.dir, "add", "-A")
	r.git(r.dir, "commit", "-qm", "开头")
	r.script("plan", `say 方案 '`+plan+`'`)
	// 环境里只带得过白名单，假 claude 用绝对路径找自己的目录
	r.write(filepath.Join(r.fake, "claude"), strings.Replace(fake, "prompt= ;", "FAKE="+r.fake+"; prompt= ;", 1), 0o755)
	return r
}

func (r *rig) write(p, s string, mode os.FileMode) {
	r.t.Helper()
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) script(step, body string) { r.write(filepath.Join(r.fake, step), body, 0o644) }

func (r *rig) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v：%v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *rig) agentq(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(cmp.Or(r.exe, bin), args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "AGENTQ_HOME="+r.home, "AGENTQ_CLAUDE="+filepath.Join(r.fake, "claude"), "AGENTQ_CODEX="+filepath.Join(r.fake, "codex"), "AGENTQ_TELEGRAM=/nonexistent", "HOME="+r.home)
	cmd.Env = append(cmd.Env, r.extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("agentq %v：%v %s", args, err, out)
	}
	return string(out)
}

func (r *rig) order() *order.Order {
	all := (&order.Store{Root: r.home}).All()
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

func (r *rig) wait(state string) *order.Order {
	r.t.Helper()
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if o := r.order(); o != nil && (o.State == state || o.Over()) && !(o.Over() && o.Running()) {
			if o.State != state {
				r.t.Fatalf("要「%s」，到了「%s」：%s\n%s", state, o.State, o.Why, r.agentq("log", o.ID))
			}
			return o
		}
	}
	r.t.Fatalf("等不到「%s」：%+v\n%s", state, r.order(), r.agentq("log", r.order().ID))
	return nil
}

func (r *rig) calls() string {
	b, _ := os.ReadFile(filepath.Join(r.fake, "calls"))
	return string(b)
}

func (r *rig) count(step string) int {
	n := 0
	for _, l := range strings.Split(r.calls(), "\n") {
		if strings.HasPrefix(l, step+" ") {
			n++
		}
	}
	return n
}

func (r *rig) has(file string) bool {
	_, err := os.Stat(filepath.Join(r.dir, file))
	return err == nil
}

func mod(name string, needs ...string) string {
	q := make([]string, len(needs))
	for i, n := range needs {
		q[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf(`{"name":%q,"goal":"做","paths":["x/"],"needs":[%s],"accept":"有"}`, name, strings.Join(q, ","))
}

func plan(small bool, mods ...string) string {
	return fmt.Sprintf(`{"small":%v,"title":"做个东西","summary":"分几块","spec":"# 方案","stop":"","modules":[%s]}`, small, strings.Join(mods, ","))
}

// 小改动：一个会话、一次测试，不审计，直接进主分支，现场清干净。
func TestSmallChange(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "test -f a.txt")
	r.agentq("do", "加个", "东西")
	o := r.wait(order.Done)
	if !r.has("a.txt") || r.count("audit") != 0 || r.count("plan") != 1 || r.count("mod-a") != 1 {
		t.Fatalf("%s", r.calls())
	}
	if o.Text != "加个 东西" || o.How != "看 a.txt" || o.Commit == "" || o.Tokens == 0 {
		t.Errorf("%+v", o)
	}
	if n := strings.Count(r.git(r.dir, "worktree", "list"), "\n"); n != 0 {
		t.Errorf("worktree 要清掉：%s", r.git(r.dir, "worktree", "list"))
	}
	if b := r.git(r.dir, "branch", "--format=%(refname:short)"); b != "main" {
		t.Errorf("分支要清掉：%s", b)
	}
}

// 大活：没依赖的同时开工，有依赖的等；审计一次，大 bug 修一次，再跑测试，不再审。
func TestFullLine(t *testing.T) {
	r := newRig(t, plan(false, mod("a"), mod("b"), strings.Replace(mod("c", "a", "b"), "{", `{"hard":true,`, 1)), "test -f c.txt")
	// a、b 互相等对方开工了才做完：不同时开工就会等到超时
	wait := `touch "$FAKE/in-%s"; i=0; while [ ! -f "$FAKE/in-%s" ]; do sleep 0.1; i=$((i+1)); [ $i -lt 100 ] || exit 9; done; echo %s > %s.txt; commit %s; say 好`
	r.script("mod-a", fmt.Sprintf(wait, "a", "b", "a", "a", "a"))
	r.script("mod-b", fmt.Sprintf(wait, "b", "a", "b", "b", "b"))
	r.script("mod-c", `[ -f a.txt ] && [ -f b.txt ] || exit 9; echo c > c.txt; commit c; say 好`)
	r.script("audit", `echo 乱写 > a.txt; echo 乱 > 乱.txt; say 看完 '{"summary":"有","bugs":[{"module":"a","where":"a.txt:1","what":"坏了","how":"打开看"}]}'`)
	r.agentq("do", "做个大的")
	o := r.wait(order.Done)
	if r.count("audit") != 1 || r.count("fix") != 1 || !r.has("fix.txt") || !r.has("c.txt") || r.has("乱.txt") {
		t.Fatalf("%s", r.calls())
	}
	if got, _ := os.ReadFile(filepath.Join(r.dir, "a.txt")); string(got) != "a\n" {
		t.Errorf("审计不许改：%s", got)
	}
	if len(o.Bugs) != 1 || !o.Fixed {
		t.Errorf("%+v", o.Bugs)
	}
	for _, want := range []string{"/trees/m-plan opus", "/m-a sonnet", "/m-c opus", "/all opus"} { // 难的和合到一起的用最强的
		if !strings.Contains(strings.ReplaceAll(r.calls(), "/private", ""), want) {
			t.Errorf("缺「%s」：%s", want, r.calls())
		}
	}
	log := r.agentq("log", o.ID)
	for _, want := range []string{"**产品经理** 交出方案，划成 3 块", "**a** 接到：做（只动 x/）", "**c** 交回：好", "打回 a：a.txt:1 坏了", "**合并** 进了 main"} {
		if !strings.Contains(log, want) {
			t.Errorf("交接单缺「%s」：%s", want, log)
		}
	}
}

// 大活做完让 Codex 审一次，用最强的模型和超高强度；风险只告诉用户，不改。它审不成才换 Claude。
func TestCodexAudit(t *testing.T) {
	r := newRig(t, strings.Replace(plan(false, mod("a"), mod("b")), "{", `{"later":["二期 —— 接着做"],`, 1), "true")
	r.write(filepath.Join(r.fake, "codex"), `#!/bin/sh
echo "$*" > `+r.fake+`/codex-args
while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
echo 乱 > 乱.txt
echo '{"summary":"看完","bugs":[{"module":"a","where":"a.txt:1","what":"坏了","how":"看"}],"risks":["以后数据大了会慢"]}' > "$out"
`, 0o755)
	r.agentq("do", "做个大的")
	o := r.wait(order.Done)
	args, _ := os.ReadFile(filepath.Join(r.fake, "codex-args"))
	if r.count("audit") != 0 || r.count("fix") != 1 || len(o.Risks) != 1 || r.has("乱.txt") {
		t.Errorf("%s %+v", r.calls(), o)
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, "docs", "BACKLOG.md"))
	if specs, _ := filepath.Glob(filepath.Join(r.dir, "docs", "specs", "*.md")); len(specs) != 1 || !strings.Contains(string(b), "- [ ] 二期 —— 接着做") {
		t.Errorf("方案和后面几期要写进项目：%v %s", specs, b)
	}
	for _, want := range []string{"-m gpt-6.2-astra", "model_reasoning_effort=xhigh", "-s workspace-write", "唯一一次全面审计", "做个大的"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("缺「%s」：%s", want, args)
		}
	}
}

// 测试红了就修，修好接着走；一直红就停下，主分支一点没动。
func TestRed(t *testing.T) {
	t.Run("修好", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "test -f red.txt")
		r.agentq("do", "做")
		r.wait(order.Done)
		if r.count("red") != 1 {
			t.Errorf("%s", r.calls())
		}
	})
	t.Run("修不好", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "exit 1")
		before := r.git(r.dir, "rev-parse", "main")
		r.script("red", `echo $$ > try.txt; commit try; say 试了`)
		r.agentq("do", "做")
		o := r.wait(order.Stuck)
		if r.count("red") != 4 || !strings.HasSuffix(strings.TrimSpace(r.calls()), " fable") || strings.Count(r.calls(), " fable") != 1 || r.git(r.dir, "rev-parse", "main") != before || !strings.Contains(o.Why, "修了 3 次、fable 出手一次还没过") {
			t.Errorf("%s %s", o.Why, r.calls())
		}
		// 人看过、改好了检查，接着做就过
		r.git(r.dir, "commit", "-q", "--allow-empty", "-m", "别的")
		os.Remove(filepath.Join(r.fake, "red"))
		r.script("red", `printf '#!/bin/sh\nexit 0\n' > .agentq/verify; commit 修; say 好`)
		r.agentq("retry", o.ID)
		r.wait(order.Done)
		if r.count("mod-a") != 1 {
			t.Errorf("做好的模块不重做：%s", r.calls())
		}
	})
}

// 两块改了同一个文件：后合的自己解决。
func TestConflict(t *testing.T) {
	r := newRig(t, plan(true, mod("a"), mod("b")), "grep -q a same && grep -q b same")
	r.script("mod-a", `echo a > same; commit a; say 好`)
	r.script("mod-b", `while [ "$(git show $(git branch --list 'agentq/*/all' --format='%(refname)'):same 2>/dev/null)" != a ]; do sleep 0.1; done; echo b > same; commit b; say 好`)
	r.script("conflict", `git merge $(git branch --list 'agentq/*/all' --format='%(refname:short)') >/dev/null 2>&1; printf 'a\nb\n' > same; commit 解; say 好`)
	r.agentq("do", "做")
	r.wait(order.Done)
	if r.count("conflict") != 1 {
		t.Errorf("%s", r.calls())
	}
}

// 做的时候主分支动了：合进来再跑一次测试才交付，别人的提交不丢。
func TestMainMoved(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), `echo x >> "`+"$HOME"+`/verified"; test -f a.txt`)
	r.script("mod-a", `echo a > a.txt; commit a; touch "$FAKE/in"; while [ ! -f "$FAKE/go" ]; do sleep 0.1; done; say 好`)
	r.agentq("do", "做")
	r.until(func() bool { _, err := os.Stat(filepath.Join(r.fake, "in")); return err == nil })
	r.write(filepath.Join(r.dir, "other.txt"), "别人的", 0o644)
	r.git(r.dir, "add", "-A")
	r.git(r.dir, "commit", "-qm", "别人")
	r.script("go", "")
	r.wait(order.Done)
	b, _ := os.ReadFile(filepath.Join(r.home, "verified"))
	if !r.has("a.txt") || !r.has("other.txt") || strings.Count(string(b), "x") != 2 {
		t.Errorf("%q", b)
	}
}

// 主目录有改动挡着：不覆盖，等它让开再合。
func TestDirtyMainWaits(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	r.write(filepath.Join(r.dir, "a.txt"), "用户没提交的", 0o644)
	r.agentq("do", "做")
	r.until(func() bool { o := r.order(); return o != nil && o.State == order.Landing && o.Why != "" })
	if got, _ := os.ReadFile(filepath.Join(r.dir, "a.txt")); string(got) != "用户没提交的" {
		t.Fatalf("盖掉了用户的：%s", got)
	}
	os.Remove(filepath.Join(r.dir, "a.txt"))
	o := r.order()
	r.agentq("stop", o.ID) // 一分钟才再试一次，停了接着做就马上试
	r.agentq("retry", o.ID)
	r.wait(order.Done)
	if got, _ := os.ReadFile(filepath.Join(r.dir, "a.txt")); string(got) != "a\n" {
		t.Errorf("%s", got)
	}
}

// 做到一半整个被杀（像重启）：resume 从那一步接着做，合好的不重做，没做完的续上会话。
func TestKilledAndResumed(t *testing.T) {
	r := newRig(t, plan(true, mod("a"), mod("b", "a")), "test -f b.txt")
	r.script("mod-b", `touch "$FAKE/in"; sleep 60`)
	r.script("again", `echo "$resume" > "$FAKE/resumed"; echo b > b.txt; commit b; say 好`)
	r.agentq("do", "做")
	r.until(func() bool { _, err := os.Stat(filepath.Join(r.fake, "in")); return err == nil })
	o := r.order()
	syscall.Kill(-o.PID, syscall.SIGKILL)
	r.until(func() bool { return !r.order().Running() })
	if out := r.agentq("ls"); !strings.Contains(out, "断了") {
		t.Errorf("%s", out)
	}
	r.agentq("resume")
	r.wait(order.Done)
	got, _ := os.ReadFile(filepath.Join(r.fake, "resumed"))
	if r.count("plan") != 1 || r.count("mod-a") != 1 || strings.TrimSpace(string(got)) != o.Tasks["b"].Session || o.Tasks["b"].Session == "" {
		t.Errorf("%s %q", r.calls(), got)
	}
}

// 停：会话一起停，主分支没动；接着做能做完。
func TestStop(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	r.script("mod-a", `echo $$ > "$FAKE/pid"; sleep 60`)
	r.script("again", `echo a > a.txt; commit a; say 好`)
	r.agentq("do", "做")
	var pid int
	r.until(func() bool {
		b, _ := os.ReadFile(filepath.Join(r.fake, "pid"))
		n, _ := fmt.Sscan(string(b), &pid)
		return n == 1
	})
	o := r.order()
	r.agentq("stop", o.ID)
	r.until(func() bool { return syscall.Kill(pid, 0) != nil })
	if o = r.order(); o.State != order.Stopped {
		t.Fatalf("%s", o.State)
	}
	r.agentq("resume")
	if r.order().Running() {
		t.Error("停了的不自动接着做")
	}
	r.agentq("retry", o.ID)
	r.wait(order.Done)
}

// 要用户本人做的：整件都是就说一声算完，不挂着等；只有一部分的，别的照做，交付消息里写上。
func TestNeedsUser(t *testing.T) {
	t.Run("整件都是", func(t *testing.T) {
		r := newRig(t, `{"small":true,"title":"付钱","summary":"","spec":"","stop":"要付款","later":[],"modules":[]}`, "true")
		r.agentq("do", "买个域名")
		if o := r.wait(order.Done); !strings.Contains(o.Why, "要你本人做：要付款") || r.count("plan") != 1 {
			t.Errorf("%s", o.Why)
		}
	})
	t.Run("只有一部分", func(t *testing.T) {
		r := newRig(t, strings.Replace(plan(true, mod("a")), `"stop":""`, `"stop":"上架要你登录账号"`, 1), "true")
		r.agentq("do", "做")
		if o := r.wait(order.Done); !strings.Contains(strings.Join(o.Risks, " "), "要你本人做：上架要你登录账号") || !r.has("a.txt") {
			t.Errorf("%+v", o.Risks)
		}
	})
}

// 会话没收干净、什么都没做，都说得出来。
func TestSessionLoose(t *testing.T) {
	t.Run("没提交的收一次尾", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "test -f a.txt")
		r.script("mod-a", `echo a > a.txt; say 好`)
		r.agentq("do", "做")
		r.wait(order.Done)
		if r.count("tidy") != 1 {
			t.Errorf("%s", r.calls())
		}
	})
	t.Run("什么都没做", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.script("mod-a", `say 不用改`)
		r.agentq("do", "做")
		if o := r.wait(order.Stuck); !strings.Contains(o.Why, "不用改") {
			t.Errorf("%s", o.Why)
		}
	})
}

// 十条一起开工也不限名额，互不影响。
func TestManyAtOnce(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	r.script("mod-a", `n=$(basename $(dirname $(dirname $(pwd)))); echo x > "$n.txt"; commit a; say 好`)
	for range 10 {
		r.agentq("do", "做")
	}
	end := time.Now().Add(90 * time.Second)
	for {
		done := 0
		for _, o := range (&order.Store{Root: r.home}).All() {
			if o.State == order.Stuck {
				t.Fatalf("%s", o.Why)
			}
			if o.State == order.Done && !o.Running() {
				done++
			}
		}
		if done == 10 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("只做完 %d 条\n%s", done, r.agentq("ls", "-a"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := len(strings.Fields(r.git(r.dir, "ls-files", "*.txt"))); n != 10 {
		t.Errorf("主分支上要有 10 条的成果：%d", n)
	}
}

func (r *rig) until(ok func() bool) {
	r.t.Helper()
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	r.t.Fatalf("等不到\n%s\n%s", r.calls(), r.agentq("ls", "-a"))
}

// Telegram 发一句话：只认用户，交给会话，回答发回去；收过的重启不重收。
func TestListen(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	r.script("chat", `say "好的"`)
	var mu sync.Mutex
	var said []string
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q.ParseForm()
		if strings.HasSuffix(q.URL.Path, "/sendMessage") {
			said = append(said, q.Form.Get("chat_id")+" "+q.Form.Get("text"))
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		offsets = append(offsets, q.Form.Get("offset"))
		if q.Form.Get("offset") == "0" {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":7,"message":{"text":"外人","from":{"id":2},"chat":{"id":2,"type":"private"}}},{"update_id":8,"message":{"text":"加个导出","from":{"id":1},"chat":{"id":1,"type":"private"}}},{"update_id":9,"message":{"text":"群里的","from":{"id":3},"chat":{"id":1,"type":"group"}}}]}`)
			return
		}
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()
	secret := filepath.Join(r.home, "tg")
	r.write(secret, "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n", 0o600)
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "listen")
		cmd.Env = append(os.Environ(), "AGENTQ_HOME="+r.home, "AGENTQ_CLAUDE="+filepath.Join(r.fake, "claude"), "AGENTQ_TELEGRAM="+secret, "AGENTQ_TELEGRAM_API="+srv.URL, "AGENTQ_PROJECTS="+filepath.Dir(r.dir))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	cmd := start()
	r.until(func() bool { // 发出去、记完了再杀
		left, _ := filepath.Glob(filepath.Join(r.home, "*box", "*.txt"))
		mu.Lock()
		defer mu.Unlock()
		return len(said) > 0 && len(left) == 0
	})
	cmd.Process.Kill()
	cmd.Wait()
	mu.Lock()
	offsets = nil
	mu.Unlock()
	cmd = start()
	r.until(func() bool { mu.Lock(); defer mu.Unlock(); return len(offsets) > 0 })
	cmd.Process.Kill()
	cmd.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(said) != 1 || !strings.HasPrefix(said[0], "1 <b>好的</b>\n<i>") || r.count("chat") != 1 || offsets[0] != "10" {
		t.Errorf("%v %v %s", said, offsets, r.calls())
	}
}

// 早报：卡住的放最前，昨天做好的、在做的、各项目用了多少都在；前天做好的不再提。
func TestBrief(t *testing.T) {
	s := &order.Store{Root: t.TempDir()}
	now := time.Now()
	add := func(dir, state string, ago time.Duration, tokens int64) {
		o, err := s.New(dir, "main", dir+state)
		if err != nil {
			t.Fatal(err)
		}
		o.Update(func() {
			o.State, o.Ended, o.Why = state, now.Add(-ago), "等你"
			o.Used = []order.Use{{At: now.Add(-ago), Tokens: tokens}}
		})
	}
	if got := brief(s, now); !strings.Contains(got, "没有派活") {
		t.Error(got)
	}
	add("/p/甲", order.Done, time.Hour, 2e6)
	add("/p/乙", order.Done, 48*time.Hour, 5e6)
	add("/p/丙", order.Stuck, 48*time.Hour, 1e6)
	got := brief(s, now)
	if strings.Contains(got, "乙") || strings.Index(got, "丙") > strings.Index(got, "甲") || !strings.Contains(got, "一天用了 2.0M token\n甲：2.0M") {
		t.Error(got)
	}
}

// 待办：全做时没互相等的同时开工，做完打勾，接着做被挡着的；卡住的、改名的不动。
func TestTodo(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	os.MkdirAll(filepath.Join(r.dir, "docs"), 0o755)
	r.write(filepath.Join(r.dir, "docs", "BACKLOG.md"), "## 待做\n\n- [ ] 甲 —— 做甲\n- [ ] 乙 —— 做乙\n- [ ] 丙 —— 做丙（依赖：甲）\n- [ ] 丁 —— （卡住：等人）\n\n## 已完成\n", 0o644)
	r.git(r.dir, "add", "-A")
	r.git(r.dir, "commit", "-qm", "待办")
	r.script("mod-a", `n=$(basename $(dirname $(dirname $(pwd)))); echo x > "$n.txt"; commit a; say "看这里"`)
	if out := r.agentq("todo"); !strings.Contains(out, "丙  等「甲」") || !strings.Contains(out, "丁  卡住") {
		t.Errorf("%s", out)
	}
	if out := r.agentq("todo", "all"); strings.Count(out, "开工") != 2 {
		t.Fatalf("甲乙一起开工：%s", out)
	}
	r.until(func() bool {
		n := 0
		for _, o := range (&order.Store{Root: r.home}).All() {
			if o.State == order.Done && !o.Running() {
				n++
			}
		}
		return n == 3
	})
	b, _ := os.ReadFile(filepath.Join(r.dir, "docs", "BACKLOG.md"))
	for _, want := range []string{"- [x] 甲 —— 看这里", "- [x] 乙 —— 看这里", "- [x] 丙 —— 看这里", "- [ ] 丁"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("缺「%s」：%s", want, b)
		}
	}
}

// 审计（2026-11-26，Codex）查出来的，每条一个用例。
func TestAuditRegressions(t *testing.T) {
	t.Run("不盖被忽略的文件", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.write(filepath.Join(r.dir, ".gitignore"), "a.txt\n", 0o644)
		r.git(r.dir, "commit", "-qam", "忽略")
		r.script("mod-a", `echo 新 > a.txt; git add -f a.txt; commit a; say 好`)
		r.write(filepath.Join(r.dir, "a.txt"), "个人的", 0o644)
		r.agentq("do", "做")
		r.until(func() bool { o := r.order(); return o != nil && o.State == order.Landing && o.Why != "" })
		if got, _ := os.ReadFile(filepath.Join(r.dir, "a.txt")); string(got) != "个人的" {
			t.Fatalf("盖掉了：%s", got)
		}
		r.agentq("stop", r.order().ID)
	})
	t.Run("接着做不丢现场的修改", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "test -f repair.txt")
		r.script("red", `echo $$ > try.txt; commit try; say 试了`)
		r.script("last", `echo $$ > try.txt; commit try; say 试了`)
		r.agentq("do", "做")
		o := r.wait(order.Stuck)
		r.write(filepath.Join(o.WorkDir(), "trees", "all", "repair.txt"), "人修的", 0o644)
		r.agentq("retry", o.ID)
		r.wait(order.Done)
		if !r.has("repair.txt") {
			t.Error("人修的丢了")
		}
	})
	t.Run("别人暂时看不到的worktree不动", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		away := filepath.Join(t.TempDir(), "盘")
		os.Mkdir(away, 0o755)
		r.git(r.dir, "worktree", "add", "-q", "-b", "mine", filepath.Join(away, "wt"))
		os.Rename(away, away+"-拔了")
		r.agentq("do", "做")
		r.wait(order.Done)
		os.Rename(away+"-拔了", away)
		r.git(filepath.Join(away, "wt"), "status")
	})
	t.Run("产品经理写不到用户目录", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.script("plan", `echo 误写 > draft.txt; say 方案 '`+plan(true, mod("a"))+`'`)
		r.write(filepath.Join(r.dir, "draft.txt"), "草稿", 0o644)
		r.agentq("do", "做")
		r.wait(order.Done)
		if got, _ := os.ReadFile(filepath.Join(r.dir, "draft.txt")); string(got) != "草稿" {
			t.Errorf("%s", got)
		}
	})
	t.Run("凭据不给项目脚本", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), `test -z "$CLAUDE_CODE_OAUTH_TOKEN"`)
		os.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "x") // 别的用例不看它
		r.script("mod-a", `[ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] && echo a > a.txt; commit a; say 好`)
		r.agentq("do", "做")
		r.wait(order.Done)
		if r.count("red") != 0 || !r.has("a.txt") {
			t.Errorf("%s", r.calls())
		}
	})
	t.Run("主管被杀后会话不跑两份", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.script("mod-a", `echo $$ > "$FAKE/pid"; sleep 60`)
		r.script("again", `echo a > a.txt; commit a; say 好`)
		r.agentq("do", "做")
		var pid int
		r.until(func() bool {
			b, _ := os.ReadFile(filepath.Join(r.fake, "pid"))
			n, _ := fmt.Sscan(string(b), &pid)
			return n == 1
		})
		syscall.Kill(r.order().PID, syscall.SIGKILL)
		r.until(func() bool { return !r.order().Running() })
		r.agentq("resume")
		r.wait(order.Done)
		if syscall.Kill(pid, 0) == nil {
			t.Error("上一份会话还活着")
		}
	})
	t.Run("没提交的待办不反复派", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		os.MkdirAll(filepath.Join(r.dir, "docs"), 0o755)
		r.write(filepath.Join(r.dir, "docs", "BACKLOG.md"), "## 待做\n\n- [ ] 甲 —— 做\n", 0o644)
		r.git(r.dir, "add", "-A")
		r.git(r.dir, "commit", "-qm", "待办")
		r.write(filepath.Join(r.dir, "docs", "BACKLOG.md"), "## 待做\n\n- [ ] 甲 —— 做\n- [ ] 乙 —— 没提交\n", 0o644)
		if out := r.agentq("todo", "all"); strings.Count(out, "开工") != 1 || !strings.Contains(out, "没提交") {
			t.Errorf("%s", out)
		}
		r.until(func() bool { o := r.order(); return o.State == order.Landing && o.Why != "" }) // 打勾要改的文件被挡着
		r.agentq("stop", r.order().ID)
		if out := r.agentq("todo", "all"); strings.Contains(out, " 开工：") {
			t.Errorf("停了的不自己再来：%s", out)
		}
	})
	t.Run("默认分支听远程的", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.git(r.dir, "branch", "-m", "trunk")
		clone := filepath.Join(t.TempDir(), "demo")
		r.git(r.dir, "clone", "-q", r.dir, clone)
		r.dir = clone
		r.git(clone, "config", "user.name", "t")
		r.git(clone, "config", "user.email", "t@t")
		r.git(clone, "checkout", "-qb", "feature")
		r.agentq("do", "做")
		r.wait(order.Done)
		r.git(clone, "cat-file", "-e", "trunk:a.txt")
		if r.has("a.txt") {
			t.Error("合到功能分支上了")
		}
	})
	t.Run("主分支检出在别的worktree", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.git(r.dir, "checkout", "-qb", "feature")
		other := filepath.Join(t.TempDir(), "main")
		r.git(r.dir, "worktree", "add", "-q", other, "main")
		r.agentq("do", "做")
		r.wait(order.Done)
		if _, err := os.Stat(filepath.Join(other, "a.txt")); err != nil {
			t.Error(err)
		}
	})
	t.Run("等着合的不挡别人", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.write(filepath.Join(r.dir, "a.txt"), "用户的", 0o644)
		r.agentq("do", "甲")
		r.until(func() bool { o := r.order(); return o != nil && o.State == order.Landing && o.Why != "" })
		first := r.order().ID
		r.script("mod-a", `echo b > b.txt; commit b; say 好`)
		r.agentq("do", "乙")
		r.wait(order.Done)
		r.agentq("stop", first)
	})
	t.Run("没有检查脚本要补上", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.git(r.dir, "rm", "-q", ".agentq/verify")
		r.git(r.dir, "commit", "-qm", "没有检查")
		r.script("red", `mkdir -p .agentq; printf '#!/bin/sh\ntest -f a.txt\n' > .agentq/verify; chmod +x .agentq/verify; commit 补; say 好`)
		r.agentq("do", "做")
		r.wait(order.Done)
		if r.count("red") != 1 {
			t.Errorf("%s", r.calls())
		}
	})
	t.Run("审计的提交不算数", func(t *testing.T) {
		r := newRig(t, plan(false, mod("a")), "true")
		r.script("audit", `echo 乱 > audit.txt; commit 乱; say 看完 '{"summary":"好","bugs":[],"risks":[]}'`)
		r.agentq("do", "做")
		r.wait(order.Done)
		if r.has("audit.txt") {
			t.Error("进了主分支")
		}
	})
	bugs := `say 看完 '{"summary":"有","bugs":[{"module":"a","where":"a.txt:1","what":"坏了","how":"看"}],"risks":[]}'`
	t.Run("没修好不说修好", func(t *testing.T) {
		r := newRig(t, plan(false, mod("a")), "true")
		r.script("audit", bugs)
		r.script("fix", `say 没修 '{"results":[{"result":"failed","note":"修不动"}]}'`)
		r.agentq("do", "做")
		if o := r.wait(order.Stuck); !strings.Contains(o.Why, "修不动") {
			t.Errorf("%s", o.Why)
		}
	})
	t.Run("超出这次的记进待办不停下", func(t *testing.T) {
		r := newRig(t, plan(false, mod("a")), "true")
		r.script("audit", bugs)
		r.script("fix", `say 以后做 '{"results":[{"result":"later","note":"安卓全屏 —— 局域网要 https，验收：安卓能全屏打开"}]}'`)
		r.agentq("do", "做")
		o := r.wait(order.Done)
		b, _ := os.ReadFile(filepath.Join(r.dir, "docs", "BACKLOG.md"))
		if !strings.Contains(string(b), "- [ ] 安卓全屏 —— 局域网要 https") || len(o.Risks) == 0 || !strings.Contains(o.Risks[len(o.Risks)-1], "安卓全屏") {
			t.Errorf("%s %v", b, o.Risks)
		}
	})
	t.Run("卡住后用户自己合了就算做完", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "exit 1")
		r.script("red", `say 试了`)
		r.script("last", `say 试了`)
		r.agentq("do", "做")
		o := r.wait(order.Stuck)
		r.git(r.dir, "merge", "-q", "--no-edit", "agentq/"+o.ID+"/all")
		r.agentq("resume")
		if o = r.order(); o.State != order.Done || o.Why != "你手动合进主分支了" {
			t.Errorf("%s %s", o.State, o.Why)
		}
	})
	t.Run("不成立的照实说", func(t *testing.T) {
		r := newRig(t, plan(false, mod("a")), "true")
		r.script("audit", bugs)
		r.script("fix", `say 不成立 '{"results":[{"result":"invalid","note":"复现不了"}]}'`)
		r.agentq("do", "做")
		if o := r.wait(order.Done); o.Invalid != 1 {
			t.Errorf("%+v", o)
		}
	})
	t.Run("模块合了没记上照样接着走", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a"), mod("b", "a")), "true")
		r.script("mod-b", `touch "$FAKE/in"; sleep 60`)
		r.script("again", `[ -f b.txt ] || { echo b > b.txt; git add b.txt; git -c user.name=t -c user.email=t@t commit -qm b; }; say 早就做好了`)
		r.agentq("do", "做")
		r.until(func() bool { _, err := os.Stat(filepath.Join(r.fake, "in")); return err == nil })
		o := r.order()
		syscall.Kill(-o.PID, syscall.SIGKILL)
		r.until(func() bool { return !r.order().Running() })
		o = r.order()
		o.Update(func() { o.Tasks["a"].Merged = false })
		r.agentq("resume")
		r.wait(order.Done)
	})
	t.Run("合进去的就是测过的", func(t *testing.T) {
		// 打勾会碰坏检查：要在合之前发现、修好
		r := newRig(t, plan(true, mod("a")), `git rev-parse HEAD >> "$HOME/verified"; ! grep -q "\[x\]" docs/BACKLOG.md || test -f red.txt`)
		os.MkdirAll(filepath.Join(r.dir, "docs"), 0o755)
		r.write(filepath.Join(r.dir, "docs", "BACKLOG.md"), "## 待做\n\n- [ ] 甲 —— 做甲\n\n## 已完成\n", 0o644)
		r.git(r.dir, "add", "-A")
		r.git(r.dir, "commit", "-qm", "待办")
		r.agentq("todo", "all")
		r.wait(order.Done)
		tested := strings.Fields(read(filepath.Join(r.home, "verified")))
		if head := r.git(r.dir, "rev-parse", "HEAD"); tested[len(tested)-1] != head || !r.has("red.txt") {
			t.Errorf("合进去的 %s 没测过：%v %s", head, tested, r.calls())
		}
		if !strings.Contains(read(filepath.Join(r.dir, "docs", "BACKLOG.md")), "- [x] 甲") {
			t.Error("没打勾")
		}
	})
	t.Run("用户仓库里不放东西", func(t *testing.T) {
		r := newRig(t, plan(true, mod("a")), "true")
		r.script("mod-a", `touch "$FAKE/in"; while [ ! -f "$FAKE/go" ]; do sleep 0.1; done; echo a > a.txt; commit a; say 好`)
		r.agentq("do", "做")
		r.until(func() bool { _, err := os.Stat(filepath.Join(r.fake, "in")); return err == nil })
		if s := r.git(r.dir, "status", "--porcelain", "--ignored"); s != "" {
			t.Errorf("%s", s)
		}
		r.script("go", "")
		r.wait(order.Done)
	})
}

// 发不出去的先留着，resume 补发；处理到一半被杀的消息重启后接着处理。
func TestMessagesNotLost(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	var mu sync.Mutex
	down, said := true, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(q.URL.Path, "/getUpdates"):
			time.Sleep(50 * time.Millisecond)
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		case down:
			w.WriteHeader(503)
		default:
			said++
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	secret := filepath.Join(r.home, "tg")
	r.write(secret, "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n", 0o600)
	envs := append(os.Environ(), "AGENTQ_HOME="+r.home, "AGENTQ_CLAUDE="+filepath.Join(r.fake, "claude"), "AGENTQ_TELEGRAM="+secret, "AGENTQ_TELEGRAM_API="+srv.URL, "AGENTQ_PROJECTS="+filepath.Dir(r.dir))
	run := func(args ...string) *exec.Cmd {
		cmd := exec.Command(bin, args...)
		cmd.Env = envs
		return cmd
	}
	run("brief").Run()
	mu.Lock()
	down = false
	mu.Unlock()
	if out, err := run("resume").CombinedOutput(); err != nil || said != 1 {
		t.Fatalf("%v %s %d", err, out, said)
	}
	r.script("chat", `say 好的`)
	os.MkdirAll(filepath.Join(r.home, "inbox"), 0o700)
	r.write(filepath.Join(r.home, "inbox", "42.txt"), "加个导出", 0o600)
	cmd := run("listen")
	cmd.Start()
	defer cmd.Process.Kill()
	r.until(func() bool { mu.Lock(); defer mu.Unlock(); return said == 2 })
	if out, err := run("listen").CombinedOutput(); err == nil || !strings.Contains(string(out), "已经有一个") {
		t.Errorf("同时只能一个在收：%s", out)
	}
}

// Telegram 拒收（比如 chat id 填错）：当场报出来、留着，改好了补发；一条不收不挡着后面的。
func TestRejectedMessage(t *testing.T) {
	home := t.TempDir()
	var mu sync.Mutex
	var said []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q.ParseForm()
		if q.Form.Get("chat_id") != "1" || strings.Contains(q.Form.Get("text"), "不收") {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
			return
		}
		said = append(said, q.Form.Get("text"))
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "AGENTQ_TELEGRAM_API=" + srv.URL}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	left := func() int {
		f, _ := filepath.Glob(filepath.Join(home, ".agentq2", "outbox", "*.txt"))
		return len(f)
	}
	secret := filepath.Join(home, ".agentq2", "telegram.env")
	put(t, secret, "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=2\n")
	if out, err := run("brief"); err == nil || !strings.Contains(out, "chat not found") || left() != 1 {
		t.Errorf("要报错、留着：%v %s 留了 %d 条", err, out, left())
	}
	put(t, secret, "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n")
	put(t, filepath.Join(home, ".agentq2", "outbox", "1.txt"), "不收的")
	if out, err := run("resume"); err != nil || !strings.Contains(out, "chat not found") {
		t.Errorf("%v %s", err, out)
	}
	if len(said) != 1 || !strings.Contains(said[0], "早报") || left() != 1 {
		t.Errorf("早报要补发，不收的留着：%v 留了 %d 条", said, left())
	}
}

// 新用户没有 ~/projects：收到的消息照样处理。
func TestListenNoProjects(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	r.script("chat", `say "没有在做的"`)
	var mu sync.Mutex
	var said []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if q.ParseForm(); strings.HasSuffix(q.URL.Path, "/sendMessage") {
			said = append(said, q.Form.Get("text"))
		}
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()
	put(t, filepath.Join(r.home, ".agentq2", "telegram.env"), "TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n")
	put(t, filepath.Join(r.home, ".agentq2", "inbox", "7.txt"), "进度怎样")
	cmd := exec.Command(bin, "listen")
	cmd.Env = []string{"HOME=" + r.home, "PATH=" + os.Getenv("PATH"), "AGENTQ_CLAUDE=" + filepath.Join(r.fake, "claude"), "AGENTQ_TELEGRAM_API=" + srv.URL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		n := len(said)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("没处理：%s", r.calls())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(said[0], "<b>没有在做的</b>") || !strings.Contains(r.calls(), "/projects ") {
		t.Errorf("%v %s", said, r.calls())
	}
}

// 自我进化：agentq 自己没跑顺的记下来，早报里报；回「修」就对自己的源码派一条，合进去后编新程序换掉自己。项目自己的事（测试修不好）不算。
func TestSelf(t *testing.T) {
	r := newRig(t, plan(true, mod("a")), "true")
	r.write(filepath.Join(r.dir, "go.mod"), "module agentq\n\ngo 1.27.4\n", 0o644)
	r.write(filepath.Join(r.dir, "main.go"), "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"新的 agentq\") }\n", 0o644)
	r.git(r.dir, "add", "-A")
	r.git(r.dir, "commit", "-qm", "源码")
	r.exe = filepath.Join(r.home, "agentq") // 换掉的是这一份，别的用例用的不动
	b, _ := os.ReadFile(bin)
	r.write(r.exe, string(b), 0o755)
	r.extra = []string{"AGENTQ_SOURCE=" + r.dir, "GOTOOLCHAIN=" + os.Getenv("GOTOOLCHAIN")}
	r.script("mod-a", `say 不用改`)
	r.agentq("do", "做")
	r.wait(order.Stuck)
	if out := r.agentq("brief"); !strings.Contains(out, "🔧 agentq 自己") || !strings.Contains(out, "什么都没提交") {
		t.Fatalf("%s", out)
	}
	os.Remove(filepath.Join(r.fake, "mod-a"))
	if out := r.agentq("self"); !strings.Contains(out, " 开工：") {
		t.Fatalf("%s", out)
	}
	r.wait(order.Done)
	r.until(func() bool {
		out, _ := exec.Command(r.exe).Output()
		return strings.Contains(string(out), "新的 agentq")
	})
	if out := r.agentq("brief"); strings.Contains(out, "🔧") {
		t.Errorf("交出去修过的不再报：%s", out)
	}
}

// 守住小：产品代码不超过 5000 行。要加东西先删东西。
func TestSmall(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "*.go").Output()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(b), "\n")
	}
	if t.Log(n, "行"); n > 5000 {
		t.Errorf("产品代码 %d 行，超过 5000：先删再加", n)
	}
}
