package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 暴力测试。AGENTQ_STRESS=1 才跑；默认 go test 跳过。真的起子进程：编译好的 agentq（-race）、编译好的假 claude、
// 假 Telegram、假 ASC、假 gh、假 launchctl，全部用临时 HOME 和临时 projects_root。
//
//	AGENTQ_STRESS=1 AGENTQ_STRESS_SEED=42 AGENTQ_STRESS_ROUNDS=60 GOTOOLCHAIN=go1.27.3 go test -race -run '^TestStress$' -timeout 2h -v ./tests
//
// AGENTQ_STRESS_ONLY=<场景名> 只跑一个场景；AGENTQ_STRESS_NORACE=1 编译不带 -race 的 agentq（调试场景时快一点）；
// AGENTQ_STRESS_KEEP=1 不删现场。出问题时打印种子、轮次和场景；同一种子同一轮的破坏序列相同（进程调度的时序当然不同）。
// 只杀测试自己起的进程：按 pid（来自假 claude 的记录）或环境里带本轮唯一 HOME 的进程，绝不按名字杀。

var (
	stressOnce    sync.Once
	stressBinPath string
	stressFakeBin string
	stressBinErr  error
)

func stressBuild(t *testing.T) (agentq, fake string) {
	t.Helper()
	stressOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agentq-stress-bin-")
		if err != nil {
			stressBinErr = err
			return
		}
		stressBinPath = filepath.Join(dir, "agentq")
		args := []string{"build", "-o", stressBinPath}
		if os.Getenv("AGENTQ_STRESS_NORACE") == "" {
			args = append(args, "-race")
		}
		if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
			stressBinErr = fmt.Errorf("编译 agentq：%v\n%s", err, out)
			return
		}
		src := filepath.Join(dir, "fake")
		os.MkdirAll(src, 0o755)
		os.WriteFile(filepath.Join(src, "main.go"), []byte(stressFakeSrc), 0o644)
		os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644)
		stressFakeBin = filepath.Join(dir, "claude")
		c := exec.Command("go", "build", "-o", stressFakeBin, ".")
		c.Dir = src
		if out, err := c.CombinedOutput(); err != nil {
			stressBinErr = fmt.Errorf("编译假 claude：%v\n%s", err, out)
		}
	})
	if stressBinErr != nil {
		t.Fatal(stressBinErr)
	}
	return stressBinPath, stressFakeBin
}

// ---- 假 Telegram：可以切成各种坏法 ----

const (
	tgOK int32 = iota
	tg5xx
	tg429
	tgHang
	tgHalf
	tg400HTML
	tgGarbage
)

const stressToken = "99887766:STRESSTOKENMARKERabcdefghijklmnop"
const stressOwner = int64(777001)

type stressSent struct {
	Method  string
	Text    string
	ReplyTo string
}

type stressMarkup struct {
	ID     int64
	Markup string
	Text   string
}

type stressTG struct {
	markups []stressMarkup
	answers []string
	edits   int
	srv     *httptest.Server
	mu      sync.Mutex
	updates []map[string]any
	sent    []stressSent
	nextID  int64
	mode    atomic.Int32
	nextUp  int64
	badHTML atomic.Int64 // 带 parse_mode=HTML 却解析不了的次数
	tooLong atomic.Int64 // 超过 4096 字的次数
	closed  atomic.Bool
}

func newStressTG() *stressTG {
	f := &stressTG{nextID: 100000, nextUp: 1000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *stressTG) close() {
	f.closed.Store(true)
	f.srv.CloseClientConnections()
	f.srv.Close()
}

var tgTagRe = regexp.MustCompile(`</?([a-zA-Z]*)[^<>]*>`)

// tgHTMLOK 照 Telegram 的规矩：只认这几个标签，要配对；裸的 < 和 & 不行。
func tgHTMLOK(s string) bool {
	var stack []string
	for _, m := range tgTagRe.FindAllStringSubmatchIndex(s, -1) {
		tag := s[m[0]:m[1]]
		name := s[m[2]:m[3]]
		switch name {
		case "b", "i", "code", "pre", "a", "u", "s", "em", "strong", "blockquote":
		default:
			return false
		}
		if strings.HasPrefix(tag, "</") {
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return false
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, name)
		}
	}
	if len(stack) > 0 {
		return false
	}
	rest := tgTagRe.ReplaceAllString(s, "")
	if strings.ContainsAny(rest, "<>") {
		return false
	}
	return !regexp.MustCompile(`&($|[^a-z#]|[a-z#]*($|[^a-z0-9;]))`).MatchString(rest)
}

func (f *stressTG) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	_ = r.ParseMultipartForm(80 << 20)
	if strings.HasPrefix(r.URL.Path, "/file/bot"+stressToken+"/") {
		w.Write([]byte("hello"))
		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+stressToken+"/")
	if !ok {
		reply(404, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
		return
	}
	switch f.mode.Load() {
	case tg5xx:
		w.WriteHeader(502)
		w.Write([]byte("<html><body>bad gateway</body></html>"))
		return
	case tg429:
		reply(429, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 30", "parameters": map[string]any{"retry_after": 30}})
		return
	case tgHang:
		select {
		case <-r.Context().Done():
		case <-time.After(25 * time.Second):
		}
		return
	case tgHalf:
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true,"result":[{"update_id":`))
		return
	case tgGarbage:
		w.WriteHeader(200)
		w.Write([]byte("\x00\xff not json"))
		return
	}
	switch method {
	case "sendMessage":
		text := r.Form.Get("text")
		if f.mode.Load() == tg400HTML && r.Form.Get("parse_mode") == "HTML" {
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: can't parse entities: Unsupported start tag"})
			return
		}
		if r.Form.Get("parse_mode") == "HTML" && !tgHTMLOK(text) {
			f.badHTML.Add(1)
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: can't parse entities: unsupported start tag"})
			return
		}
		plain := text
		if r.Form.Get("parse_mode") == "HTML" {
			plain = textfmt.HTMLToPlain(text)
		}
		if strings.TrimSpace(plain) == "" {
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message text is empty"})
			return
		}
		if utf8.RuneCountInString(plain) > 4096 {
			f.tooLong.Add(1)
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message is too long"})
			return
		}
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.sent = append(f.sent, stressSent{Method: method, Text: text, ReplyTo: r.Form.Get("reply_parameters")})
		if mk := r.Form.Get("reply_markup"); mk != "" {
			f.markups = append(f.markups, stressMarkup{ID: id, Markup: mk, Text: text})
		}
		f.mu.Unlock()
		reply(200, map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
	case "sendPhoto", "sendVideo", "sendDocument":
		text := r.Form.Get("caption")
		if r.MultipartForm != nil {
			for _, fhs := range r.MultipartForm.File {
				for _, fh := range fhs {
					text += " 文件:" + fh.Filename
					if rd, err := fh.Open(); err == nil {
						var buf bytes.Buffer
						buf.ReadFrom(rd)
						rd.Close()
						text += " " + buf.String()
					}
				}
			}
		}
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.sent = append(f.sent, stressSent{Method: method, Text: text})
		f.mu.Unlock()
		reply(200, map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
	case "answerCallbackQuery":
		f.mu.Lock()
		f.answers = append(f.answers, r.Form.Get("text"))
		f.mu.Unlock()
		reply(200, map[string]any{"ok": true, "result": true})
	case "editMessageText":
		f.mu.Lock()
		f.edits++
		f.mu.Unlock()
		reply(200, map[string]any{"ok": true, "result": map[string]any{"message_id": r.Form.Get("message_id")}})
	case "getFile":
		reply(200, map[string]any{"ok": true, "result": map[string]any{"file_id": r.Form.Get("file_id"), "file_size": 5, "file_path": "documents/f.bin"}})
	case "getUpdates":
		var off int64
		fmt.Sscan(r.Form.Get("offset"), &off)
		var wait int
		fmt.Sscan(r.Form.Get("timeout"), &wait)
		deadline := time.Now().Add(min(time.Duration(wait)*time.Second, 800*time.Millisecond))
		for {
			f.mu.Lock()
			var out []map[string]any
			for _, u := range f.updates {
				if id := u["update_id"].(int64); id >= off && len(out) < 50 {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if len(out) > 0 || !time.Now().Before(deadline) || r.Context().Err() != nil || f.closed.Load() {
				if out == nil {
					out = []map[string]any{}
				}
				reply(200, map[string]any{"ok": true, "result": out})
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	default:
		reply(404, map[string]any{"ok": false, "error_code": 404, "description": "Not Found: " + method})
	}
}

// push 排一条更新。
func (f *stressTG) push(chat, from int64, text string, replyTo int64, at time.Time) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUp++
	f.nextID++
	m := map[string]any{"message_id": f.nextID, "date": at.Unix(), "text": text, "chat": map[string]any{"id": chat, "type": "private"}}
	if from != 0 {
		m["from"] = map[string]any{"id": from, "is_bot": false}
	}
	if replyTo != 0 {
		m["reply_to_message"] = map[string]any{"message_id": replyTo, "date": at.Unix(), "text": "之前的消息", "chat": map[string]any{"id": chat}}
	}
	f.updates = append(f.updates, map[string]any{"update_id": f.nextUp, "message": m})
	return f.nextID
}

// pushCallback 排一个按钮回调。chat 是 0 时不带 message（真的会有：消息太旧时 Telegram 不给）。
func (f *stressTG) pushCallback(from, chat, msgID int64, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUp++
	q := map[string]any{"id": fmt.Sprintf("cb%d", f.nextUp), "from": map[string]any{"id": from, "is_bot": false}, "data": data}
	if chat != 0 {
		q["message"] = map[string]any{"message_id": msgID, "date": time.Now().Unix(), "chat": map[string]any{"id": chat}}
	}
	f.updates = append(f.updates, map[string]any{"update_id": f.nextUp, "callback_query": q})
}

// pushDoc 排一条带文件的更新。
func (f *stressTG) pushDoc(name string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUp++
	f.nextID++
	m := map[string]any{"message_id": f.nextID, "date": time.Now().Unix(), "caption": "看看这个", "chat": map[string]any{"id": stressOwner, "type": "private"},
		"from": map[string]any{"id": stressOwner}, "document": map[string]any{"file_id": fmt.Sprintf("f%d", f.nextUp), "file_name": name, "file_size": size}}
	f.updates = append(f.updates, map[string]any{"update_id": f.nextUp, "message": m})
}

func (f *stressTG) sentCopy() []stressSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stressSent(nil), f.sent...)
}

// ---- 环境 ----

type stressEnv struct {
	t       *testing.T
	rng     *rand.Rand
	base    string
	home    string
	root    string
	bin     string
	fakeBin string
	binDir  string
	path    string
	tg      *stressTG
	tuning  string // config.TestTuningEnv 的值，writeConfig 从 cfg 里分出来
	cfg     map[string]any
	extra   []string
	mu      sync.Mutex
	bad     []string
	items   map[string]string // 条目号 → 项目目录；破坏后还要在
	skipTok map[string]bool   // 这些项目的 BACKLOG 是场景故意毁掉的
	daemons []*exec.Cmd
	// 上限
	modelCap int  // 模型调用总数
	limitCap int  // 一直报撞上限的条目的模型调用数（0 = 和别的一样）
	tokCap   int  // 同一条目的模型调用数
	tgCap    int  // 发出的消息总数
	sameCap  int  // 同一段文字发出的次数
	noState  bool // 状态文件被场景故意弄坏（不断言可解析）
	// ahead：测试把钟拨快了多少。退避等的是拨过的钟，只能往前拨。
	ahead    time.Duration
	noConfig bool
}

var stressPanicRe = regexp.MustCompile(`(?m)^panic: |^fatal error: |DATA RACE|^goroutine \d+ \[running|signal SIGSEGV|^runtime: `)

func newStressEnv(t *testing.T, rng *rand.Rand, homeName string) *stressEnv {
	t.Helper()
	bin, fake := stressBuild(t)
	base, err := os.MkdirTemp("", "aq-")
	if err != nil {
		t.Fatal(err)
	}
	base, _ = filepath.EvalSymlinks(base)
	if homeName == "" {
		homeName = "home"
	}
	e := &stressEnv{t: t, rng: rng, base: base, home: filepath.Join(base, homeName), root: filepath.Join(base, "projects"),
		bin: bin, fakeBin: fake, binDir: filepath.Join(base, "bin"), items: map[string]string{}, skipTok: map[string]bool{},
		modelCap: 300, tokCap: 8, limitCap: 40, tgCap: 400, sameCap: 40}
	for _, d := range []string{e.home, e.root, e.binDir, filepath.Join(e.home, ".agentq"), filepath.Join(e.home, "fake")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.path = e.binDir + ":/usr/bin:/bin:/usr/sbin:/sbin"
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	// 假 gh、假 launchctl
	os.WriteFile(filepath.Join(e.binDir, "gh"), []byte("#!/bin/sh\necho '[]'\n"), 0o755)
	os.WriteFile(filepath.Join(e.binDir, "launchctl"), []byte("#!/bin/sh\nprintf 'PID\\tStatus\\tLabel\\n'\n"), 0o755)
	e.tg = newStressTG()
	max, per := 2+rng.Intn(2), 1+rng.Intn(2)
	e.cfg = map[string]any{
		"projects_root": e.root, "claude_path": e.fakeBin, "tick_after_job": rng.Intn(3) > 0,
		"patrol_enabled": false, "notify_enabled": true, "max_parallel": max, "per_project": per,
		"timeout": "7s", "limit_cooldown": "1s", "breaker_cooldown": "1s", "breaker_max": "2s",
	}
	e.limits(max, per)
	e.writeConfig()
	return e
}

func (e *stressEnv) limits(max, per int) {
	os.WriteFile(filepath.Join(e.home, "fake", "limits"), []byte(fmt.Sprintf("%d %d\n", max, per)), 0o644)
}

func (e *stressEnv) writeConfig() {
	cfg, tuning := splitTuning(e.cfg)
	e.tuning = tuning
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "config.json"), b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *stressEnv) telegram() {
	dir := filepath.Join(e.home, ".agentq", "secrets")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "telegram.env"), []byte(fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", stressToken, stressOwner)), 0o600)
}

func (e *stressEnv) violate(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if strings.HasPrefix(msg, "[") { // 同一个问题收尾时会再查到一次
		if _, rest, ok := strings.Cut(msg, "] "); ok {
			for _, b := range e.bad {
				if strings.HasSuffix(b, rest) {
					return
				}
			}
		}
	}
	if len(e.bad) < 200 {
		e.bad = append(e.bad, msg)
	}
}

func (e *stressEnv) environ() []string {
	mac := filepath.Join(e.home, "fake", "macnotify.log")
	return append([]string{"HOME=" + e.home, "PATH=" + e.path, "AGENTQ_TELEGRAM_API=" + e.tg.srv.URL, config.TestTuningEnv + "=" + e.tuning,
		"AGENTQ_MACNOTIFY_LOG=" + mac, "AGENTQ_TEST_LAUNCHCTL=" + filepath.Join(e.binDir, "launchctl"), "GORACE=exitcode=66", "LC_ALL=en_US.UTF-8",
		"TMPDIR=" + os.TempDir()}, e.extra...)
}

// agentq 跑一条命令，带硬超时：超时当死锁（发 SIGQUIT 留下栈，再杀进程组）。
func (e *stressEnv) agentq(limit time.Duration, args ...string) (string, int) {
	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		e.violate("起不来 agentq %v：%v", args, err)
		return "", -1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		_ = cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-done
		}
		e.violate("agentq %v 超过 %s 没结束（疑似死锁）：\n%s", args, limit, tailOf(buf.String(), 4000))
		return buf.String(), -2
	}
	out := buf.String()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if f, ferr := os.OpenFile(filepath.Join(e.base, "agentq-outs.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil { // 留现场时查原因用
		fmt.Fprintf(f, "$ agentq %s（退出码 %d）\n%s\n", strings.Join(args, " "), code, tailOf(out, 20000))
		f.Close()
	}
	e.scanOut(fmt.Sprintf("agentq %v", args), out)
	if code == 66 {
		e.violate("agentq %v 有数据竞争（exit 66）：\n%s", args, tailOf(out, 4000))
	}
	return out, code
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (e *stressEnv) scanOut(what, out string) {
	if loc := stressPanicRe.FindStringIndex(out); loc != nil {
		e.violate("%s 输出里有崩溃：\n%s", what, tailOf(out[max(0, loc[0]-200):], 3000))
	}
}

// startBG 起一个不等的进程。
func (e *stressEnv) startBG(args ...string) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	buf := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		e.violate("起不来 agentq %v：%v", args, err)
		return nil, buf
	}
	return cmd, buf
}

// tickKilled 起一个 tick，过一会儿 SIGKILL（只杀这一个 pid；它起的任务外壳在自己的进程组里）。
func (e *stressEnv) tickKilled(delay time.Duration) {
	cmd, buf := e.startBG("tick")
	if cmd == nil {
		return
	}
	time.Sleep(delay)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	e.scanOut("被杀的 tick", buf.String())
}

// listen 起常驻的 agentq listen。
func (e *stressEnv) listen() *exec.Cmd {
	cmd, _ := e.startBG("listen")
	if cmd != nil {
		e.daemons = append(e.daemons, cmd)
	}
	return cmd
}

func (e *stressEnv) stopDaemons() {
	for _, c := range e.daemons {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
		if b, ok := c.Stdout.(*bytes.Buffer); ok {
			e.scanOut("agentq listen", b.String())
		}
	}
	e.daemons = nil
}

// git 在项目目录跑 git（测试搭环境用）。
func (e *stressEnv) git(dir string, args ...string) (string, error) {
	c := exec.Command("/usr/bin/git", append([]string{"-C", dir}, args...)...)
	c.Env = []string{"HOME=" + e.home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}
	out, err := c.CombinedOutput()
	return string(out), err
}

var stressTokN atomic.Int64

func (e *stressEnv) tok() string {
	return fmt.Sprintf("ITEM-%sx%d", strconv.FormatInt(e.rng.Int63n(1<<30), 36), stressTokN.Add(1))
}

func (e *stressEnv) item(mode string) (line, tok string) {
	tok = e.tok()
	return fmt.Sprintf("- [ ] 条目 %s —— MODE=%s 验收：看着办", tok, mode), tok
}

const stressBacklogTpl = "# 待办\n\n## 待做\n\n%s\n## 等用户决定\n\n## 以后再说\n\n## 已完成\n"

// repo 建一个真 git 项目，待做里放 lines。
func (e *stressEnv) repo(name string, lines ...string) string {
	return e.repoBody(name, fmt.Sprintf(stressBacklogTpl, strings.Join(lines, "\n")+"\n"), lines)
}

func (e *stressEnv) repoBody(name, body string, lines []string) string {
	e.t.Helper()
	dir := filepath.Join(e.root, name)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
	os.WriteFile(filepath.Join(dir, ".agentq", "verify"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if out, err := e.git(dir, "init", "-q", "-b", "main"); err != nil {
		e.t.Fatalf("git init：%v %s", err, out)
	}
	e.git(dir, "add", "-A")
	if out, err := e.git(dir, "commit", "-q", "-m", "init"); err != nil {
		e.t.Fatalf("git commit：%v %s", err, out)
	}
	for _, l := range lines {
		if t := stressTokRe.FindString(l); t != "" {
			e.items[t] = dir
		}
	}
	return dir
}

var stressTokRe = regexp.MustCompile(`ITEM-[a-z0-9]+`)

type stressCall struct {
	Ev     string `json:"ev"`
	Pid    int    `json:"pid"`
	Ppid   int    `json:"ppid"`
	Pgid   int    `json:"pgid"`
	Kind   string `json:"kind"`
	Tok    string `json:"tok"`
	Mode   string `json:"mode"`
	Job    string `json:"job"`
	Resume string `json:"resume"`
	// Takeover：接手任务的提示词。
	Takeover bool `json:"takeover"`
}

func (e *stressEnv) calls() []stressCall {
	b, _ := os.ReadFile(filepath.Join(e.home, "fake", "calls.log"))
	var out []stressCall
	for _, l := range strings.Split(string(b), "\n") {
		var c stressCall
		if json.Unmarshal([]byte(l), &c) == nil && c.Ev != "" {
			out = append(out, c)
		}
	}
	return out
}

func (e *stressEnv) liveClaudes() []stressCall {
	var out []stressCall
	for _, c := range e.calls() {
		if c.Ev == "start" && syscall.Kill(c.Pid, 0) == nil {
			out = append(out, c)
		}
	}
	return out
}

func (e *stressEnv) state() (*State, error) {
	return state.Load(filepath.Join(e.home, ".agentq", "state.json"))
}

// homeProcs：环境里带本轮 HOME 的进程（ps -E 看环境，假 claude、agentq 都看得到）。
func (e *stressEnv) homeProcs() map[int]string {
	c := exec.Command("/bin/ps", "-A", "-E", "-ww", "-o", "pid=,stat=,command=")
	c.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8")
	out, _ := c.Output()
	res := map[int]string{}
	mark := "HOME=" + e.home + " "
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.Contains(l, mark) {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 3 || strings.HasPrefix(f[1], "Z") {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		s := strings.Join(f[2:], " ")
		if i := strings.Index(s, " HOME="); i > 0 {
			s = s[:i]
		}
		res[pid] = textfmt.TruncRunes(s, 120)
	}
	return res
}

// checkLeaks：等一会儿，还活着的就是泄漏，记下来并清掉。
func (e *stressEnv) checkLeaks(grace time.Duration) {
	deadline := time.Now().Add(grace)
	var procs map[int]string
	for {
		procs = e.homeProcs()
		if len(procs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	var names []string
	for pid, c := range procs {
		names = append(names, fmt.Sprintf("%d %s", pid, c))
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	e.violate("结束后还有进程没退出：%s", strings.Join(names, "；"))
}

// cleanupProcs：不算违规地清掉（失败路径）。
func (e *stressEnv) cleanupProcs() {
	for pid := range e.homeProcs() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// drain 暂停接活，tick 到没有运行中任务。
func (e *stressEnv) drain(limit time.Duration) {
	e.agentq(90*time.Second, "pause")
	deadline := time.Now().Add(limit)
	for {
		e.agentq(200*time.Second, "tick")
		st, err := e.state()
		if err == nil && len(st.Running()) == 0 && len(gateBusy(st)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				e.violate("状态文件一直读不出：%v", err)
				return
			}
			var names []string
			for _, j := range st.Running() {
				names = append(names, j.ID+"/"+j.Status+fmt.Sprintf("/pid%d", j.PID))
			}
			names = append(names, gateBusy(st)...)
			e.violate("%s 后还有运行中任务没收尾：%v", limit, names)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// gateBusy：还在排队或在过门、会自己有结论的任务。主目录挡着的（不在 main、index 被锁）要等用户，不算。
func gateBusy(st *State) []string {
	var out []string
	for _, j := range st.Jobs {
		if j.Gate != nil && !j.Gate.WaitNoted && (j.Gate.State == state.GateQueued || j.Gate.State == state.GateRunning) {
			out = append(out, j.ID+"/门 "+j.Gate.State)
		}
	}
	return out
}

// walkHome 扫 HOME 下所有文件找不该出现的串（秘密），跳过放秘密本身的地方。
func (e *stressEnv) secretsScan() {
	markers := []string{stressToken, "STRESSTOKENMARKER"}
	hit := func(where string, b []byte) {
		for _, m := range markers {
			if bytes.Contains(b, []byte(m)) {
				e.violate("秘密 %s… 出现在 %s", textfmt.TruncRunes(m, 12), where)
			}
		}
	}
	_ = filepath.WalkDir(e.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(e.home, p)
		if d.IsDir() {
			if rel == ".appstoreconnect" || rel == "fake" || rel == filepath.Join(".agentq", "secrets") || rel == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if fi, err := d.Info(); err != nil || !fi.Mode().IsRegular() || fi.Size() > 8<<20 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil {
			hit(rel, b)
		}
		return nil
	})
	for _, s := range e.tg.sentCopy() {
		hit("Telegram 消息", []byte(s.Text))
	}
}

// checkInvariants：每轮结束时都断言。
func (e *stressEnv) checkInvariants(phase string) {
	// 崩溃：launchd.log（外壳和外壳触发的 tick 的输出）
	if b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "launchd.log")); err == nil {
		if loc := stressPanicRe.FindIndex(b); loc != nil {
			e.violate("[%s] launchd.log 里有崩溃：\n%s", phase, tailOf(string(b[max(0, loc[0]-200):min(len(b), loc[0]+3000)]), 3200))
		}
	}
	// 假 claude 当场记下的重复、超并发
	if b, err := os.ReadFile(filepath.Join(e.home, "fake", "violations.log")); err == nil && len(b) > 0 {
		e.violate("[%s] %s", phase, tailOf(string(b), 1500))
		os.Remove(filepath.Join(e.home, "fake", "violations.log"))
	}
	// 状态文件能解析；同指纹最多一个；并发不超
	if !e.noState {
		if st, err := e.state(); err != nil {
			e.violate("[%s] 状态文件读不出：%v", phase, err)
		} else {
			seen := map[string]string{}
			perProject := map[string]int{}
			for _, j := range st.Running() {
				if j.Fingerprint != "" {
					if other, ok := seen[j.Fingerprint]; ok {
						e.violate("[%s] 同一指纹两个运行中任务：%s %s", phase, other, j.ID)
					}
					seen[j.Fingerprint] = j.ID
				}
				if state.TakesSlot(j.Kind) {
					perProject[j.Project]++
				}
			}
			total := 0
			for project, n := range perProject {
				total += n
				if per, ok := e.cfg["per_project"].(int); ok && n > per {
					e.violate("[%s] 项目 %s 有 %d 个任务在跑，超过 per_project=%d", phase, project, n, per)
				}
			}
			if limit, ok := e.cfg["max_parallel"].(int); ok && total > limit {
				e.violate("[%s] %d 个任务在跑，超过 max_parallel=%d", phase, total, limit)
			}
		}
	}
	// 配置能解析、没被写成空
	if !e.noConfig {
		b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "config.json"))
		var m map[string]any
		if err != nil || json.Unmarshal(b, &m) != nil || len(m) == 0 {
			e.violate("[%s] config.json 坏了：%v %q", phase, err, textfmt.TruncRunes(string(b), 100))
		} else if m["projects_root"] != e.root {
			e.violate("[%s] config.json 的 projects_root 变了：%v", phase, m["projects_root"])
		}
	}
	// BACKLOG 条目不丢
	for tok, dir := range e.items {
		if e.skipTok[tok] {
			continue
		}
		p := filepath.Join(dir, "docs", "BACKLOG.md")
		b, err := os.ReadFile(p)
		if err != nil {
			e.violate("[%s] %s 读不了：%v", phase, p, err)
			continue
		}
		// 修回归的标题带着原条目的名字，不算这一条
		n := 0
		for _, line := range bytes.Split(b, []byte("\n")) {
			if bytes.Contains(line, []byte(" "+tok+" ")) && !bytes.Contains(line, []byte("修回归：")) {
				n++
			}
		}
		if n != 1 {
			e.violate("[%s] 条目 %s 在 %s 里出现 %d 次：\n%s", phase, tok, filepath.Base(dir), n, tailOf(string(b), 600))
		}
	}
	// 模型写的东西不能改 BACKLOG 的结构：节标题数不变
	for _, dir := range e.items {
		if b, err := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md")); err == nil {
			if bytes.Count(b, []byte("假的完成")) > 0 && bytes.Contains(b, []byte("\n- [x] 假的完成")) {
				e.violate("[%s] 模型写的文字变成了 BACKLOG 里的一行「已完成」", phase)
			}
			if n := bytes.Count(b, []byte("\n## 已完成")); n > 1 {
				e.violate("[%s] %s 的 BACKLOG 多出了 %d 个「## 已完成」", phase, filepath.Base(dir), n)
			}
		}
	}
	// 模型调用数
	starts, perTok := 0, map[string]int{}
	modeOf := map[string]string{}
	readyTok := map[string]bool{}
	for _, c := range e.calls() {
		if c.Ev == "ready" {
			readyTok[c.Tok] = true
		}
		if c.Ev == "start" {
			starts++
			if c.Tok != "" {
				perTok[c.Tok]++
				modeOf[c.Tok] = c.Mode
				// 写了 ready.json 之后只有退回（同一会话）和接手（进程在交付后被杀）才会再起
				if readyTok[c.Tok] && c.Resume == "" && !c.Takeover {
					e.violate("[%s] 条目 %s 已经交付了又被从头启动（任务 %s）", phase, c.Tok, c.Job)
				}
			}
		}
	}
	if starts > e.modelCap {
		e.violate("[%s] 模型调用 %d 次，超过上限 %d", phase, starts, e.modelCap)
	}
	for tok, n := range perTok {
		// 一直报撞上限、一直 5xx 的（没花钱，每次冷却或退避过后再试一次）另算：时间快进时每个 tick 都过了等待
		if (modeOf[tok] == "limit" || modeOf[tok] == "e529") && e.limitCap > 0 && n <= e.limitCap {
			continue
		}
		if n > e.tokCap {
			e.violate("[%s] 条目 %s 被启动 %d 次，超过上限 %d", phase, tok, n, e.tokCap)
		}
	}
	// 通知不失控
	sent := e.tg.sentCopy()
	if len(sent) > e.tgCap {
		e.violate("[%s] 发出 %d 条消息，超过上限 %d", phase, len(sent), e.tgCap)
	}
	same := map[string]int{}
	for _, s := range sent {
		same[s.Text+"|"+s.ReplyTo]++
	}
	for text, n := range same {
		if n > e.sameCap {
			e.violate("[%s] 同一条消息发了 %d 次：%s", phase, n, textfmt.TruncRunes(text, 120))
			break
		}
	}
	if n := e.tg.tooLong.Load(); n > 0 {
		e.violate("[%s] 给 Telegram 发过 %d 次超长消息（会一直失败）", phase, n)
	}
	e.secretsScan()
}

// finish 收尾：断言、清理、留现场。
func (e *stressEnv) finish(name string) {
	e.stopDaemons()
	e.checkLeaks(30 * time.Second)
	e.checkInvariants(name)
	e.tg.close()
	if len(e.bad) > 0 || os.Getenv("AGENTQ_STRESS_KEEP") != "" {
		e.t.Logf("现场留在 %s", e.base)
	} else {
		chmodAll(e.base)
		os.RemoveAll(e.base)
	}
	for _, b := range e.bad {
		e.t.Errorf("%s", b)
	}
}

func chmodAll(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}

// ---- 场景 ----

type stressScenario struct {
	name   string
	weight int
	fn     func(t *testing.T, rng *rand.Rand)
}

var stressTaskModes = []string{"ok", "ok", "slow", "slow", "hang", "quick", "half", "nonjson", "exit3", "orphan", "claimdie", "claimwait",
	"limit", "limitmid", "budget", "e529", "e403", "badturns", "badapi", "badstruct", "badresult", "badorigin", "selfkill", "killshell", "stderrflood", "hangterm", "orphanterm", "selfmerge"}

func pick[T any](rng *rand.Rand, s []T) T { return s[rng.Intn(len(s))] }

// seedProjects 建 n 个项目，每个 2-3 条随机方式的待做。
func (e *stressEnv) seedProjects(n int, modes []string) {
	for i := 0; i < n; i++ {
		var lines []string
		for k := 0; k < 2+e.rng.Intn(2); k++ {
			l, _ := e.item(pick(e.rng, modes))
			lines = append(lines, l)
		}
		e.repo(fmt.Sprintf("proj%d", i), lines...)
	}
}

// 进程：任务运行中随机 SIGKILL 外壳、claude、tick；假 claude 各种死法。
func scenarioKill(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("kill")
	e.seedProjects(3+rng.Intn(3), stressTaskModes)
	deadline := time.Now().Add(time.Duration(6+rng.Intn(8)) * time.Second)
	for time.Now().Before(deadline) {
		switch rng.Intn(10) {
		case 0, 1, 2:
			e.agentq(120*time.Second, "tick")
		case 3, 4, 5:
			e.tickKilled(time.Duration(rng.Intn(500)) * time.Millisecond)
		case 6:
			if l := e.liveClaudes(); len(l) > 0 {
				_ = syscall.Kill(pick(rng, l).Pid, syscall.SIGKILL)
			}
		case 7:
			if l := e.liveClaudes(); len(l) > 0 {
				// 父进程是外壳（ppid 属于本轮时才杀，已被 launchd 收养的不杀）
				if c := pick(rng, l); c.Ppid > 1 {
					if _, mine := e.homeProcs()[c.Ppid]; mine {
						_ = syscall.Kill(c.Ppid, syscall.SIGKILL)
					}
				}
			}
		case 8:
			switch rng.Intn(3) {
			case 0:
				e.agentq(60*time.Second, "new", fmt.Sprintf("做个工具 %d", rng.Int63()))
			default:
				if st, err := e.state(); err == nil && len(st.Running()) > 0 {
					e.agentq(90*time.Second, "stop", pick(rng, st.Running()).ID)
				}
			}
		default:
			time.Sleep(time.Duration(rng.Intn(300)) * time.Millisecond)
		}
	}
	e.drain(60 * time.Second)
	e.checkInvariants("kill 收尾后")
	// 恢复：之后 tick 能拿到锁并正常完成。任务外壳结束时的 tick --after 可能正拿着锁，这次跳过是正常的；一直拿不到才算问题。
	out, code := e.agentq(120*time.Second, "tick")
	for deadline := time.Now().Add(30 * time.Second); code == 0 && strings.Contains(out, "上一个 tick 还没结束") && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		out, code = e.agentq(120*time.Second, "tick")
	}
	if code != 0 || !strings.Contains(out, "运行中") {
		e.violate("破坏后 tick 没正常完成（退出码 %d）：\n%s", code, tailOf(out, 1000))
	}
}

// 专打启动窗口：tick 刚把外壳和 claude 起来、还没写进状态文件时被杀，下一次 tick 不能再起一个同样的。
func scenarioKillStart(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("killstart")
	e.cfg["timeout"] = "20s"
	e.writeConfig()
	for i := 0; i < 8; i++ {
		l, _ := e.item(pick(rng, []string{"slow", "killtick", "killtick"}))
		e.repo(fmt.Sprintf("s%d", i), l)
	}
	for i := 0; i < 25; i++ {
		e.tickKilled(time.Duration(rng.Intn(350)) * time.Millisecond)
		if rng.Intn(2) == 0 {
			e.tickKilled(time.Duration(rng.Intn(350)) * time.Millisecond)
		}
		e.agentq(120*time.Second, "tick")
		e.agentq(60*time.Second, "status")
	}
	e.drain(60 * time.Second)
	e.checkInvariants("killstart")
}

// 时间快进：几周的 tick 压在一轮里，走孤儿接手、标卡住、退避重试、过期清理这些按时间才触发的路径。
func scenarioTimelapse(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("timelapse")
	e.telegram()
	e.cfg["patrol_enabled"] = rng.Intn(2) == 0
	e.cfg["timeout"], e.cfg["breaker_cooldown"], e.cfg["breaker_max"], e.cfg["limit_cooldown"] = "4h", "30m", "4h", "1h"
	e.writeConfig()
	modes := []string{"quick", "claimdie", "claimwait", "limit", "limitmid", "budget", "e529", "e403", "half", "exit3", "ok", "orphan"}
	for i, m := range modes {
		l, _ := e.item(m)
		e.repo(fmt.Sprintf("t%02d", i), l)
	}
	stepH := 3 + rng.Intn(9)
	steps := 80
	// 孤儿、接手要真的分支：有一条标记写的分支真的有内容
	withBranch, _ := e.item("ok")
	dir := e.repo("branchy", withBranch+"（进行中：agent/real 2020-01-01 10:00）")
	e.git(dir, "checkout", "-q", "-b", "agent/real")
	os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x"), 0o644)
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", "work")
	e.git(dir, "checkout", "-q", "main")
	days := float64(steps*stepH) / 24
	e.tokCap = int(days*2) + 8
	e.limitCap = steps + 8
	e.modelCap = (len(modes)+1)*e.tokCap + steps + 40
	e.tgCap = 2000
	e.sameCap = 60
	base := time.Now()
	for i := 0; i < steps; i++ {
		e.extra = []string{"AGENTQ_TEST_NOW=" + base.Add(time.Duration(i*stepH)*time.Hour).Format(time.RFC3339)}
		e.agentq(200*time.Second, "tick")
		if i%7 == 0 {
			e.agentq(60*time.Second, "status")
		}
		// 让这一批结束（秒退的），接下来的 tick 收尾
		for k := 0; k < 30; k++ {
			if l := e.liveClaudes(); len(l) == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	e.agentq(60*time.Second, "pause")
	e.agentq(200*time.Second, "tick")
	e.drain(60 * time.Second)
	e.extra = nil
	e.agentq(60*time.Second, "stats")
	e.checkInvariants("timelapse")
	// state.json 不能越涨越大：30 天前结束的要清掉
	if fi, err := os.Stat(filepath.Join(e.home, ".agentq", "state.json")); err == nil && fi.Size() > 8<<20 {
		e.violate("state.json 涨到 %d 字节", fi.Size())
	}
}

// 假 claude 的每种结束方式都走一遍。
func scenarioModes(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("modes")
	// 门进程计入 max_parallel：比以前的 4 多两个名额给门，不然 50MB 结果占掉大半时限后接不完全部条目（种子 894 第 29 轮）
	e.cfg["max_parallel"], e.cfg["per_project"] = 6, 1
	e.limits(6, 1)
	e.writeConfig()
	modes := append([]string(nil), stressTaskModes...)
	if rng.Intn(4) == 0 {
		modes = append(modes, "big", "bigjson")
	}
	rng.Shuffle(len(modes), func(i, k int) { modes[i], modes[k] = modes[k], modes[i] })
	want := map[string]string{}
	for i, m := range modes {
		l, tok := e.item(m)
		want[tok] = m
		e.repo(fmt.Sprintf("m%02d", i), l)
	}
	// 50MB 的结果在 -race 下一次 tick 要一两分钟（不带 -race 3 秒），所以时限放宽
	deadline := time.Now().Add(280 * time.Second)
	for time.Now().Before(deadline) {
		start := time.Now()
		e.agentq(280*time.Second, "tick")
		if d := time.Since(start); d > 200*time.Second {
			e.violate("一次 tick 用了 %s，一直占着锁", d)
		}
		st, err := e.state()
		if err == nil {
			started := map[string]bool{}
			for _, c := range e.calls() {
				if c.Ev == "start" && c.Tok != "" { // 审查门的进程没有条目，不算：算上了少接一条就当接完了
					started[c.Tok] = true
				}
			}
			if len(st.Running()) == 0 && len(started) >= len(want) {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.drain(40 * time.Second)
	e.agentq(60*time.Second, "stats", "90")
	e.checkInvariants("modes")
	started := map[string]bool{}
	for _, c := range e.calls() {
		if c.Ev == "start" {
			started[c.Tok] = true
		}
	}
	for tok, m := range want {
		if !started[tok] {
			e.violate("方式 %s 的条目 %s 始终没启动", m, tok)
		}
	}
	st, _ := e.state()
	for _, j := range st.Jobs {
		if m := want[stressTokRe.FindString(j.Item)]; m == "ok" && j.Status != state.StatusDone {
			e.violate("正常结束的任务状态是 %s：%s", j.Status, j.Summary)
		}
	}
}

// 并发：大量命令同时抢 tick.lock、notify.lock、inbox.lock。
func scenarioConcurrent(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("concurrent")
	e.telegram()
	e.writeConfig()
	modes := []string{"ok", "ok", "slow", "quick", "claimwait", "orphan"}
	e.seedProjects(3, modes)
	var stop atomic.Bool
	var wg sync.WaitGroup
	// 盯着文件：配置、BACKLOG、状态永远不能半截或变空
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			for _, p := range []string{"config.json", "state.json", "notify.json"} {
				b, err := os.ReadFile(filepath.Join(e.home, ".agentq", p))
				if err != nil {
					continue
				}
				if !json.Valid(b) {
					e.violate("并发时 %s 读到不完整内容（%d 字节）", p, len(b))
				}
			}
			for _, dir := range e.items {
				if b, err := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md")); err != nil || len(b) == 0 {
					e.violate("并发时 %s 的 BACKLOG 读到空的：%v", filepath.Base(dir), err)
				}
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()
	cmds := [][]string{{"tick"}, {"tick"}, {"tick"}, {"status"}, {"notify", "send", "done", "并发消息"}, {"notify", "status"},
		{"pause"}, {"resume"}, {"resume"}, {"stats"}, {"run", "proj0"}, {"run", "proj1"}, {"run", "proj2"},
		{"new", "做个小工具"}, {"logs", "nosuchjob"}, {"stop", "nosuchjob"}, {"backlog", "archive", "proj0", "--dry-run"}}
	e.tokCap = 60 // agentq run 不看 retry_after，这里在猛按它
	msgRng := rand.New(rand.NewSource(rng.Int63()))
	for w := 0; w < 12; w++ {
		wg.Add(1)
		sub := rand.New(rand.NewSource(rng.Int63()))
		go func() {
			defer wg.Done()
			for !stop.Load() {
				e.agentq(200*time.Second, pick(sub, cmds)...)
			}
		}()
	}
	e.listen()
	e.listen() // 第二个要等第一个退出，不能两个一起收
	// 消息也同时来
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			e.tg.push(stressOwner, stressOwner, pick(msgRng, []string{"状态", "额度", "在跑什么", "你好", "暂停", "恢复"}), 0, time.Now())
			time.Sleep(time.Duration(50) * time.Millisecond)
		}
	}()
	time.Sleep(time.Duration(8+rng.Intn(8)) * time.Second)
	stop.Store(true)
	wg.Wait()
	e.stopDaemons()
	e.drain(70 * time.Second)
	e.checkInvariants("concurrent")
}

type stressCorrupt struct {
	name string
	fn   func(path string, rng *rand.Rand) // 写坏 path
	// unread：读都读不了（目录、000），项目规则是当未知，修好才恢复
	unread bool
}

func stressCorruptions() []stressCorrupt {
	write := func(s string) func(string, *rand.Rand) {
		return func(p string, _ *rand.Rand) {
			os.RemoveAll(p)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(s), 0o644)
		}
	}
	return []stressCorrupt{
		{name: "garbage", fn: func(p string, r *rand.Rand) {
			b := make([]byte, 4096)
			r.Read(b)
			os.RemoveAll(p)
			os.WriteFile(p, b, 0o644)
		}},
		{name: "truncate", fn: func(p string, _ *rand.Rand) {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				os.Truncate(p, fi.Size()/2)
				return
			}
			os.WriteFile(p, []byte(`{"jobs":[{"id":"x`), 0o644)
		}},
		{name: "empty", fn: write("")},
		{name: "null", fn: write("null")},
		{name: "emptyobj", fn: write("{}")},
		{name: "array", fn: write("[]")},
		{name: "halfjob", fn: write(`{"jobs":[{"id":"x","status":"runn`)},
		{name: "wrongtype", fn: write(`{"jobs":"no","pending":5,"offers":{},"next":"x","items":3,"cooldown_until":7}`)},
		{name: "nullelem", fn: write(`{"jobs":[null],"pending":[null],"offers":[null],"items":[null],"apps":[null]}`)},
		{name: "huge", fn: func(p string, _ *rand.Rand) {
			os.RemoveAll(p)
			f, err := os.Create(p)
			if err == nil {
				f.Truncate(100 << 20)
				f.Close()
			}
		}},
		{name: "readonly", fn: func(p string, _ *rand.Rand) {
			if _, err := os.Stat(p); err != nil {
				os.WriteFile(p, []byte("{}"), 0o644)
			}
			os.Chmod(p, 0o444)
		}},
		{name: "dir", unread: true, fn: func(p string, _ *rand.Rand) { os.RemoveAll(p); os.MkdirAll(filepath.Join(p, "x"), 0o755) }},
		{name: "noperm", unread: true, fn: func(p string, _ *rand.Rand) {
			if _, err := os.Stat(p); err != nil {
				os.WriteFile(p, []byte("{}"), 0o644)
			}
			os.Chmod(p, 0)
		}},
	}
}

func stressRestore(p string) {
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		os.Chmod(p, 0o755) // 目录没有 x 权限删不掉
		os.RemoveAll(p)
		return
	}
	os.Chmod(p, 0o644)
}

// 状态文件：写坏、截断、清空、变目录、只读、巨大。
func scenarioCorrupt(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("corrupt")
	e.telegram()
	e.writeConfig()
	var lines []string
	for i := 0; i < 2; i++ {
		l, _ := e.item("slow")
		lines = append(lines, l)
	}
	e.repo("alpha", lines...)
	l, _ := e.item("slow")
	e.repo("beta", l)
	e.agentq(120*time.Second, "tick") // 有运行中任务，也写出 notify.json
	data := filepath.Join(e.home, ".agentq")
	st, _ := e.state()
	targets := []string{"state.json", "notify.json", "config.json", "suggestions.json", "meta", "jobdir", "tick.lock", "notify.lock", "telegram.env", "patrol.json"}
	tgt := pick(rng, targets)
	cor := pick(rng, stressCorruptions())
	var path string
	switch tgt {
	case "telegram.env":
		path = filepath.Join(data, "secrets", "telegram.env")
	case "jobdir":
		if st == nil || len(st.Jobs) == 0 {
			tgt, path = "state.json", filepath.Join(data, "state.json")
		} else {
			path = filepath.Join(data, "jobs", pick(rng, st.Jobs).ID)
			cor = stressCorrupt{name: "删掉", fn: func(p string, _ *rand.Rand) { os.RemoveAll(p) }}
		}
	case "meta":
		if st == nil || len(st.Jobs) == 0 {
			tgt, path = "state.json", filepath.Join(data, "state.json")
		} else {
			path = filepath.Join(data, "jobs", pick(rng, st.Jobs).ID, "meta.json")
		}
	default:
		path = filepath.Join(data, tgt)
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	var cfgSaved []byte
	if tgt == "config.json" {
		cfgSaved, _ = os.ReadFile(path)
		e.noConfig = true
	}
	if tgt == "state.json" {
		e.noState = true
	}
	e.t.Logf("破坏 %s：%s", tgt, cor.name)
	cor.fn(path, rng)
	before, _ := os.ReadFile(path)
	// 破坏之后：什么命令都试一遍，不能崩、不能卡
	e.tg.push(stressOwner, stressOwner, "状态", 0, time.Now())
	e.tg.push(stressOwner, stressOwner, "你好", 0, time.Now())
	e.tg.push(stressOwner, stressOwner, "做 1", 0, time.Now())
	for i := 0; i < 2; i++ {
		e.agentq(200*time.Second, "tick")
		for _, c := range [][]string{{"status"}, {"notify", "status"}, {"notify", "send", "done", "破坏后的消息"},
			{"run", "alpha"}, {"stats"}, {"stats", "30"}} {
			e.agentq(200*time.Second, c...)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if tgt == "config.json" {
		after, _ := os.ReadFile(path)
		if cor.name != "readonly" && !bytes.Equal(before, after) {
			e.violate("写坏的 config.json 被改写了（%d → %d 字节）", len(before), len(after))
		}
		stressRestore(path)
		if err := os.WriteFile(path, cfgSaved, 0o600); err != nil {
			e.violate("恢复配置失败：%v", err)
		}
		e.noConfig = false
	} else if cor.unread {
		stressRestore(path)
	} else if strings.HasSuffix(tgt, ".lock") {
		stressRestore(path)
	}
	if tgt == "state.json" && cor.name != "readonly" && !cor.unread {
		// 内容坏了：下一次 tick 必须恢复（有备份），不是永远卡住
		e.agentq(200*time.Second, "tick")
		if _, err := e.state(); err != nil {
			e.violate("state.json（%s）坏了两次 tick 后还读不出：%v", cor.name, err)
		}
		e.noState = false
	}
	if tgt == "state.json" && cor.unread {
		e.noState = false
	}
	if tgt == "state.json" {
		stressRestore(path)
		e.noState = false
	}
	e.drain(70 * time.Second)
	e.checkInvariants("corrupt " + tgt + " " + cor.name)
	if fails := countStateLines(filepath.Join(data, "launchd.log"), "写状态文件失败"); fails > 20 {
		e.violate("写状态文件失败刷了 %d 行", fails)
	}
	// 修好之后 tick 能正常走完
	out, code := e.agentq(200*time.Second, "tick")
	if code != 0 || !strings.Contains(out, "运行中") {
		e.violate("破坏 %s（%s）修好后 tick 没正常完成（退出码 %d）：\n%s", tgt, cor.name, code, tailOf(out, 1000))
	}
}

func countStateLines(path, s string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), s)
}

// BACKLOG 的各种烂法。
func scenarioBacklog(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("backlog")
	e.cfg["max_parallel"] = 4
	e.limits(4, 2)
	e.writeConfig()
	type variant struct {
		name string
		fn   func(name string)
	}
	item := func(mode string) (string, string) { return e.item(mode) }
	// 一个「孤儿」标记：很久以前认领、没人管，触发去标记 / 接手 / 标卡住的路径，让 agentq 自己去改这些烂 BACKLOG
	orphan := func() (string, string) {
		l, t := item("ok")
		return l + "（进行中：agent/gone 2020-01-01 10:00）", t
	}
	mix := func() []string {
		a, _ := item("ok")
		b, _ := orphan()
		c, _ := item("slow")
		return []string{a, b, c}
	}
	variants := []variant{
		{"malformed", func(n string) {
			ls := mix()
			body := "# 待办\n## 待做\n- [ ]\n- [ ]    \n- []  没空格\n-[ ] x\n## \n## 待做\n" + strings.Join(ls, "\n") + "\n<!-- 没关的注释\n- [ ] 注释里的\n"
			e.repoBody(n, body, ls)
			for _, l := range ls {
				e.skipTok[stressTokRe.FindString(l)] = false
			}
		}},
		{"long10m", func(n string) {
			a, _ := item("ok")
			b, t := item("ok")
			e.repo(n, b+" "+strings.Repeat("长", 3<<20), a)
			e.skipTok[t] = false
		}},
		{"long200k", func(n string) {
			a, _ := item("slow")
			e.repo(n, a+" "+strings.Repeat("长", 70<<10))
		}},
		{"long900k", func(n string) {
			a, _ := item("slow")
			e.repo(n, a+" "+strings.Repeat("长", 300<<10))
		}},
		{"crlf", func(n string) {
			ls := mix()
			e.repoBody(n, strings.ReplaceAll(fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), "\n", "\r\n"), ls)
		}},
		{"nosection", func(n string) {
			ls := mix()
			e.repoBody(n, strings.Join(ls, "\n")+"\n", ls)
		}},
		{"binary", func(n string) {
			b := make([]byte, 8192)
			rng.Read(b)
			ls := mix()
			nul, _ := item("ok")
			body := "# 待办\n## 待做\n" + nul + "\x00\x01\x02\n" + strings.Join(ls, "\n") + "\n" + string(b)
			e.repoBody(n, body, append(ls, nul))
		}},
		{"badutf8", func(n string) {
			ls := mix()
			for i := range ls {
				ls[i] += " \xff\xfe\xc0"
			}
			e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
		}},
		{"conflict", func(n string) {
			ls := mix()
			body := "# 待办\n\n## 待做\n\n<<<<<<< HEAD\n" + ls[0] + "\n=======\n" + ls[1] + "\n>>>>>>> agent/x\n" + ls[2] + "\n\n## 已完成\n"
			e.repoBody(n, body, ls)
		}},
		{"notrepo", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			os.RemoveAll(filepath.Join(d, ".git"))
		}},
		{"brokengit", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			os.WriteFile(filepath.Join(d, ".git", "HEAD"), []byte("garbage\n"), 0o644)
			os.RemoveAll(filepath.Join(d, ".git", "objects"))
		}},
		{"otherbranch", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			e.git(d, "checkout", "-q", "-b", "feature")
		}},
		{"detached", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			e.git(d, "checkout", "-q", "--detach")
		}},
		{"nomain", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			e.git(d, "branch", "-q", "-m", "main", "trunk")
		}},
		{"symlink", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			p := filepath.Join(d, "docs", "BACKLOG.md")
			real := filepath.Join(d, "docs", "real-backlog.md")
			os.Rename(p, real)
			os.Symlink("real-backlog.md", p)
			e.git(d, "add", "-A")
			e.git(d, "commit", "-q", "-m", "symlink")
		}},
		{"dangling", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			p := filepath.Join(d, "docs", "BACKLOG.md")
			os.Remove(p)
			os.Symlink("/nonexistent/backlog.md", p)
			for _, l := range ls {
				e.skipTok[stressTokRe.FindString(l)] = true
			}
		}},
		{"isdir", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			p := filepath.Join(d, "docs", "BACKLOG.md")
			os.Remove(p)
			os.MkdirAll(filepath.Join(p, "x"), 0o755)
			for _, l := range ls {
				e.skipTok[stressTokRe.FindString(l)] = true
			}
		}},
		{"dirtybacklog", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			f, _ := os.OpenFile(filepath.Join(d, "docs", "BACKLOG.md"), os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString("没提交的改动\n")
			f.Close()
		}},
		{"indexlock", func(n string) {
			ls := mix()
			d := e.repoBody(n, fmt.Sprintf(stressBacklogTpl, strings.Join(ls, "\n")+"\n"), ls)
			os.WriteFile(filepath.Join(d, ".git", "index.lock"), nil, 0o644)
		}},
	}
	n := 2 + rng.Intn(3)
	used := map[string]string{}
	for i := 0; i < n; i++ {
		v := pick(rng, variants)
		name := fmt.Sprintf("b%d-%s", i, v.name)
		e.t.Logf("BACKLOG：%s", name)
		v.fn(name)
		used[name] = v.name
	}
	// 留一个好项目：别的项目再烂，它也要被做到
	good, goodTok := e.item("ok")
	e.repo("zz-good", good)
	symBefore := map[string]bool{}
	for name, v := range used {
		if v == "symlink" {
			symBefore[name] = true
		}
	}
	// 运行中改写、删除
	for i := 0; i < 6; i++ {
		e.agentq(200*time.Second, "tick")
		e.agentq(60*time.Second, "status")
		if i == 2 {
			for name, v := range used {
				if v != "malformed" {
					continue
				}
				p := filepath.Join(e.root, name, "docs", "BACKLOG.md")
				b, _ := os.ReadFile(p)
				os.WriteFile(p, append(b, []byte("\n追加一行\n")...), 0o644) // 运行中被改写
			}
		}
		time.Sleep(time.Duration(300+rng.Intn(700)) * time.Millisecond)
	}
	e.drain(70 * time.Second)
	e.checkInvariants("backlog")
	for name := range symBefore {
		p := filepath.Join(e.root, name, "docs", "BACKLOG.md")
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			e.violate("%s 的 BACKLOG 原来是软链接，被 agentq 换成了普通文件", name)
		}
	}
	found := false
	for _, c := range e.calls() {
		found = found || (c.Ev == "start" && c.Tok == goodTok)
	}
	if !found {
		st, _ := e.state()
		e.violate("别的项目 BACKLOG 烂了，好项目的条目 %s 也没被做（熔断冷却只有 1 秒）：%+v", goodTok, st)
	}
}

// 消息：洪水、超长、HTML、未知回复 id、别人发的、Telegram 各种坏。
func scenarioMessages(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("messages")
	e.telegram()
	e.cfg["max_parallel"] = 3
	e.writeConfig()
	os.WriteFile(filepath.Join(e.home, "fake", "defmode"), []byte(pick(rng, []string{"", "", "html", "long", "quick", "half", "exit3", "badstruct", "badstruct2", "nonjson"})), 0o644)
	l, _ := e.item("slow")
	e.repo("alpha", l)
	data := filepath.Join(e.home, ".agentq")
	offer := int64(555) // 回复一条没登记的消息
	b, _ := json.Marshal(notify.State{UpdateOffset: 1})
	os.WriteFile(filepath.Join(data, "notify.json"), b, 0o600)
	// 长消息走推送队列：一条发不出去的不能堵死同类的后面所有消息
	e.agentq(60*time.Second, "notify", "send", "done", strings.Repeat("很长的一句话", 1500))
	e.agentq(60*time.Second, "notify", "send", "patrol", "短标题", strings.Repeat("补充", 5000))
	flood := 150
	if rng.Intn(4) == 0 {
		flood = 1000
	}
	e.modelCap = 80
	e.tgCap = flood*3 + 100
	e.sameCap = flood + 20
	texts := []string{"状态", "status", "/status", "状态。", "额度", "额度？", "在跑什么", "暂停", "恢复", "暂停？", "恢复吧", "停 1234", "停 下来", "停 0101-000000-x", "做 1", "做 1 3", "做 999",
		"发", "发。", "发吧", " 发 ", "发\n", "<b>x</b>", "<script>alert(1)</script>", "&amp; & < >", strings.Repeat("长", 4096), strings.Repeat("超长", 6000), "你好", "‮倒着写", "🙂", "\x00空字节", "</i></b>"}
	replyTos := []int64{0, 0, 0, 0, offer, 99999, 1}
	var wg sync.WaitGroup
	var stop atomic.Bool
	if rng.Intn(2) == 0 {
		e.listen()
	}
	wg.Add(1)
	tickRng := rand.New(rand.NewSource(rng.Int63()))
	go func() {
		defer wg.Done()
		for !stop.Load() {
			e.agentq(200*time.Second, "tick")
			time.Sleep(time.Duration(50+tickRng.Intn(200)) * time.Millisecond)
		}
	}()
	modes := []int32{tgOK, tgOK, tgOK, tg5xx, tg429, tgHalf, tg400HTML, tgGarbage}
	if rng.Intn(3) == 0 {
		modes = append(modes, tgHang)
	}
	sub := rand.New(rand.NewSource(rng.Int63()))
	for i := 0; i < flood; i++ {
		text, to := pick(sub, texts), pick(sub, replyTos)
		chat, from := stressOwner, stressOwner
		switch sub.Intn(12) {
		case 0:
			chat, from = 123456, 123456
			text = "OTHERCHATMARKER-不该"
		case 1:
			from = 0 // 频道转发，没有 from
		case 2:
			from = 424242 // 同一个 chat 里别的人
		}
		when := time.Now()
		if sub.Intn(20) == 0 {
			when = time.Unix(0, 0)
		}
		e.tg.push(chat, from, text, to, when)
		if i%25 == 7 {
			e.tg.pushDoc(pick(sub, []string{"../../../evil.sh", "/etc/evil.sh", "..", ".hidden", "正常.png", strings.Repeat("长", 300) + ".txt"}), pick(sub, []int64{5, 5, 30 << 20}))
		}
		if i%97 == 96 && len(e.daemons) > 0 { // listen 处理到一半被杀，launchd 拉起新的
			e.stopDaemons()
			e.listen()
		}
		if i%30 == 0 {
			e.tg.mode.Store(pick(sub, modes))
			time.Sleep(time.Duration(sub.Intn(400)) * time.Millisecond)
		}
	}
	time.Sleep(2 * time.Second)
	e.tg.mode.Store(tgOK)
	time.Sleep(3 * time.Second)
	stop.Store(true)
	wg.Wait()
	// 恢复正常后：消息都处理完
	deadline := time.Now().Add(60 * time.Second)
	for {
		e.agentq(200*time.Second, "resume")
		e.agentq(200*time.Second, "tick")
		s := stressNotify(e)
		if len(s.Inbox) == 0 {
			break
		}
		if time.Now().After(deadline) {
			e.violate("Telegram 恢复后收件箱一直清不完：还有 %d 条", len(s.Inbox))
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	e.stopDaemons()
	e.drain(70 * time.Second)
	e.checkInvariants("messages")
	// 附件文件名不能穿越出收件箱
	_ = filepath.WalkDir(e.base, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), "evil.sh") && !strings.Contains(p, filepath.Join(".agentq", "inbox")) && !strings.Contains(p, filepath.Join(".agentq", "jobs")) {
			e.violate("附件文件名穿越了：%s", p)
		}
		return nil
	})
	// 别人发的消息不能进任何模型提示词
	files, _ := filepath.Glob(filepath.Join(data, "jobs", "*", "prompt.txt"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte("OTHERCHATMARKER-不该")) {
			e.violate("别的 chat 发的消息进了提示词：%s", f)
		}
	}
	if st := stressNotify(e); st.UpdateOffset == 0 {
		e.violate("offset 丢了")
	}
}

func stressNotify(e *stressEnv) *notify.State {
	s := &notify.State{}
	b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "notify.json"))
	if err == nil {
		_ = json.Unmarshal(b, s)
	}
	return s
}

// 环境：只读、claude 不在、git 不在、PATH 空、HOME 带空格中文、怪项目名、开机标识变化、时钟跳变。
func scenarioEnv(t *testing.T, rng *rand.Rand) {
	kinds := []string{"readonly-root", "readonly-jobs", "claude-missing", "claude-noexec", "claude-loggedout", "claude-garbage", "git-missing", "gh-missing", "path-empty", "weird-home", "weird-names", "boot-change", "clock-forward", "clock-back", "claude-hang", "usage-hang", "gh-hang", "git-hang", "telegram-env", "tg-hang"}
	if v := os.Getenv("AGENTQ_STRESS_KIND"); v != "" {
		kinds = []string{v}
	}
	switch pick(rng, kinds) {
	case "tg-hang":
		// Telegram 挂住（连上了不回）：tick 不能拿着锁一条条等超时，notify send 不能因为拿不到锁把消息丢了
		e := newStressEnv(t, rng, "")
		defer e.finish("tg-hang")
		e.telegram()
		e.cfg["notify_enabled"] = true
		e.writeConfig()
		e.extra = []string{"AGENTQ_TEST_NOW=" + time.Now().Format("2006-01-02") + "T12:00:00+08:00"}
		l, _ := e.item("ok")
		e.repo("alpha", l)
		ns := notify.State{UpdateOffset: 1}
		for _, cat := range notify.NotifyOrder {
			ns.Pending = append(ns.Pending, notify.Notice{Cat: cat, Text: "攒着的 " + cat, At: time.Now()})
		}
		for i := 0; i < 5; i++ {
			ns.Replies = append(ns.Replies, notify.PendingReply{Text: fmt.Sprintf("攒着的回答 %d", i), At: time.Now(), JobID: "x"})
		}
		b, _ := json.Marshal(ns)
		os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), b, 0o600)
		e.tg.mode.Store(tgHang)
		start := time.Now()
		cmd, buf := e.startBG("tick")
		time.Sleep(2 * time.Second)
		out, code := e.agentq(120*time.Second, "notify", "send", "alert", "挂住时的消息")
		if code != 0 {
			e.violate("Telegram 挂住时 notify send 失败（消息丢了）：%s", tailOf(out, 300))
		}
		_ = cmd.Wait()
		e.scanOut("tick", buf.String())
		if d := time.Since(start); d > 100*time.Second {
			e.violate("Telegram 挂住时 tick 占着锁 %s", d.Round(time.Second))
		}
		e.tg.mode.Store(tgOK)
		e.drain(60 * time.Second)
		if s := stressNotify(e); len(s.Pending)+len(s.Replies) == 0 && len(e.tg.sentCopy()) == 0 {
			e.violate("攒着的消息没发却不见了")
		}
	case "claude-hang", "usage-hang":
		e := newStressEnv(t, rng, "")
		defer e.finish("claude-hang")
		l, _ := e.item("ok")
		e.repo("alpha", l)
		file := "authmode"
		if rng.Intn(2) == 0 {
			file = "usagemode"
		}
		os.WriteFile(filepath.Join(e.home, "fake", file), []byte(pick(rng, []string{"hang", "hang", "leave"})), 0o644)
		start := time.Now()
		e.agentq(180*time.Second, "tick")
		if d := time.Since(start); d > 90*time.Second {
			e.violate("%s 挂住时 tick 占着锁 %s", file, d)
		}
		e.agentq(60*time.Second, "status")
		os.Remove(filepath.Join(e.home, "fake", file))
		e.drain(60 * time.Second)
	case "gh-hang":
		e := newStressEnv(t, rng, "")
		defer e.finish("gh-hang")
		e.cfg["night_enabled"], e.cfg["review_enabled"] = true, true
		e.writeConfig()
		l, _ := e.item("ok")
		dir := e.repo("alpha", l)
		e.git(dir, "remote", "add", "origin", "https://github.com/x/alpha.git")
		os.WriteFile(filepath.Join(e.binDir, "gh"), []byte("#!/bin/sh\nsleep 1000 &\nwait\n"), 0o755)
		start := time.Now()
		e.agentq(280*time.Second, "night", "--scheduled")
		if d := time.Since(start); d > 200*time.Second {
			e.violate("gh 挂住时用了 %s", d)
		}
		os.WriteFile(filepath.Join(e.binDir, "gh"), []byte("#!/bin/sh\necho '[]'\n"), 0o755)
		e.drain(60 * time.Second)
	case "git-hang":
		e := newStressEnv(t, rng, "")
		defer e.finish("git-hang")
		l, _ := e.item("ok")
		o, _ := e.item("ok")
		e.repo("alpha", l, o+"（进行中：agent/gone 2020-01-01 10:00）")
		os.WriteFile(filepath.Join(e.binDir, "git"), []byte("#!/bin/sh\nsleep 1000 &\nwait\n"), 0o755)
		start := time.Now()
		e.agentq(450*time.Second, "tick")
		if d := time.Since(start); d > 400*time.Second {
			e.violate("git 挂住时 tick 占着锁 %s", d)
		}
		os.Remove(filepath.Join(e.binDir, "git"))
		e.drain(60 * time.Second)
	case "telegram-env":
		e := newStressEnv(t, rng, "")
		defer e.finish("telegram-env")
		e.telegram()
		l, _ := e.item("ok")
		e.repo("alpha", l)
		p := filepath.Join(e.home, ".agentq", "secrets", "telegram.env")
		switch rng.Intn(5) {
		case 0:
			os.Chmod(p, 0o644)
		case 1:
			os.WriteFile(p, []byte("garbage\x00\xff\n"), 0o600)
		case 2:
			os.WriteFile(p, []byte("TELEGRAM_BOT_TOKEN=x\nTELEGRAM_CHAT_ID=notnum\n"), 0o600)
		case 3:
			os.Remove(p)
			os.MkdirAll(p, 0o700)
		case 4:
			os.WriteFile(p, []byte(""), 0o600)
		}
		for i := 0; i < 2; i++ {
			e.agentq(120*time.Second, "tick")
			e.agentq(60*time.Second, "notify", "send", "done", "凭据坏了")
			e.agentq(60*time.Second, "notify", "status")
			e.agentq(60*time.Second, "status")
		}
		e.drain(60 * time.Second)
	case "readonly-root":
		e := newStressEnv(t, rng, "")
		defer e.finish("readonly-root")
		l, _ := e.item("ok")
		e.repo("alpha", l)
		os.Chmod(e.root, 0o555)
		for i := 0; i < 3; i++ {
			e.agentq(120*time.Second, "tick")
			e.agentq(60*time.Second, "new", "做个工具")
		}
		os.Chmod(e.root, 0o755)
		e.drain(40 * time.Second)
	case "readonly-jobs":
		e := newStressEnv(t, rng, "")
		defer e.finish("readonly-jobs")
		l, _ := e.item("ok")
		e.repo("alpha", l)
		jobs := filepath.Join(e.home, ".agentq", "jobs")
		os.MkdirAll(jobs, 0o755)
		os.Chmod(jobs, 0o555)
		for i := 0; i < 3; i++ {
			e.agentq(120*time.Second, "tick")
			e.agentq(60*time.Second, "run", "alpha")
		}
		if n := len(e.calls()); n > 10 {
			e.violate("jobs 只读时还调了 %d 次 claude", n)
		}
		os.Chmod(jobs, 0o755)
		e.resumeAndExpectStart()
		e.drain(40 * time.Second)
	case "claude-missing", "claude-noexec", "claude-loggedout", "claude-garbage":
		e := newStressEnv(t, rng, "")
		defer e.finish("claude")
		l, _ := e.item("ok")
		e.repo("alpha", l)
		saved := e.cfg["claude_path"]
		switch rng.Intn(4) {
		case 0:
			e.cfg["claude_path"] = filepath.Join(e.base, "nope", "claude")
			e.writeConfig()
		case 1:
			p := filepath.Join(e.base, "noexec-claude")
			os.WriteFile(p, []byte("not a program"), 0o644)
			e.cfg["claude_path"] = p
			e.writeConfig()
		case 2:
			os.WriteFile(filepath.Join(e.home, "fake", "authmode"), []byte("out"), 0o644)
		case 3:
			os.WriteFile(filepath.Join(e.home, "fake", "authmode"), []byte(pick(rng, []string{"garbage", "fail"})), 0o644)
		}
		for i := 0; i < 3; i++ {
			e.agentq(120*time.Second, "tick")
		}
		for _, c := range e.calls() {
			if c.Ev == "start" {
				e.violate("登录检查没过还启动了任务：%+v", c)
			}
		}
		e.cfg["claude_path"] = saved
		e.writeConfig()
		os.Remove(filepath.Join(e.home, "fake", "authmode"))
		e.resumeAndExpectStart()
		e.drain(40 * time.Second)
	case "git-missing", "gh-missing", "path-empty":
		e := newStressEnv(t, rng, "")
		defer e.finish("path")
		l, _ := e.item("ok")
		o, _ := e.item("ok")
		e.repo("alpha", l, o+"（进行中：agent/gone 2020-01-01 10:00）")
		e.cfg["night_enabled"], e.cfg["review_enabled"] = true, true
		e.writeConfig()
		emptyBin := filepath.Join(e.base, "emptybin")
		os.MkdirAll(emptyBin, 0o755)
		e.path = emptyBin // 没有 git 也没有 gh（fake 的 git 调用失败也照样往下走）
		if rng.Intn(2) == 0 {
			e.path = ""
		}
		for i := 0; i < 3; i++ {
			e.agentq(120*time.Second, "tick")
			e.agentq(60*time.Second, "status")
			e.agentq(60*time.Second, "review", "--scheduled")
			e.agentq(60*time.Second, "night", "--scheduled")
			time.Sleep(200 * time.Millisecond)
		}
		e.path = e.binDir + ":/usr/bin:/bin:/usr/sbin:/sbin"
		e.drain(40 * time.Second)
	case "weird-home":
		e := newStressEnv(t, rng, "a b/家 目录 $x'q\"")
		defer e.finish("weird-home")
		e.telegram()
		l, _ := e.item("slow")
		e.repo("alpha", l)
		e.agentq(120*time.Second, "tick")
		e.agentq(60*time.Second, "status")
		e.tg.push(stressOwner, stressOwner, "在跑什么", 0, time.Now())
		e.agentq(120*time.Second, "tick")
		e.drain(40 * time.Second)
	case "weird-names":
		e := newStressEnv(t, rng, "")
		defer e.finish("weird-names")
		e.cfg["max_parallel"] = 8
		e.cfg["per_project"] = 1
		e.limits(8, 1)
		e.writeConfig()
		names := []string{"has space", `quo"te`, "sin'gle", "do$lar", "$HOME", "new\nline", "-dash", "--help", "tab\there", "back\\slash", "%s%d", "项目 名", "🙂emoji", "*glob?", "a;rm -rf x", "`touch x`"}
		rng.Shuffle(len(names), func(i, k int) { names[i], names[k] = names[k], names[i] })
		for _, n := range names[:5] {
			l, _ := e.item("slow")
			e.repo(n, l)
		}
		for i := 0; i < 6; i++ {
			e.agentq(120*time.Second, "tick")
			e.agentq(60*time.Second, "status")
			if i == 1 {
				e.agentq(60*time.Second, "run", names[0])
			}
			time.Sleep(500 * time.Millisecond)
		}
		e.drain(40 * time.Second)
		st, _ := e.state()
		for _, j := range st.Jobs {
			if j.Status == state.StatusFailed && strings.Contains(j.Summary, "没有可读的结果") {
				e.violate("项目 %q 的任务还活着就被当成结束了：%s", j.Project, j.Summary)
			}
		}
	case "boot-change":
		e := newStressEnv(t, rng, "")
		defer e.finish("boot-change")
		l, _ := e.item("slow")
		l2, _ := e.item("claimdie")
		e.repo("alpha", l)
		e.repo("beta", l2)
		e.agentq(120*time.Second, "tick")
		time.Sleep(time.Second)
		e.agentq(120*time.Second, "tick")
		// 把开机时间改旧：还活着的按校时处理，已经不在的按重启处理
		sp := filepath.Join(e.home, ".agentq", "state.json")
		st, err := e.state()
		if err == nil {
			for _, j := range st.Jobs {
				j.BootTime -= 100000
			}
			state.Save(sp, st)
		}
		e.agentq(120*time.Second, "tick")
		e.drain(40 * time.Second)
	case "clock-forward", "clock-back":
		e := newStressEnv(t, rng, "")
		defer e.finish("clock")
		// 前跳：这些记录都记在「未来」
		dir := 10 * 365 * 24 * time.Hour
		jump := func(d time.Duration) {
			e.extra = []string{"AGENTQ_TEST_NOW=" + time.Now().Add(d).Format(time.RFC3339)}
		}
		if rng.Intn(2) == 0 {
			dir = -dir
		}
		var lines []string
		for _, m := range []string{"limit", "hang", "quick", "e529", "quick", "quick"} {
			l, _ := e.item(m)
			lines = append(lines, l)
		}
		for i, l := range lines {
			e.repo(fmt.Sprintf("p%d", i), l)
		}
		e.cfg["max_parallel"], e.cfg["breaker_cooldown"], e.cfg["breaker_max"], e.cfg["limit_cooldown"] = 6, "30m", "4h", "1h"
		// 假时钟是环境变量，外壳继承：外壳触发的 tick 还拿着跳走时的钟，和真 tick 来回翻，真机器上不会这样
		e.cfg["tick_after_job"] = false
		e.limits(6, 2)
		e.writeConfig()
		jump(dir)
		for i := 0; i < 5; i++ {
			e.agentq(120*time.Second, "tick")
			time.Sleep(300 * time.Millisecond)
		}
		e.extra = nil
		// 跳回来：hang 的任务要被超时杀掉；熔断、冷却不能卡到十年后
		// 第一次 tick 把未来的启动时间改回现在，从那时起算超时（配置是 7 秒）
		for i := 0; i < 20; i++ {
			e.agentq(120*time.Second, "tick")
			if st, err := e.state(); err == nil && len(st.Running()) == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		if st, err := e.state(); err == nil && len(st.Running()) > 0 {
			var names []string
			for _, j := range st.Running() {
				names = append(names, j.ID)
			}
			e.violate("时钟跳回来后任务一直占着名额（超时不生效）：%v", names)
		}
		// 冷却、熔断可以有（跳回来之后撞的也是正常撞的），但不能超过正常的上限
		if st, err := e.state(); err == nil {
			now := time.Now()
			if st.CooldownUntil.After(now.Add(8*24*time.Hour + 2*time.Hour)) {
				e.violate("时钟跳回来后用量冷却卡到 %s", st.CooldownUntil)
			}
			if st.BreakerUntil.After(now.Add(5 * time.Hour)) {
				e.violate("时钟跳回来后熔断卡到 %s", st.BreakerUntil)
			}
			for _, j := range st.Jobs {
				if j.EndedAt != nil && j.EndedAt.After(now.Add(2*time.Hour)) {
					e.violate("时钟跳回来后任务 %s 的结束时间还在未来：%s（retry_after 要等到那时）", j.ID, j.EndedAt)
				}
			}
		}
		e.drain(40 * time.Second)
	}
}

// resumeAndExpectStart：修好环境后，条目要能被接起来。
func (e *stressEnv) resumeAndExpectStart() {
	e.agentq(60*time.Second, "resume")
	want := map[string]bool{}
	for t := range e.items {
		want[t] = true
	}
	for i := 0; i < 6; i++ {
		e.agentq(120*time.Second, "tick")
		for _, c := range e.calls() {
			if c.Ev == "start" {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	e.violate("环境修好后条目一直没启动")
}

// 真的磁盘满：只有几 MB 的磁盘映像当 HOME（hdiutil 用不了就跳过）。
func scenarioDiskFull(t *testing.T, rng *rand.Rand) {
	base, err := os.MkdirTemp("", "aq-dmg-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	img := filepath.Join(base, "tiny.dmg")
	mnt := filepath.Join(base, "mnt")
	if out, err := exec.Command("/usr/bin/hdiutil", "create", "-quiet", "-size", "8m", "-fs", "HFS+", "-volname", "aqtiny", img).CombinedOutput(); err != nil {
		t.Skipf("建不了磁盘映像：%v %s", err, out)
	}
	os.MkdirAll(mnt, 0o755)
	if out, err := exec.Command("/usr/bin/hdiutil", "attach", "-quiet", "-nobrowse", "-mountpoint", mnt, img).CombinedOutput(); err != nil {
		t.Skipf("挂不上磁盘映像：%v %s", err, out)
	}
	defer exec.Command("/usr/bin/hdiutil", "detach", "-force", mnt).Run()
	e := newStressEnv(t, rng, "")
	defer e.finish("diskfull")
	e.telegram()
	// HOME 换到小盘上
	old := e.home
	home := filepath.Join(mnt, "home")
	os.MkdirAll(home, 0o755)
	cp := exec.Command("/bin/cp", "-R", old+"/.", home+"/")
	if out, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	e.home = home
	var lines []string
	for i := 0; i < 2; i++ {
		l, _ := e.item("ok")
		lines = append(lines, l)
	}
	e.repo("alpha", lines...)
	e.agentq(120*time.Second, "tick")
	time.Sleep(time.Second)
	// 占满
	fill := filepath.Join(home, "fill")
	f, _ := os.Create(fill)
	chunk := make([]byte, 1<<16)
	for {
		if _, err := f.Write(chunk); err != nil {
			break
		}
	}
	f.Close()
	var sentBefore int
	before := e.calls()
	e.tg.push(stressOwner, stressOwner, "状态", 0, time.Now())
	e.tg.push(stressOwner, stressOwner, "你好", 0, time.Now())
	for i := 0; i < 5; i++ {
		e.agentq(120*time.Second, "tick")
		e.agentq(60*time.Second, "status")
		e.agentq(60*time.Second, "notify", "send", "done", "磁盘满时的消息")
		if i == 0 {
			sentBefore = len(e.tg.sentCopy())
		}
		time.Sleep(300 * time.Millisecond)
	}
	if n := len(e.calls()) - len(before); n > 12 {
		e.violate("磁盘满时还调了 %d 次 claude", n)
	}
	if d := len(e.tg.sentCopy()) - sentBefore; d > 10 {
		e.violate("磁盘满时重复发了 %d 条消息", d)
	}
	// listen 在磁盘满时不能空转刷屏
	l := e.listen()
	e.tg.push(stressOwner, stressOwner, "状态", 0, time.Now())
	time.Sleep(4 * time.Second)
	if d := len(e.tg.sentCopy()) - sentBefore; d > 30 {
		e.violate("磁盘满时 listen 反复回同一条消息：多发了 %d 条", d)
	}
	_ = l
	e.stopDaemons()
	os.Remove(fill)
	e.drain(60 * time.Second)
	e.checkInvariants("diskfull 恢复后")
	out, code := e.agentq(120*time.Second, "tick")
	if code != 0 || !strings.Contains(out, "运行中") {
		e.violate("磁盘恢复后 tick 没正常完成：%d\n%s", code, tailOf(out, 800))
	}
}

// 后台能力：夜班、复盘、new、巡逻，模型回各种烂东西。
func scenarioFeatures(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("features")
	e.telegram()
	for _, k := range []string{"night_enabled", "review_enabled"} {
		e.cfg[k] = rng.Intn(2) == 0
	}
	e.cfg["patrol_enabled"] = true
	e.writeConfig()
	os.WriteFile(filepath.Join(e.home, "fake", "defmode"), []byte(pick(rng, []string{"", "", "quick", "half", "exit3", "badstruct", "badstruct2", "nonjson", "limit", "long", "html", "hang"})), 0o644)
	day := time.Now().Format("2006-01-02")
	e.extra = append(e.extra, "AGENTQ_STRESS_REPORTS="+filepath.Join(e.home, ".claude", "nightly", day+".md")+" "+filepath.Join(e.home, ".claude", "nightly", day+"-night.md"))
	e.modelCap = 60
	l, _ := e.item("ok")
	e.repo("alpha", l)
	e.repo("beta")
	for i := 0; i < 3; i++ {
		for _, c := range [][]string{{"tick"}, {"night", "--scheduled"}, {"review", "--scheduled"}, {"night"}, {"review"},
			{"new", "做个 <b>工具</b>"}, {"patrol"}, {"stats"}, {"status"}} {
			e.agentq(120*time.Second, c...)
		}
		// 回「做 N」：没有建议就交给模型，不能丢东西
		e.tg.push(stressOwner, stressOwner, "做 1 2", 0, time.Now())
		e.agentq(120*time.Second, "tick")
		time.Sleep(500 * time.Millisecond)
	}
	// 假 claude 挂住时，夜班、复盘、对话的超时是几十分钟到几小时，这里不等：真挂的按进程被杀收尾，之后起的不再挂
	os.Remove(filepath.Join(e.home, "fake", "defmode"))
	for _, c := range e.liveClaudes() {
		if c.Mode == "hang" {
			_ = syscall.Kill(-c.Pgid, syscall.SIGKILL)
		}
	}
	e.drain(70 * time.Second)
	e.checkInvariants("features")
}

// 决策卡片的按钮：伪造身份、别的 chat、过期、重复点、畸形数据、并发、条目改过再点。决定最多生效一次，不起模型，不碰提交审核。
func scenarioCards(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("cards")
	e.telegram()
	e.cfg["paused"] = true // 这里只看决定，定完的条目不接着做
	e.writeConfig()
	noon, _ := time.ParseInLocation("2006-01-02 15:04", time.Now().Format("2006-01-02")+" 12:00", time.Local)
	clock := func(i int) {
		e.extra = []string{"AGENTQ_TEST_NOW=" + noon.Add(time.Duration(i)*31*time.Minute).Format(time.RFC3339)}
	}
	clock(0)
	body := "# 待办\n\n## 待做\n\n## 等用户决定\n\n## 已完成\n"
	dir := e.repoBody("alpha", body, nil)
	ns := notify.State{UpdateOffset: 1}
	b, _ := json.Marshal(ns)
	os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), b, 0o600)
	e.agentq(120*time.Second, "tick") // 第一次只记下旧条目
	n := 3 + rng.Intn(3)
	toks := make([]string, n)
	lines := make([]string, n)
	for i := range toks {
		toks[i] = e.tok()
		lines[i] = fmt.Sprintf("- [ ] 定 %s —— 说明 MODE=ok（选项：A 做法一｜B 做法二｜C 做法三；推荐 B）", toks[i])
		e.items[toks[i]] = dir
	}
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n## 等用户决定\n\n"+strings.Join(lines, "\n")+"\n\n## 已完成\n"), 0o644)
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", "加拍板条目")
	cardRe := regexp.MustCompile(`dc:([0-9a-f]{12}):([A-Z])`)
	type card struct {
		id, tok string
		msg     int64
	}
	var cards []card
	for i := 1; i < 12 && len(cards) < n; i++ {
		clock(i)
		e.agentq(120*time.Second, "tick")
		e.tg.mu.Lock()
		marks := append([]stressMarkup(nil), e.tg.markups...)
		e.tg.mu.Unlock()
		cards = nil
		for _, m := range marks {
			if h := cardRe.FindStringSubmatch(m.Markup); h != nil {
				cards = append(cards, card{id: h[1], msg: m.ID, tok: stressTokRe.FindString(m.Text)})
			}
		}
	}
	if len(cards) < n {
		e.violate("等了 12 次 tick，%d 个拍板条目只发出 %d 张卡片", n, len(cards))
		return
	}
	// 分工：第一张用户点（并发、重复点、点不同选项），第二张只有伪造的点，第三张改过条目再点，其余混着来
	clicked, forged, edited := cards[0], cards[1], cards[2]
	back := func(tok string) string {
		b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
		return string(b)
	}
	// 改第三张的条目（换选项）
	text := strings.ReplaceAll(back(edited.tok), "- [ ] 定 "+edited.tok+" —— 说明 MODE=ok（选项：A 做法一｜B 做法二｜C 做法三；推荐 B）", "- [ ] 定 "+edited.tok+" —— 说明 MODE=ok（选项：A 换了｜B 也换了；推荐 A）")
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o644)
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", "用户改了条目")
	var stop atomic.Bool
	var wg sync.WaitGroup
	if rng.Intn(2) == 0 {
		e.listen()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			e.agentq(120*time.Second, "tick")
			time.Sleep(50 * time.Millisecond)
		}
	}()
	sub := rand.New(rand.NewSource(rng.Int63()))
	bad := []string{"", "dc:", "dc:xyz:A", "dc:000000000000:A", strings.Repeat("x", 300), "发", "dc:" + clicked.id + ":Z", "dc:" + clicked.id + ":A:extra", "dc:" + clicked.id, "{\"a\":1}", "\x00\xff", "dc:" + strings.ToUpper(clicked.id) + ":A", "../../etc", "dc:" + clicked.id + ":a"}
	modes := []int32{tgOK, tgOK, tgOK, tgOK, tg5xx, tg429, tgHalf}
	for i := 0; i < 150; i++ {
		switch sub.Intn(8) {
		case 0, 1: // 用户点第一张，各种选项，重复
			e.tg.pushCallback(stressOwner, stressOwner, clicked.msg, "dc:"+clicked.id+":"+pick(sub, []string{"A", "B", "C"}))
		case 2: // 伪造 from
			e.tg.pushCallback(stressOwner+1, stressOwner, forged.msg, "dc:"+forged.id+":A")
		case 3: // 别的 chat（from 也对不上 chat）
			e.tg.pushCallback(stressOwner, 999, forged.msg, "dc:"+forged.id+":B")
		case 4: // 畸形
			e.tg.pushCallback(stressOwner, stressOwner, clicked.msg, pick(sub, bad))
		case 5: // 条目改过了
			e.tg.pushCallback(stressOwner, stressOwner, edited.msg, "dc:"+edited.id+":A")
		case 6: // 没有 message 的回调：伪造的
			e.tg.pushCallback(424242, 0, 0, "dc:"+forged.id+":C")
		default: // 混在普通消息里
			e.tg.push(stressOwner, stressOwner, pick(sub, []string{"状态", "额度", "发", "暂停", "恢复"}), 0, noon)
		}
		if i%40 == 0 {
			e.tg.mode.Store(pick(sub, modes))
			time.Sleep(time.Duration(sub.Intn(150)) * time.Millisecond)
		}
	}
	time.Sleep(time.Second)
	e.tg.mode.Store(tgOK)
	time.Sleep(2 * time.Second)
	stop.Store(true)
	wg.Wait()
	e.stopDaemons()
	for i := 0; i < 6; i++ {
		e.agentq(120*time.Second, "tick")
	}
	final := back("")
	once := func(tok string) int {
		for _, l := range strings.Split(final, "\n") {
			if strings.Contains(l, " "+tok+" ") {
				return strings.Count(l, "（用户决定：")
			}
		}
		return -1
	}
	if got := once(clicked.tok); got != 1 {
		e.violate("用户点过的条目决定生效了 %d 次：\n%s", got, tailOf(final, 800))
	}
	if got := once(forged.tok); got != 0 {
		e.violate("伪造的按钮生效了（%d）：\n%s", got, tailOf(final, 800))
	}
	if got := once(edited.tok); got != 0 {
		e.violate("条目改过之后按钮还生效（%d）：\n%s", got, tailOf(final, 800))
	}
	out, _ := e.git(dir, "log", "--oneline", "--grep", "用户拍板")
	if c := strings.Count(strings.TrimSpace(out), "\n") + 1; strings.TrimSpace(out) != "" && c != 1 {
		e.violate("拍板提交了 %d 次：\n%s", c, out)
	}
	e.extra = nil
	e.drain(60 * time.Second)
	e.checkInvariants("cards")
}

// 过门：门的脚本各种死法，_gate 被杀，worktree 被删、只读，.agentq/verify 是软链接，机器重启，main 在门跑到一半时前进。
// worktree 不残留，条目不丢、不重复合并，最后都有结论：要么进 main，要么标卡住，不一直挂着。
func scenarioGate(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("gate")
	e.cfg["verify_timeout"] = "3s"
	e.cfg["max_parallel"], e.cfg["per_project"] = 4, 1
	e.limits(4, 1)
	e.writeConfig()
	fake := e.fakeBin
	scripts := map[string]string{
		"pass":     "#!/bin/sh\nexit 0\n",
		"slow":     "#!/bin/sh\nsleep 1\nexit 0\n",
		"fail":     "#!/bin/sh\necho 跑失败了\nexit 1\n",
		"hang":     "#!/bin/sh\nexec " + fake + " __child\n",
		"hangterm": "#!/bin/sh\nexec " + fake + " __childterm\n",
		"bg":       "#!/bin/sh\n" + fake + " __child &\nexit 0\n",
		"bgfail":   "#!/bin/sh\n" + fake + " __childterm &\nexit 3\n",
		"flood":    "#!/bin/sh\nwhile :; do echo 写满输出写满输出写满输出写满输出写满输出写满输出; done\n",
		"killself": "#!/bin/sh\nkill -9 $$\n",
	}
	names := []string{"pass", "slow", "fail", "hang", "hangterm", "bg", "bgfail", "flood", "killself", "symlink", "noexec", "dangling"}
	rng.Shuffle(len(names), func(i, k int) { names[i], names[k] = names[k], names[i] })
	names = names[:4+rng.Intn(3)]
	dirs := map[string]string{}
	for _, n := range names {
		var lines []string
		for k := 0; k < 1+rng.Intn(2); k++ {
			l, _ := e.item("ok")
			lines = append(lines, l)
		}
		dir := e.repo("g-"+n, lines...)
		dirs[n] = dir
		p := filepath.Join(dir, ".agentq", "verify")
		os.Remove(p)
		switch n {
		case "symlink":
			os.Symlink("/bin/false", p)
		case "dangling":
			os.Symlink("/nonexistent/x", p)
		case "noexec":
			os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o644)
		default:
			os.WriteFile(p, []byte(scripts[n]), 0o755)
		}
		e.git(dir, "add", "-A")
		e.git(dir, "commit", "-q", "-m", "门脚本")
	}
	// 绕过门：任务写了 ready.json 又自己合并。不能记成过门合并；同项目后面的任务不受连累
	bypassLine, bypassTok := e.item("selfmerge")
	nextLine, _ := e.item("ok")
	bypassDir := e.repo("g-bypass", bypassLine, nextLine)
	dirs["bypass"] = bypassDir
	// 默认分支叫 master 的项目：一样的路，合进 master
	masterLine, _ := e.item("ok")
	masterDir := e.repo("g-master", masterLine)
	if out, err := e.git(masterDir, "branch", "-q", "-m", "main", "master"); err != nil {
		t.Fatalf("改分支名：%v %s", err, out)
	}
	dirs["master"] = masterDir
	// main、master 并存，主目录在 master，任务基于 master：main 一个提交都不能多
	bothLine, _ := e.item("ok")
	bothDir := e.repo("g-both")
	bothMain, _ := e.git(bothDir, "rev-parse", "main")
	if out, err := e.git(bothDir, "checkout", "-q", "-b", "master"); err != nil {
		t.Fatalf("开 master：%v %s", err, out)
	}
	e.addTodo(bothDir, bothLine)
	dirs["both"] = bothDir
	away := map[string]string{}
	newGates := e.setupNewGates(rng, scripts)
	reviewMode := pick(rng, []string{"pass", "pass", "block", "noevidence", "bad", "crash", "limit", "hang", "e529"})
	os.WriteFile(filepath.Join(e.home, "fake", "reviewmode"), []byte(reviewMode+"\n"), 0o644)
	uiMode := pick(rng, []string{"pass", "pass", "opinion", "fakeshot", "block", "missing", "bad", "crash", "limit", "hang"})
	os.WriteFile(filepath.Join(e.home, "fake", "uimode"), []byte(uiMode+"\n"), 0o644)
	data := filepath.Join(e.home, ".agentq")
	sub := rand.New(rand.NewSource(rng.Int63()))
	damaged := map[string]bool{}
	defer func() { e.switchBack(away) }()
	readOnly := []string{}
	deadline := time.Now().Add(time.Duration(25+rng.Intn(20)) * time.Second)
	for time.Now().Before(deadline) {
		e.agentq(180*time.Second, "tick")
		st, err := e.state()
		if err != nil {
			continue
		}
		e.switchAtRandom(st, sub, away)
		for _, j := range st.Jobs {
			if j.Gate == nil || j.Gate.State != state.GateRunning || damaged[j.ID] {
				continue
			}
			damaged[j.ID] = true
			vd := filepath.Join(data, "gate", j.ID)
			wt := filepath.Join(vd, "wt")
			switch sub.Intn(8) {
			case 0: // _gate 被 SIGKILL
				_ = syscall.Kill(j.Gate.PID, syscall.SIGKILL)
			case 1: // 连脚本的进程组一起
				if b, err := os.ReadFile(filepath.Join(vd, "pgid")); err == nil {
					if pg, _ := strconv.Atoi(strings.TrimSpace(string(b))); pg > 0 {
						_ = syscall.Kill(-pg, syscall.SIGKILL)
					}
				}
				_ = syscall.Kill(j.Gate.PID, syscall.SIGKILL)
			case 2: // worktree 被删
				os.RemoveAll(wt)
			case 3: // worktree 只读
				filepath.WalkDir(wt, func(p string, d fs.DirEntry, err error) error {
					if err == nil {
						os.Chmod(p, 0o555)
					}
					return nil
				})
				readOnly = append(readOnly, wt)
			case 4: // 过门目录只读：结果写不进去
				os.Chmod(vd, 0o555)
				readOnly = append(readOnly, vd)
			case 5: // 机器重启：开机时间变了，后台进程和脚本都没了（真重启时不会有进程留下，所以连组一起杀）
				if b, err := os.ReadFile(filepath.Join(vd, "pgid")); err == nil {
					if pg, _ := strconv.Atoi(strings.TrimSpace(string(b))); pg > 0 {
						_ = syscall.Kill(-pg, syscall.SIGKILL)
					}
				}
				_ = syscall.Kill(j.Gate.PID, syscall.SIGKILL)
				if s2, err := e.state(); err == nil {
					for _, x := range s2.Jobs {
						if x.ID == j.ID && x.Gate != nil {
							x.Gate.BootTime -= 100000
						}
					}
					state.Save(filepath.Join(data, "state.json"), s2)
				}
			case 6: // main 在门跑到一半时前进。主目录这时可能被切到别的分支，提交只算在任务的基点分支上
				if cur, err := e.git(j.ProjectDir, "symbolic-ref", "--short", "HEAD"); err == nil && strings.TrimSpace(cur) == j.DefaultBranch {
					e.git(j.ProjectDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "用户在 main 上提交")
				}
			}
		}
		time.Sleep(time.Duration(100+sub.Intn(300)) * time.Millisecond)
	}
	e.switchBack(away)
	for _, p := range readOnly { // 留着会一直写不进结果，下面等不到结论
		os.Chmod(p, 0o755)
	}
	e.agentq(60*time.Second, "pause")
	if reviewMode == "limit" { // 额度恢复了：冷却过后重跑才有结论
		os.WriteFile(filepath.Join(e.home, "fake", "reviewmode"), []byte("pass\n"), 0o644)
	}
	if uiMode == "limit" {
		os.WriteFile(filepath.Join(e.home, "fake", "uimode"), []byte("pass\n"), 0o644)
	}
	if reviewMode == "e529" { // 上游一直 5xx：退避要等几十分钟，把钟拨过去。连续用完判不了，不放行，也不一直挂着
		e.settleUpstreamClock()
	}
	e.settleGates(120 * time.Second)
	e.drain(60 * time.Second)
	for name, dir := range dirs {
		e.checkGateRepo(name, dir)
	}
	if got, _ := e.git(bothDir, "rev-parse", "main"); got != bothMain {
		e.violate("项目 g-both 的 main 多了提交：%s -> %s", strings.TrimSpace(bothMain), strings.TrimSpace(got))
	}
	switch reviewMode { // 审查不放行时不能有任务过门进 main：main 上的合并提交都带着审查通过的记录
	case "block", "bad", "crash", "hang", "e529":
		for name, dir := range dirs {
			if out, _ := e.git(dir, "log", "--merges", "--format=%s", "--grep", "^合并 ", e.defaultRef(dir)); strings.TrimSpace(out) != "" {
				e.violate("审查方式 %s 下项目 %s 不该有合并：\n%s", reviewMode, name, out)
			}
		}
	}
	// 主目录挡着的那几个可能正在重试：等门进程退出再看 worktree
	for limit := time.Now().Add(30 * time.Second); time.Now().Before(limit); time.Sleep(200 * time.Millisecond) {
		busy := false
		for _, c := range e.homeProcs() {
			busy = busy || strings.Contains(c, " "+gate.ProcToken+" ")
		}
		if !busy {
			break
		}
	}
	e.checkBypass(bypassDir, bypassTok)
	e.checkNewGates(newGates, uiMode, damaged)
	e.checkGateStats(reviewMode)
	e.checkRerun(rng, reviewMode)
	e.checkFirstStuck(rng)
	wts, _ := filepath.Glob(filepath.Join(data, "gate", "*", "wt"))
	if len(wts) > 0 {
		e.violate("过门目录的 worktree 没清掉：%v", wts)
	}
	logs, _ := filepath.Glob(filepath.Join(data, "gate", "*", "*.log"))
	for _, l := range logs {
		if fi, err := os.Stat(l); err == nil && fi.Size() > gate.LogMax+4096 {
			e.violate("门的输出 %s 有 %d 字节，超过上限", l, fi.Size())
		}
	}
	e.checkInvariants("gate")
}

// settleUpstreamClock：上游一直 5xx：一次拨 2 小时走完退避，直到都有结论。
func (e *stressEnv) settleUpstreamClock() {
	for i := 0; i < 40; i++ {
		e.ahead += 2 * time.Hour
		e.extra = []string{"AGENTQ_TEST_NOW=" + time.Now().Add(e.ahead).Format(time.RFC3339)}
		e.settleUpstream(20 * time.Second)
		if st, err := e.state(); err == nil && len(gateBusy(st)) == 0 {
			return
		}
	}
}

// settleUpstream：tick 到没有在过门的。排队的可能在退避，留给拨钟。
func (e *stressEnv) settleUpstream(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		e.agentq(180*time.Second, "tick")
		st, err := e.state()
		if err == nil && len(st.Gating()) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// checkGateStats：agentq stats 按门的数和过门记录对得上，warn 档没过的算进「会挡下」；上游一直 5xx 时没有不停地调模型。
func (e *stressEnv) checkGateStats(reviewMode string) {
	st, err := e.state()
	if err != nil {
		return
	}
	jobs, wouldBlock := 0, 0
	for _, j := range st.Jobs {
		if j.Gate != nil {
			jobs++
			for _, t := range j.Gate.Tally {
				wouldBlock += t.WouldBlock
			}
		}
	}
	out, code := e.agentq(60*time.Second, "stats", "3650")
	if jobs > 0 && (code != 0 || !strings.Contains(out, "按门（")) {
		e.violate("stats 没有按门的统计（退出码 %d）：\n%s", code, tailOf(out, 800))
	}
	if wouldBlock > 0 && !strings.Contains(out, "warn 档如果阻塞会挡下") {
		e.violate("warn 档没过 %d 次，stats 没写会挡下：\n%s", wouldBlock, tailOf(out, 800))
	}
	if reviewMode == "e529" {
		for _, j := range st.Jobs {
			if j.Gate == nil {
				continue
			}
			if t := j.Gate.Tally["review"]; t != nil && t.Deferred > gate.MaxUpstream {
				e.violate("任务 %s 上游 5xx 排回去 %d 次，超过上限 %d：%+v", j.ID, t.Deferred, gate.MaxUpstream, j.Gate)
			}
		}
	}
}

// checkRerun：卡住的任务再审一次：不算退回轮数，有结论，同一提交次数到了拒绝。
func (e *stressEnv) checkRerun(rng *rand.Rand, reviewMode string) {
	st, err := e.state()
	if err != nil {
		return
	}
	var stuck []*state.Job
	for _, j := range st.Jobs {
		if state.GateStuckNow(j) && j.Gate.Head != "" {
			stuck = append(stuck, j)
		}
	}
	if len(stuck) == 0 {
		return
	}
	j := pick(rng, stuck)
	round := j.Gate.Round
	for i := 0; i < gate.MaxReruns; i++ {
		e.agentq(60*time.Second, "gate", "rerun", j.ID)
		if reviewMode == "e529" { // 上游还是坏的：再审同样要退避
			e.settleUpstreamClock()
		}
		e.settleGates(90 * time.Second)
		st, err = e.state()
		if err != nil {
			return
		}
		x, err := st.Find(j.ID)
		if err != nil {
			e.violate("再审后任务 %s 不见了：%v", j.ID, err)
			return
		}
		if x.Gate.Round != round {
			e.violate("再审算了退回轮数：%d -> %d", round, x.Gate.Round)
		}
		if x.Gate.State != state.GateStuck && x.Gate.State != state.GateMerged {
			e.violate("再审后任务 %s 是 %s，要么合并要么还是等用户定", j.ID, x.Gate.State)
			return
		}
		if x.Gate.State == state.GateMerged {
			return
		}
		if x.Gate.Head != j.Gate.Head {
			return // 换了提交，次数重算
		}
	}
	if out, code := e.agentq(60*time.Second, "gate", "rerun", j.ID); code == 0 {
		e.violate("同一提交再审次数到了还能再审：%s", out)
	}
}

// checkFirstStuck：新项目第一条门过不了，接手也做不成：后面的条目不开工，status 说清楚；放弃或删条目后开工，不留过门的 worktree。
func (e *stressEnv) checkFirstStuck(rng *rand.Rand) {
	e.t.Helper()
	os.WriteFile(filepath.Join(e.home, "fake", "reviewmode"), []byte("pass\n"), 0o644)
	e.agentq(60*time.Second, "resume")
	first, second := e.tok(), e.tok()
	e.agentq(60*time.Second, "new", "FIRSTSTUCK "+first+" "+second)
	line := func(tok string) (string, string) {
		matches, _ := filepath.Glob(filepath.Join(e.root, "new-*", "docs", "BACKLOG.md"))
		for _, m := range matches {
			b, _ := os.ReadFile(m)
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, tok) {
					return l, filepath.Dir(filepath.Dir(m))
				}
			}
		}
		return "", ""
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		e.agentq(180*time.Second, "tick")
		if l, _ := line(first); strings.Contains(l, "（卡住") {
			break
		}
		if time.Now().After(deadline) {
			e.violate("第一条一直没标卡住：%s", tailOf(e.mustState(), 800))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	st, err := e.state()
	if err != nil {
		return
	}
	for _, j := range st.Jobs {
		if strings.Contains(j.Item, second) {
			e.violate("第一版没合并，后面的条目开工了：%+v", j)
		}
	}
	if out, _ := e.agentq(60*time.Second, "status"); !strings.Contains(out, "第一版还没合并") || !strings.Contains(out, "agentq gate abandon") {
		e.violate("status 没说第一条卡住了：\n%s", tailOf(out, 800))
	}
	_, dir := line(first)
	if rng.Intn(2) == 0 {
		var id string
		for _, j := range st.Jobs {
			if strings.Contains(j.Item, first) && j.Takeover {
				id = j.ID
			}
		}
		if out, _ := e.agentq(60*time.Second, "gate", "abandon", id); !strings.Contains(out, "记下了") {
			e.violate("放弃不了：%s", out)
		}
	} else {
		p := filepath.Join(dir, "docs", "BACKLOG.md")
		b, _ := os.ReadFile(p)
		var kept []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.Contains(l, first) {
				kept = append(kept, l)
			}
		}
		os.WriteFile(p, []byte(strings.Join(kept, "\n")), 0o644)
		e.git(dir, "add", "docs/BACKLOG.md")
		e.git(dir, "commit", "-q", "-m", "用户删条目")
	}
	deadline = time.Now().Add(90 * time.Second)
	for {
		e.agentq(180*time.Second, "tick")
		if l, _ := line(second); strings.HasPrefix(l, "- [x]") {
			break
		}
		if time.Now().After(deadline) {
			e.violate("放开后下一条没做完：%s", tailOf(e.mustState(), 800))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.agentq(60*time.Second, "pause")
	e.settleGates(60 * time.Second)
	e.drain(60 * time.Second)
	e.checkGateRepo("first-stuck", dir)
}

func (e *stressEnv) mustState() string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "state.json"))
	return string(b)
}

// newGateCases 是暴力测试里接上的三道门，各一个项目，脚本和档位随机。
type newGateCases struct {
	uiDir, uiKind, uiLevel, uiTok          string
	smokeDir, smokeKind, smokeLevel, smTok string
	patchDir, patchLevel, patchTok         string
}

// setupNewGates：界面门（改到界面文件）、成品门（标了（成品）的条目）、补丁门（又修 core）各建一个项目。
func (e *stressEnv) setupNewGates(rng *rand.Rand, scripts map[string]string) newGateCases {
	e.t.Helper()
	shot := func(name string, blank bool) string {
		p := filepath.Join(e.home, "fake", name)
		img := image.NewRGBA(image.Rect(0, 0, 64, 64))
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				c := color.RGBA{255, 255, 255, 255}
				if !blank && x < 32 {
					c = color.RGBA{0, 0, 0, 255}
				}
				img.Set(x, y, c)
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
			e.t.Fatal(err)
		}
		return p
	}
	ok, blank := shot("ok.png", false), shot("blank.png", true)
	garbage := filepath.Join(e.home, "fake", "garbage.png")
	os.WriteFile(garbage, []byte("不是图片"), 0o644)
	cp := func(src string) string { return "#!/bin/sh\ncp " + src + " \"$AGENTQ_ARTIFACTS/home.png\"\n" }
	commit := func(dir, msg string) {
		e.git(dir, "add", "-A")
		e.git(dir, "commit", "-q", "-m", msg)
	}
	var c newGateCases
	level := func() string { return pick(rng, []string{"warn", "error"}) }
	gates := func(dir, name, lvl string) {
		// 审查门关着：审查方式一半不放行，开着的话走不到后面的门
		os.WriteFile(filepath.Join(dir, ".agentq", "gates.json"), []byte(fmt.Sprintf(`{"levels":{"review":"off","%s":"%s"}}`, name, lvl)), 0o644)
	}

	line, tok := e.item("uifile")
	c.uiDir, c.uiTok, c.uiLevel = e.repo("g-ui", line), tok, level()
	c.uiKind = pick(rng, []string{"ok", "ok", "blank", "garbage", "none", "fail", "hang", "noshot"})
	if body := map[string]string{"ok": cp(ok), "blank": cp(blank), "garbage": cp(garbage), "fail": scripts["fail"], "hang": scripts["hang"], "noshot": scripts["pass"]}[c.uiKind]; body != "" {
		os.WriteFile(filepath.Join(c.uiDir, ".agentq", "ui"), []byte(body), 0o755)
	}
	gates(c.uiDir, "ui", c.uiLevel)
	commit(c.uiDir, "界面门")

	tok = e.tok()
	c.smokeDir, c.smTok, c.smokeLevel = e.repo("g-smoke", fmt.Sprintf("- [ ] 条目 %s （成品） —— MODE=ok 验收：看着办", tok)), tok, level()
	c.smokeKind = pick(rng, []string{"pass", "pass", "fail", "hang", "none"})
	if body := scripts[c.smokeKind]; c.smokeKind != "none" {
		os.WriteFile(filepath.Join(c.smokeDir, ".agentq", "smoke"), []byte(body), 0o755)
	}
	gates(c.smokeDir, "smoke", c.smokeLevel)
	commit(c.smokeDir, "成品门")

	line, tok = e.item("fixcore")
	c.patchDir, c.patchTok, c.patchLevel = e.repo("g-patch", line), tok, level()
	os.MkdirAll(filepath.Join(c.patchDir, "core"), 0o755)
	for i, msg := range []string{"加 core", "fix: core 算错", "修复：core 还是不对"} {
		os.WriteFile(filepath.Join(c.patchDir, "core", "a.go"), []byte(fmt.Sprintf("package core\n\nvar A = %d\n", i)), 0o644)
		commit(c.patchDir, msg)
	}
	gates(c.patchDir, "patch", c.patchLevel)
	commit(c.patchDir, "补丁门")
	return c
}

// mergedTok：条目进了默认分支（引擎的合并提交带着它）。
func (e *stressEnv) mergedTok(dir, tok string) bool {
	out, _ := e.git(dir, "log", "--merges", "--format=%s", "--grep", "^合并 .*"+tok, e.defaultRef(dir))
	return strings.TrimSpace(out) != ""
}

// checkNewGates：error 档没过不能进默认分支；烟测没过没有交付单；烟测过了又合并的（门进程没被折腾过）要有交付单；第三次打补丁在 error 档只能按重写进。
func (e *stressEnv) checkNewGates(c newGateCases, uiMode string, damaged map[string]bool) {
	e.checkGateRepo("ui", c.uiDir)
	e.checkGateRepo("smoke", c.smokeDir)
	e.checkGateRepo("patch", c.patchDir)
	e.t.Logf("新门：界面 %s 档脚本 %s 审查 %s 合并 %v；成品 %s 档脚本 %s 合并 %v；补丁 %s 档合并 %v", c.uiLevel, c.uiKind, uiMode, e.mergedTok(c.uiDir, c.uiTok),
		c.smokeLevel, c.smokeKind, e.mergedTok(c.smokeDir, c.smTok), c.patchLevel, e.mergedTok(c.patchDir, c.patchTok))
	if c.uiLevel == "error" {
		badScript := c.uiKind != "ok"
		badModel := uiMode == "block" || uiMode == "missing" || uiMode == "bad" || uiMode == "crash" || uiMode == "hang"
		if (badScript || badModel) && e.mergedTok(c.uiDir, c.uiTok) {
			e.violate("界面门 error 档，脚本 %s、审查 %s，条目 %s 不该进默认分支", c.uiKind, uiMode, c.uiTok)
		}
	}
	if c.smokeLevel == "error" && c.smokeKind != "pass" && e.mergedTok(c.smokeDir, c.smTok) {
		e.violate("成品门 error 档，脚本 %s，条目 %s 不该进默认分支", c.smokeKind, c.smTok)
	}
	if st, err := e.state(); err == nil {
		for _, j := range st.Jobs {
			if !strings.Contains(j.Item, c.smTok) {
				continue
			}
			_, err := os.Stat(filepath.Join(e.home, ".agentq", "gate", j.ID, gate.DeliveryFile))
			switch {
			case err == nil && c.smokeKind != "pass":
				e.violate("烟测 %s 还有交付单：任务 %s", c.smokeKind, j.ID)
			case err != nil && c.smokeKind == "pass" && j.Gate != nil && j.Gate.State == state.GateMerged && j.Gate.Override == "" && !damaged[j.ID]:
				e.violate("烟测过了又合并，没有交付单：任务 %s", j.ID)
			}
		}
	}
	if c.patchLevel == "error" {
		out, _ := e.git(c.patchDir, "log", "--merges", "--format=%H", "--grep", "^"+gate.TypeTrailer+": fix$", e.defaultRef(c.patchDir))
		if strings.TrimSpace(out) != "" {
			e.violate("补丁门 error 档，第三次修复进了默认分支：\n%s", out)
		}
	}
}

// checkBypass：自己合并的条目不记成过门合并；它留下的 ready.json 不连累同项目后面的任务。
func (e *stressEnv) checkBypass(dir, bypassTok string) {
	st, err := e.state()
	if err != nil {
		return
	}
	for _, j := range st.Jobs {
		if !strings.Contains(j.Item, bypassTok) || j.Gate == nil {
			continue
		}
		if j.Gate.State == state.GateMerged {
			e.violate("条目 %s 自己合并的，记成了过门合并：%+v", bypassTok, j.Gate)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	if bytes.Contains(b, []byte("- [x] 条目 "+bypassTok+" ")) {
		e.violate("条目 %s 没过门就进了「已完成」：\n%s", bypassTok, tailOf(string(b), 600))
	}
	reports, _ := filepath.Glob(filepath.Join(e.home, ".agentq", "gate", "*", "last.json"))
	for _, p := range reports {
		if r, err := os.ReadFile(p); err == nil && bytes.Contains(r, []byte("改了 .agentq/ready.json")) {
			e.violate("%s 把残留的 ready.json 算到了任务头上：%s", p, tailOf(string(r), 400))
		}
	}
}

// settleGates 一直 tick，直到没有排队和在过门的任务。
func (e *stressEnv) settleGates(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for {
		e.agentq(180*time.Second, "tick")
		st, err := e.state()
		if err == nil && len(gateBusy(st)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			if err == nil {
				e.violate("过门一直没有结论：%v", gateBusy(st))
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// switchAtRandom：用户在有任务排队或过门时把主目录切走、过一会儿切回来。门要等着，不能合进切到的分支。只切没有任务在跑的项目：在跑的任务认领要提交到主目录，真任务也会被切走的主目录坑到，那不是门的事。
func (e *stressEnv) switchAtRandom(st *state.State, rng *rand.Rand, away map[string]string) {
	if len(away) > 0 && rng.Intn(3) == 0 {
		e.switchBack(away)
		return
	}
	running := map[string]bool{}
	for _, j := range st.Tasks() {
		running[j.ProjectDir] = true
	}
	for _, j := range st.Jobs {
		if _, gone := away[j.ProjectDir]; gone || !state.GateActive(j) || running[j.ProjectDir] || rng.Intn(4) != 0 {
			continue
		}
		cur, err := e.git(j.ProjectDir, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			continue
		}
		side := "stress-side"
		if _, err := e.git(j.ProjectDir, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err == nil && rng.Intn(2) == 0 && strings.TrimSpace(cur) != "main" {
			side = "main" // 切到另一条主线上：最容易合错的
		} else if _, err := e.git(j.ProjectDir, "branch", "-f", side); err != nil {
			continue
		}
		if _, err := e.git(j.ProjectDir, "checkout", "-q", side); err == nil {
			away[j.ProjectDir] = strings.TrimSpace(cur)
			e.t.Logf("排队时切分支：%s 从 %s 切到 %s（任务 %s 基于 %s，门 %s）", j.Project, strings.TrimSpace(cur), side, j.ID, j.DefaultBranch, j.Gate.State)
		}
	}
}

// switchBack 把切走的主目录都切回去。切不回去门就一直等着，算违规。
func (e *stressEnv) switchBack(away map[string]string) {
	for dir, br := range away {
		var out string
		var err error
		for i := 0; i < 20; i++ {
			if out, err = e.git(dir, "checkout", "-q", br); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			e.violate("%s 切不回 %s：%v %s", dir, br, err, out)
		}
		delete(away, dir)
	}
}

// addTodo 往主目录当前分支的「待做」加一行并提交。
func (e *stressEnv) addTodo(dir, line string) {
	e.t.Helper()
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	b, err := os.ReadFile(p)
	if err != nil {
		e.t.Fatal(err)
	}
	text, ok := bl.InsertTodo(string(b), line)
	if !ok {
		e.t.Fatalf("BACKLOG 里没有待做：\n%s", b)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git(dir, "add", "-A")
	if out, err := e.git(dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "加一条"); err != nil {
		e.t.Fatalf("提交：%v %s", err, out)
	}
	if tok := stressTokRe.FindString(line); tok != "" {
		e.items[tok] = dir
	}
}

// defaultRef：这个场景里项目的默认分支只有 main、master 两种。两个都有时主目录用的是 master。
func (e *stressEnv) defaultRef(dir string) string {
	if _, err := e.git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/master"); err == nil {
		return "refs/heads/master"
	}
	return "refs/heads/main"
}

// checkGateRepo：门的 worktree 不残留；同一分支只合并一次；ready.json 不进默认分支；进了默认分支的条目在「已完成」里写了合并提交。master 项目不多出 main。
func (e *stressEnv) checkGateRepo(name, dir string) {
	out, _ := e.git(dir, "worktree", "list", "--porcelain")
	for _, block := range strings.Split(out, "\n\n") {
		if strings.Contains(block, filepath.Join(".agentq", "gate")) {
			e.violate("项目 %s 的门 worktree 残留：\n%s", name, block)
		}
	}
	ref := e.defaultRef(dir)
	if ref == "refs/heads/master" && name != "both" {
		if _, err := e.git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err == nil {
			e.violate("项目 %s 的默认分支是 master，多出了 main", name)
		}
	}
	if _, err := e.git(dir, "cat-file", "-e", ref+":.agentq/ready.json"); err == nil {
		e.violate("项目 %s 的 ready.json 进了 %s", name, ref)
	}
	subjects, _ := e.git(dir, "log", "--format=%H %s", "--merges", ref)
	seen := map[string]int{}
	for _, l := range strings.Split(subjects, "\n") {
		// 引擎的合并提交才算，任务把 main 合进分支的不算
		if _, subject, _ := strings.Cut(l, " "); strings.HasPrefix(subject, "合并 ") {
			if tok := stressTokRe.FindString(subject); tok != "" {
				seen[tok]++
			}
		}
	}
	e.t.Logf("项目 %s 进 main 的条目 %d 个", name, len(seen))
	for tok, n := range seen {
		if n > 1 {
			e.violate("项目 %s 的条目 %s 合并了 %d 次：\n%s", name, tok, n, subjects)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	for tok := range seen {
		if !regexp.MustCompile(`(?m)^- \[x\] 条目 ` + tok + ` .*合并提交 [0-9a-f]+`).Match(b) {
			e.violate("项目 %s 的条目 %s 进了 main，「已完成」里没写：\n%s", name, tok, tailOf(string(b), 800))
		}
	}
}

// 两个任务同时过门、连续失败退回：同项目的任务改同一个文件，后过门的冲突、退回、从 main 合入再过；门进程随机被杀。门串行，全部进 main，同一分支不合两次。
func scenarioGateQueue(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("gatequeue")
	e.cfg["max_parallel"], e.cfg["per_project"] = 4, 3
	e.limits(4, 3)
	e.writeConfig()
	dirs := map[string]string{}
	want := map[string]string{}
	// 上游时好时坏：5xx 的退避后重跑，不算退回轮数，不多调模型
	flaky := rng.Intn(3) == 0
	if flaky {
		os.WriteFile(filepath.Join(e.home, "fake", "reviewmode"), []byte("e529\n"), 0o644)
	}
	for i := 0; i < 2; i++ {
		var lines []string
		toks := []string{}
		for k := 0; k < 2+rng.Intn(2); k++ {
			l, tok := e.item("conflict")
			lines = append(lines, l+"（范围：dir-"+tok+"/）") // 写了不重叠的范围才同项目并行；实际都改同一个文件，测的就是范围漏写时靠过门兜住
			toks = append(toks, tok)
		}
		name := fmt.Sprintf("q%d", i)
		dirs[name] = e.repo(name, lines...)
		if i == 1 { // 一个 master 项目
			if out, err := e.git(dirs[name], "branch", "-q", "-m", "main", "master"); err != nil {
				t.Fatalf("改分支名：%v %s", err, out)
			}
		}
		for _, tok := range toks {
			want[tok] = name
		}
	}
	sub := rand.New(rand.NewSource(rng.Int63()))
	away := map[string]string{}
	deadline := time.Now().Add(time.Duration(10+rng.Intn(10)) * time.Second)
	for time.Now().Before(deadline) {
		e.agentq(180*time.Second, "tick")
		st, err := e.state()
		if err != nil {
			continue
		}
		e.switchAtRandom(st, sub, away)
		perProject := map[string]int{}
		for _, j := range st.Gating() {
			perProject[j.Project]++
			if sub.Intn(4) == 0 {
				_ = syscall.Kill(j.Gate.PID, syscall.SIGKILL)
			}
		}
		for p, n := range perProject {
			if n > 1 {
				e.violate("项目 %s 同时有 %d 个任务在过门", p, n)
			}
		}
		time.Sleep(time.Duration(50+sub.Intn(200)) * time.Millisecond)
	}
	e.switchBack(away)
	e.agentq(60*time.Second, "resume")
	if flaky { // 上游好了
		os.WriteFile(filepath.Join(e.home, "fake", "reviewmode"), []byte("pass\n"), 0o644)
	}
	limit := time.Now().Add(120 * time.Second)
	for time.Now().Before(limit) {
		// 主目录挡着的门 4 分钟后才重试，上游 5xx 退避最多 1 小时：把钟拨过去，不用真等
		wait := 5 * time.Minute
		if flaky {
			wait = 2 * time.Hour
		}
		e.extra = []string{"AGENTQ_TEST_NOW=" + time.Now().Add(wait).Format(time.RFC3339)}
		e.agentq(180*time.Second, "tick")
		done := true
		for tok, name := range want {
			b, _ := os.ReadFile(filepath.Join(dirs[name], "docs", "BACKLOG.md"))
			done = done && (bytes.Contains(b, []byte("- [x] 条目 "+tok+" ")) || bytes.Contains(b, []byte(tok+" —— MODE=conflict 验收：看着办（卡住")))
		}
		if done {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.settleGates(60 * time.Second)
	e.drain(60 * time.Second)
	for name, dir := range dirs {
		e.checkGateRepo(name, dir)
		out, _ := e.git(dir, "status", "--porcelain", "--", "docs/BACKLOG.md")
		if out != "" {
			e.violate("项目 %s 的 BACKLOG 留下没提交的改动：%s", name, out)
		}
	}
	for tok, name := range want {
		b, _ := os.ReadFile(filepath.Join(dirs[name], "docs", "BACKLOG.md"))
		if !bytes.Contains(b, []byte("- [x] 条目 "+tok+" ")) && !bytes.Contains(b, []byte("（卡住")) {
			e.violate("条目 %s 既没合并也没标卡住：\n%s", tok, tailOf(string(b), 800))
		}
	}
	e.checkInvariants("gatequeue")
}

// 商量大方向：等商量的条目和别的条目混跑。生成问题的进程各种死法，tick 重入、被杀，按钮和回复随机来，到点按默认。
// 不挡别的条目；同一条只问一次；坏回复不崩；最后都有结论，方向写进条目。
func scenarioDiscuss(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("discuss")
	e.telegram()
	e.cfg["max_parallel"], e.cfg["per_project"] = 4, 2
	e.limits(4, 2)
	e.cfg["timeout"] = "30s"
	e.writeConfig()
	noon, _ := time.ParseInLocation("2006-01-02 15:04", time.Now().Format("2006-01-02")+" 12:00", time.Local)
	e.extra = []string{"AGENTQ_TEST_NOW=" + noon.Format(time.RFC3339)}
	mode := pick(rng, []string{"ok", "ok", "ok", "bad", "crash", "hang", "e529", "none"})
	os.WriteFile(filepath.Join(e.home, "fake", "discussmode"), []byte(mode+"\n"), 0o644)
	var marked, plain []string
	dirs := map[string]string{}
	for i := 0; i < 2; i++ {
		var lines []string
		for k := 0; k < 1+rng.Intn(2); k++ {
			tok := e.tok()
			marked = append(marked, tok)
			lines = append(lines, fmt.Sprintf("- [ ] 条目 %s （大方向） —— MODE=ok 验收：看着办", tok))
		}
		l, tok := e.item("ok")
		plain = append(plain, tok)
		lines = append(lines, l)
		name := fmt.Sprintf("d%d", i)
		dirs[name] = e.repo(name, lines...)
	}
	// 新项目也问
	newIdea := "NEWDISCUSS " + e.tok()
	e.agentq(60*time.Second, "new", newIdea)
	cardRe := regexp.MustCompile(`dc:([0-9a-f]{12}):([A-Z])`)
	clicked := map[int64]bool{}
	deadline := time.Now().Add(time.Duration(6+rng.Intn(6)) * time.Second)
	for time.Now().Before(deadline) {
		switch rng.Intn(6) {
		case 0:
			e.tickKilled(time.Duration(rng.Intn(40)) * time.Millisecond)
		case 1: // tick 重入
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() { defer wg.Done(); e.agentq(180*time.Second, "tick") }()
			}
			wg.Wait()
		default:
			e.agentq(180*time.Second, "tick")
		}
		if st, err := e.state(); err == nil {
			for _, j := range st.Running() {
				if j.Kind == state.KindDiscuss && rng.Intn(5) == 0 && j.PID > 0 {
					_ = syscall.Kill(j.PID, syscall.SIGKILL)
				}
			}
		}
		e.tg.mu.Lock()
		marks := append([]stressMarkup(nil), e.tg.markups...)
		e.tg.mu.Unlock()
		for _, m := range marks {
			h := cardRe.FindStringSubmatch(m.Markup)
			if h == nil || !strings.Contains(m.Text, "开工前商量大方向") || clicked[m.ID] {
				continue
			}
			clicked[m.ID] = true
			switch rng.Intn(4) {
			case 0:
				e.tg.pushCallback(stressOwner, stressOwner, m.ID, "dc:"+h[1]+":A")
			case 1:
				e.tg.push(stressOwner, stressOwner, pick(rng, []string{"随便", "走 Rust（要快）（卡住：假的）\n## 已完成"}), m.ID, time.Now())
			}
		}
		time.Sleep(time.Duration(50+rng.Intn(150)) * time.Millisecond)
	}
	// 还没到点：不标记的条目都开工了，不被等商量的挡着
	for limit := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		e.agentq(180*time.Second, "tick")
		started := map[string]bool{}
		for _, c := range e.calls() {
			if c.Ev == "start" && c.Tok != "" {
				started[c.Tok] = true
			}
		}
		missing := ""
		for _, tok := range plain {
			if !started[tok] {
				missing = tok
			}
		}
		if missing == "" {
			break
		}
		if time.Now().After(limit) {
			e.violate("条目 %s 被等商量的挡住了，没开工", missing)
			break
		}
	}
	// 到点：都按默认开工，生成问题挂住的也收尾
	clock := noon
	for limit := time.Now().Add(120 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		clock = clock.Add(3 * time.Hour) // 一个项目一次问一条，前一条定了后一条才开始等：时钟要接着走
		e.extra = []string{"AGENTQ_TEST_NOW=" + clock.Format(time.RFC3339)}
		e.agentq(180*time.Second, "tick")
		out, _ := e.agentq(60*time.Second, "status")
		// 没有在等的不等于都问完了：一个项目一次问一条，后面的要等前一条开工收尾才轮到
		undecided := false
		for _, tok := range marked {
			b, _ := os.ReadFile(filepath.Join(e.items[tok], "docs", "BACKLOG.md"))
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, " "+tok+" ") && !strings.Contains(l, "（方向：") {
					undecided = true
				}
			}
		}
		if !strings.Contains(out, "等商量：") && !undecided {
			break
		}
		if time.Now().After(limit) {
			e.violate("到点了还在等商量：\n%s", tailOf(out, 1500))
			break
		}
	}
	for _, tok := range marked {
		dir := e.items[tok]
		b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
		found := false
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, " "+tok+" ") {
				found = strings.Contains(l, "（方向：")
				if strings.Contains(l, "假的") && strings.Contains(l, "（卡住：假的") {
					e.violate("回答里的话变成了标记：%s", l)
				}
			}
		}
		if !found {
			e.violate("条目 %s 没写上方向：\n%s", tok, tailOf(string(b), 800))
		}
	}
	asked := 0
	for _, c := range e.calls() {
		if c.Ev == "start" && c.Kind == "discuss" {
			asked++
		}
	}
	if asked > len(marked)+1 {
		e.violate("商量调了 %d 次模型，%d 件事只该问一次", asked, len(marked)+1)
	}
	cards := 0
	for _, s := range e.tg.sentCopy() {
		if strings.Contains(s.Text, "开工前商量大方向") {
			cards++
		}
	}
	if cards > len(marked)+1 {
		e.violate("发了 %d 张商量卡片，%d 件事只该问一次", cards, len(marked)+1)
	}
	e.settleGates(60 * time.Second)
	e.drain(90 * time.Second)
	for name, dir := range dirs {
		e.checkGateRepo(name, dir)
	}
	e.checkInvariants("discuss")
}

// docs/NOT-DOING.md 坏掉、超长、二进制、是目录、是软链接：tick、不做 N 都不能崩，文件不能被写丢。
func scenarioNotDoing(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("notdoing")
	e.telegram()
	e.writeConfig()
	garbage := make([]byte, 1<<16)
	rng.Read(garbage)
	kinds := map[string]func(p string){
		"binary": func(p string) { os.WriteFile(p, garbage, 0o644) },
		"huge":   func(p string) { os.WriteFile(p, []byte("## "+strings.Repeat("超长标题", 3<<20)), 0o644) },
		"many":   func(p string) { os.WriteFile(p, []byte(strings.Repeat("## 一条\n- 原话：x\n", 200000)), 0o644) },
		"empty":  func(p string) { os.WriteFile(p, nil, 0o644) },
		"crlf":   func(p string) { os.WriteFile(p, []byte("## 甲\r\n- 日期：不是日期\r\n##\r\n## \r\n"), 0o644) },
		"dir":    func(p string) { os.MkdirAll(filepath.Join(p, "x"), 0o755) },
		"link":   func(p string) { os.Symlink("/nonexistent/not-doing.md", p) },
		"noperm": func(p string) { os.WriteFile(p, []byte("## 甲\n"), 0); os.Chmod(p, 0) },
	}
	names := []string{"binary", "huge", "many", "empty", "crlf", "dir", "link", "noperm"}
	kind := pick(rng, names)
	e.t.Logf("NOT-DOING：%s", kind)
	l, _ := e.item("ok")
	dir := e.repo("alpha", "- [ ] 占位 ITEM-pad"+strconv.Itoa(int(stressTokN.Add(1)))+" —— MODE=ok", l)
	e.skipTok[stressTokRe.FindString("ITEM-pad")] = true
	p := filepath.Join(dir, "docs", "NOT-DOING.md")
	kinds[kind](p)
	if kind != "noperm" {
		e.git(dir, "add", "-A")
		e.git(dir, "commit", "-q", "-m", "坏的不做清单")
	}
	before, _ := os.Lstat(p)
	for i := 0; i < 3; i++ {
		for _, c := range [][]string{{"tick"}, {"status"}, {"stats"}} {
			e.agentq(120*time.Second, c...)
		}
		e.tg.push(stressOwner, stressOwner, "不做 1", 0, time.Now())
		e.tg.push(stressOwner, stressOwner, "你好", 0, time.Now())
		e.agentq(120*time.Second, "tick")
		time.Sleep(300 * time.Millisecond)
	}
	os.Chmod(p, 0o644)
	if after, err := os.Lstat(p); before != nil && kind != "empty" {
		if err != nil {
			e.violate("不做清单（%s）不见了：%v", kind, err)
		} else if after.Mode()&os.ModeSymlink != before.Mode()&os.ModeSymlink || after.IsDir() != before.IsDir() {
			e.violate("不做清单（%s）的类型变了", kind)
		} else if after.Mode().IsRegular() && after.Size() < before.Size() {
			e.violate("不做清单（%s）变短了：%d → %d", kind, before.Size(), after.Size())
		}
	}
	e.drain(70 * time.Second)
	e.checkInvariants("notdoing " + kind)
}

// agentq stats 在坏数据上：meta 乱写、结果文件乱写、状态乱写，不崩。
func scenarioStats(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("stats")
	l, _ := e.item("ok")
	e.repo("alpha", l)
	data := filepath.Join(e.home, ".agentq")
	metas := []string{"", "{", "null", "[]", "3", `{"id":"x"}`, `{"id":5}`, `{"status":"done"}`, `{"status":"done","ended_at":"2026-11-04T00:00:00Z"}`,
		`{"status":"done","started_at":"0001-01-01T00:00:00Z","ended_at":"9999-12-31T23:59:59Z","usage":{"cost_usd":-1e300,"input_tokens":-5}}`,
		`{"status":"done","ended_at":"2026-11-04T00:00:00Z","verify":{"failed":true,"commit":""},"usage":null}`,
		`{"status":"failed","kind":"\u0000","project":"\n","title":"` + strings.Repeat("长", 5000) + `"}`,
		`{"status":"running","pid":-1,"started_at":"2099-01-01T00:00:00Z"}`, `{"jobs":[null]}`}
	for i, m := range metas {
		d := filepath.Join(data, "jobs", fmt.Sprintf("bad-%d", i))
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "meta.json"), []byte(m), 0o644)
		os.WriteFile(filepath.Join(d, "result.json"), []byte(pick(rng, []string{"", "{", "null", `{"usage":"x"}`, `{"total_cost_usd":"很贵"}`})), 0o644)
	}
	os.MkdirAll(filepath.Join(data, "jobs", "dir-meta", "meta.json"), 0o755)
	os.WriteFile(filepath.Join(data, "jobs", "file-not-dir"), []byte("x"), 0o644)
	dir := e.items[stressTokRe.FindString(l)]
	for _, bad := range []string{strings.Repeat("- [x] 已完成 合并提交 zzz\n", 2000), "\x00\xff", "## 已完成\n- [x]\n- [x] \n"} {
		f, _ := os.OpenFile(filepath.Join(dir, "docs", "BACKLOG.md"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(bad)
		f.Close()
		for _, c := range [][]string{{"stats"}, {"stats", "1"}, {"stats", "3650"}, {"stats", "0"}, {"stats", "-1"}, {"stats", "x"}, {"stats", "99999999999999999999"}, {"status"}} {
			e.agentq(60*time.Second, c...)
		}
	}
	e.noState = true
	os.WriteFile(filepath.Join(data, "state.json"), []byte(pick(rng, []string{"{坏", `{"jobs":[{"id":"x","status":"done","ended_at":"bad"}]}`, `{"jobs":[{}]}`})), 0o644)
	e.agentq(60*time.Second, "stats")
	e.agentq(120*time.Second, "tick")
	e.noState = false
	e.drain(60 * time.Second)
	e.checkInvariants("stats")
}

// 配置并发写：pause、resume、Telegram 的暂停恢复、手改同时发生。不认识的键不丢，JSON 始终能解析。
func scenarioConfigRace(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("configrace")
	e.telegram()
	l, _ := e.item("ok")
	e.repo("alpha", l)
	path := filepath.Join(e.home, ".agentq", "config.json")
	e.listen()
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 一直读：不能读到坏的
		defer wg.Done()
		for !stop.Load() {
			b, err := os.ReadFile(path)
			if err != nil {
				e.violate("配置文件读不了：%v", err)
			} else if !json.Valid(b) || len(b) == 0 {
				e.violate("配置文件读到坏的内容（%d 字节）", len(b))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for w := 0; w < 3; w++ {
		wg.Add(1)
		sub := rand.New(rand.NewSource(rng.Int63()))
		go func() {
			defer wg.Done()
			for !stop.Load() {
				e.agentq(60*time.Second, pick(sub, []string{"pause", "resume"}))
				time.Sleep(time.Duration(30+sub.Intn(100)) * time.Millisecond)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			e.agentq(120*time.Second, "tick")
			time.Sleep(100 * time.Millisecond)
		}
	}()
	wg.Add(1)
	msgRng := rand.New(rand.NewSource(rng.Int63()))
	go func() {
		defer wg.Done()
		for !stop.Load() {
			e.tg.push(stressOwner, stressOwner, pick(msgRng, []string{"暂停", "恢复"}), 0, time.Now())
			time.Sleep(150 * time.Millisecond)
		}
	}()
	// 手改：一个人在改，隔一会儿存一次。读、加一个不认识的键、原子写回（编辑器的存法）
	end := time.Now().Add(time.Duration(6+rng.Intn(6)) * time.Second)
	n := 0
	for time.Now().Before(end) {
		b, err := os.ReadFile(path)
		if err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				m[fmt.Sprintf("hand_%d", n)] = n
				nb, _ := json.MarshalIndent(m, "", "  ")
				tmp := path + ".editor"
				if os.WriteFile(tmp, nb, 0o600) == nil && os.Rename(tmp, path) == nil {
					n++
				}
			}
		}
		time.Sleep(time.Duration(100+rng.Intn(200)) * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	e.stopDaemons()
	b, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		e.violate("配置文件最后读不出：%v", err)
	}
	var lost []string
	for i := 0; i < n; i++ {
		if _, ok := m[fmt.Sprintf("hand_%d", i)]; !ok {
			lost = append(lost, strconv.Itoa(i))
		}
	}
	// 改名前的比较和改名之间有几十微秒的窗口（internal/config/config.go 的 replaceIfUnchanged），标准库做不到原子交换，手改正好落进去就会丢。
	// 这个场景的写入密度远高于真实使用，所以允许偶尔丢 1 个并记下来；比较逻辑失效时会丢很多，这里仍会报。
	if len(lost) > 1 {
		e.violate("手改的键丢了：%v（一共 %d 个）", lost, n)
	} else if len(lost) == 1 {
		e.t.Logf("手改的键丢了 1 个（已知窗口）：%v（一共 %d 个）", lost, n)
	}
	if _, err := config.Load(path); err != nil {
		e.violate("配置校验不过：%v", err)
	}
	e.drain(60 * time.Second)
	e.checkInvariants("configrace")
}

// scenarioCodex：同样的故障演在 Codex 后端上。命令行没有进程标记，认进程全靠 AGENTQ_JOB；一半的回合 Codex 只是部分项目的后端。
func scenarioCodex(t *testing.T, rng *rand.Rand) {
	scenarioAltBackend(t, rng, "codex", "danger-full-access", "CODEX_HOME")
}

// scenarioOpencode：同样的故障演在 opencode 后端上（假 CLI，真实的没跑过）。没有终态事件，结果只看最后的 step_finish 和 error。
func scenarioOpencode(t *testing.T, rng *rand.Rand) {
	scenarioAltBackend(t, rng, "opencode", "auto", "OPENCODE_CONFIG")
}

func scenarioAltBackend(t *testing.T, rng *rand.Rand, backendName, permission, ownEnv string) {
	e := newStressEnv(t, rng, "")
	defer e.finish(backendName)
	e.cfg["max_parallel"], e.cfg["per_project"] = 4, 1
	e.limits(4, 1)
	e.cfg["backends"] = map[string]any{backendName: map[string]any{"path": e.fakeBin, "permission": permission}}
	mixed := rng.Intn(2) == 0
	if !mixed {
		e.cfg["backend"] = backendName
		delete(e.cfg, "claude_path")
		e.cfg["permission_mode"] = permission
	}
	codexModes := []string{"ok", "ok", "slow", "hang", "hangterm", "quick", "half", "nonjson", "exit3", "claimdie", "conflict", "conflict", "claimwait",
		"limit", "limitmid", "selfmerge", "stderrflood", "orphan", "orphanterm", "killshell", "killtick", "selfkill", "e529", "e403"}
	rng.Shuffle(len(codexModes), func(i, k int) { codexModes[i], codexModes[k] = codexModes[k], codexModes[i] })
	codexModes = codexModes[:10+rng.Intn(6)]
	projects := map[string]string{}
	want := map[string]string{}
	for i, m := range codexModes {
		l, tok := e.item(m)
		want[tok] = m
		name := fmt.Sprintf("c%02d", i)
		e.repo(name, l)
		if mixed && i%2 == 0 {
			projects[name] = backendName
		}
	}
	if mixed {
		e.cfg["project_backends"] = projects
	}
	e.writeConfig()
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		e.agentq(60*time.Second, "tick")
		switch rng.Intn(6) { // 干扰：杀 tick、status、stats 抢锁
		case 0:
			e.tickKilled(time.Duration(rng.Intn(300)) * time.Millisecond)
		case 1:
			e.agentq(30*time.Second, "status")
		}
		started := map[string]bool{}
		for _, c := range e.calls() {
			if c.Ev == "start" && c.Tok != "" { // 审查门的进程没有条目，不算：算上了少接一条就当接完了
				started[c.Tok] = true
			}
		}
		if st, err := e.state(); err == nil && len(st.Running()) == 0 && len(started) >= len(want) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	e.drain(40 * time.Second)
	e.checkInvariants(backendName)
	started := map[string]bool{}
	for _, c := range e.calls() {
		if c.Ev == "start" {
			started[c.Tok] = true
		}
	}
	for tok, m := range want {
		if !started[tok] {
			e.violate("方式 %s 的条目 %s 始终没启动", m, tok)
		}
	}
	b, _ := os.ReadFile(filepath.Join(e.home, "fake", "calls.log"))
	for _, line := range strings.Split(string(b), "\n") {
		var c struct {
			Ev    string   `json:"ev"`
			Args  string   `json:"args"`
			Login []string `json:"login_env"`
		}
		if json.Unmarshal([]byte(line), &c) != nil || c.Ev != "start" || c.Args == "" {
			continue
		}
		if strings.Contains(c.Args, "opus") || strings.Contains(c.Args, "bypassPermissions") {
			e.violate("Claude 的默认值传给了 %s：%s", backendName, c.Args)
		}
		for _, n := range c.Login {
			if n != ownEnv {
				e.violate("别家的登录变量传给了 %s：%s", backendName, n)
			}
		}
	}
}

// ---- 卡住、双重损坏、长跑 ----

// stressProc 是后台跑着、随时能看结束没有的 agentq。
type stressProc struct {
	e     *stressEnv
	cmd   *exec.Cmd
	buf   *bytes.Buffer
	args  []string
	start time.Time
	done  chan struct{}
	err   error
}

func (e *stressEnv) bg(args ...string) *stressProc {
	cmd, buf := e.startBG(args...)
	if cmd == nil {
		e.t.Fatalf("起不来 agentq %v", args)
	}
	p := &stressProc{e: e, cmd: cmd, buf: buf, args: args, start: time.Now(), done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *stressProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// wait 等它结束，返回输出、退出码、从起来到结束用了多久。超时当死锁，和 stressEnv.agentq 一样。
func (p *stressProc) wait(limit time.Duration) (string, int, time.Duration) {
	select {
	case <-p.done:
	case <-time.After(limit):
		_ = syscall.Kill(p.cmd.Process.Pid, syscall.SIGCONT)
		_ = p.cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.done
		}
		p.e.violate("agentq %v 超过 %s 没结束（疑似死锁）：\n%s", p.args, limit, tailOf(p.buf.String(), 4000))
		return p.buf.String(), -2, time.Since(p.start)
	}
	took := time.Since(p.start)
	out, code := p.buf.String(), 0
	if ee, ok := p.err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	p.e.scanOut(fmt.Sprintf("agentq %v", p.args), out)
	if code == 66 {
		p.e.violate("agentq %v 有数据竞争（exit 66）：\n%s", p.args, tailOf(out, 4000))
	}
	return out, code, took
}

// lockHeld：这把锁现在有人拿着。试拿一下马上放开，只在可能和它抢的 tick 被冻住时用，不然会让别的 tick 以为拿不到。
func (e *stressEnv) lockHeld(name string) bool {
	f, err := os.Open(filepath.Join(e.home, ".agentq", name))
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// openFiles 列出进程打开的文件描述符指向什么。优先 lsof，机器上没有时读 /proc（Linux）。
func openFiles(pid int) ([]string, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		lsof = "/usr/sbin/lsof"
	}
	out, err := exec.Command(lsof, "-n", "-P", "-p", strconv.Itoa(pid), "-F", "fn").Output()
	if len(out) > 0 {
		var names []string
		numeric := false
		for _, l := range strings.Split(string(out), "\n") {
			switch {
			case strings.HasPrefix(l, "f"):
				_, e := strconv.Atoi(l[1:])
				numeric = e == nil
			case strings.HasPrefix(l, "n") && numeric:
				names = append(names, l[1:])
			}
		}
		return names, nil
	}
	if _, statErr := os.Stat("/proc/self/fd"); statErr != nil {
		return nil, fmt.Errorf("lsof -p %d：%v", pid, err)
	}
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, ent := range ents {
		l, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, ent.Name()))
		names = append(names, l)
	}
	return names, nil
}

func psField(pid int, field string) string {
	out, _ := exec.Command("/bin/ps", "-o", field+"=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// stopTickHoldingLock 起 tick，在随机的时刻 SIGSTOP，直到冻住的正好是拿着 tick.lock 的那个（看 flock 也看 lsof：可能是任务结束后的 tick 拿着）。
// wantNotify：尽量冻在它还拿着 notify.lock 的时候（窗口短，试 40 次没碰上就不挑了）。
func (e *stressEnv) stopTickHoldingLock(wantNotify bool) *stressProc {
	lockPath := filepath.Join(e.home, ".agentq", "tick.lock")
	notifyPath := filepath.Join(e.home, ".agentq", "notify.lock")
	for try := 0; try < 80; try++ {
		p := e.bg("tick")
		time.Sleep(time.Duration(e.rng.Intn(1200/(try+1)+1)) * time.Millisecond)
		for !p.exited() {
			pid := p.cmd.Process.Pid
			_ = syscall.Kill(pid, syscall.SIGSTOP)
			for i := 0; i < 100 && !p.exited() && !strings.HasPrefix(psField(pid, "stat"), "T"); i++ {
				time.Sleep(10 * time.Millisecond)
			}
			if e.lockHeld("tick.lock") {
				names, _ := openFiles(pid)
				if slices.Contains(names, lockPath) && (!wantNotify || try >= 40 || slices.Contains(names, notifyPath)) {
					return p
				}
			}
			_ = syscall.Kill(pid, syscall.SIGCONT)
			time.Sleep(time.Duration(5+e.rng.Intn(60)) * time.Millisecond)
		}
		p.wait(time.Second)
	}
	e.t.Fatalf("试了 80 次没能冻住拿着 tick.lock 的 tick")
	return nil
}

// taskJobs：起过模型的任务号。
func (e *stressEnv) taskJobs() map[string]bool {
	out := map[string]bool{}
	for _, c := range e.calls() {
		if c.Ev == "start" && c.Kind == "task" {
			out[c.Job] = true
		}
	}
	return out
}

// conclusion：条目合并了，或者标了卡住。
func (e *stressEnv) conclusion(dir, tok string) (merged, stuck bool) {
	b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "- [x] 条目 "+tok+" ") {
			merged = true
		}
		if strings.Contains(l, " "+tok+" ") && strings.Contains(l, "（卡住") {
			stuck = true
		}
	}
	return merged, stuck
}

// settleItems tick 到这些条目都有结论。隔一会儿把钟拨过孤儿标记的 24 小时：丢了记录的条目靠接手，不用真等。
func (e *stressEnv) settleItems(want map[string]string, limit time.Duration) {
	deadline := time.Now().Add(limit)
	lastJump := time.Now()
	for {
		e.agentq(200*time.Second, "tick")
		var open []string
		for tok, dir := range want {
			if merged, stuck := e.conclusion(dir, tok); !merged && !stuck {
				open = append(open, tok)
			}
		}
		if len(open) == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.violate("%s 后这些条目还没有结论：%v\n%s", limit, open, tailOf(e.mustState(), 1500))
			return
		}
		if time.Since(lastJump) > 8*time.Second {
			lastJump = time.Now()
			e.ahead += 25 * time.Hour
			e.extra = []string{"AGENTQ_TEST_NOW=" + time.Now().Add(e.ahead).Format(time.RFC3339)}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// finishRepos：门都有结论，没有运行中任务，每个项目不重复合并、BACKLOG 没留下没提交的改动。
func (e *stressEnv) finishRepos(dirs map[string]string) {
	e.agentq(90*time.Second, "pause")
	if e.ahead > 0 { // AGENTQ_TEST_NOW 是定住的：拨过任务超时，不然挂住的任务永远不超时
		e.ahead += time.Hour
		e.extra = []string{"AGENTQ_TEST_NOW=" + time.Now().Add(e.ahead).Format(time.RFC3339)}
	}
	e.settleGates(60 * time.Second)
	e.drain(70 * time.Second)
	for name, dir := range dirs {
		e.checkGateRepo(name, dir)
		if out, _ := e.git(dir, "status", "--porcelain", "--", "docs/BACKLOG.md"); out != "" {
			e.violate("项目 %s 的 BACKLOG 留下没提交的改动：%s", name, out)
		}
	}
}

// stoplock：SIGSTOP 冻住拿着 tick.lock 的 tick。冻住期间别的 tick 跳过，status 照常答并说清谁拿着锁，run 和 setup 等到上限如实报错，
// 听消息照常回；没有新任务被起。放开或杀掉之后接着干，不丢、不重复。
func scenarioStopLock(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("stoplock")
	e.telegram()
	e.cfg["max_parallel"], e.cfg["per_project"] = 3, 2
	e.limits(3, 2)
	e.writeConfig()
	want, dirs := map[string]string{}, map[string]string{}
	for _, name := range []string{"alpha", "beta"} {
		l1, t1 := e.item("slow")
		l2, t2 := e.item("ok")
		dirs[name] = e.repo(name, l1, l2)
		want[t1], want[t2] = dirs[name], dirs[name]
	}
	hangLine, _ := e.item("hang") // 一直活着：status 不能把它说成已结束
	dirs["gamma"] = e.repo("gamma", hangLine)
	e.listen()
	for i := rng.Intn(3); i > 0; i-- { // 冻住的时候可能已经有任务在跑
		e.agentq(120*time.Second, "tick")
	}
	p := e.stopTickHoldingLock(rng.Intn(2) == 0)
	pid := p.cmd.Process.Pid
	t.Logf("冻住 tick pid %d，用时 %s，notify.lock 也拿着：%v", pid, time.Since(p.start).Round(time.Millisecond), e.lockHeld("notify.lock"))
	time.Sleep(3 * time.Second) // 冻住之前刚起的外壳，等它把 claude 起来
	before := e.taskJobs()

	setup, run := e.bg("setup", "--yes"), e.bg("run", "alpha")
	for i := 0; i < 3; i++ {
		start := time.Now()
		out, code := e.agentq(60*time.Second, "tick")
		if code != 0 || !strings.Contains(out, "上一个 tick 还没结束，本次跳过") {
			e.violate("锁被冻住的 tick 拿着，tick 该如实跳过（退出码 %d）：\n%s", code, tailOf(out, 1000))
		}
		if took := time.Since(start); took > 30*time.Second {
			e.violate("拿不到锁的 tick 等了 %s", took)
		}
	}
	out, code := e.agentq(60*time.Second, "status")
	if code != 0 {
		e.violate("锁被拿着时 status 退出码 %d：\n%s", code, tailOf(out, 1000))
	}
	if !strings.Contains(out, fmt.Sprintf("pid %d 拿着 tick.lock", pid)) {
		e.violate("tick 卡着锁，status 没说：\n%s", tailOf(out, 1500))
	}
	if st, err := e.state(); err != nil {
		e.violate("锁被拿着时状态文件读不出：%v", err)
	} else {
		for _, j := range st.Tasks() {
			if stat := psField(j.PID, "stat"); j.PID > 0 && stat != "" && !strings.HasPrefix(stat, "Z") {
				for _, l := range strings.Split(out, "\n") {
					if strings.Contains(l, j.ID) && strings.Contains(l, "进程已结束") {
						e.violate("任务 %s 的进程还活着（pid %d），status 说已结束：%s", j.ID, j.PID, l)
					}
				}
			}
		}
	}
	// 听消息不等 tick：只有冻住的 tick 正好拿着 notify.lock 时才回不了
	if !e.lockHeld("notify.lock") {
		n := len(e.tg.sentCopy())
		e.tg.push(stressOwner, stressOwner, "状态", 0, time.Now())
		deadline := time.Now().Add(40 * time.Second)
		for len(e.tg.sentCopy()) == n && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if len(e.tg.sentCopy()) == n {
			e.violate("tick 被冻住，listen 40 秒没回消息")
		}
		if out, code := e.agentq(60*time.Second, "notify", "send", "done", "冻住期间的消息"); code != 0 {
			e.violate("notify.lock 没人拿，notify send 失败（退出码 %d）：%s", code, tailOf(out, 500))
		}
	} else if out, code := e.agentq(60*time.Second, "notify", "send", "done", "冻住期间的消息"); code == 0 || !strings.Contains(out, "拿不到") {
		e.violate("notify.lock 被冻住的 tick 拿着，notify send 该如实报错（退出码 %d）：%s", code, tailOf(out, 500))
	}

	out, code, took := setup.wait(170 * time.Second)
	if code == 0 || !strings.Contains(out, "background tick still running") {
		e.violate("锁被拿着时 setup 该如实报错（退出码 %d）：\n%s", code, tailOf(out, 1000))
	}
	if took < 85*time.Second || took > 150*time.Second {
		e.violate("setup 该等到上限（90 秒）再报错，实际 %s", took)
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".local", "bin", "agentq")); err == nil {
		e.violate("setup 没拿到锁，却装上了 ~/.local/bin/agentq")
	}
	if e.lockHeld(filepath.Join("..", ".local", "share", "agentq", "install.lock")) {
		e.violate("setup 退出后 install.lock 还被拿着")
	}
	out, code, took = run.wait(120 * time.Second)
	if code == 0 || !strings.Contains(out, "可能有 tick 卡住了") {
		e.violate("锁被拿着时 run 该如实报错（退出码 %d）：\n%s", code, tailOf(out, 1000))
	}
	if took < 55*time.Second {
		e.violate("run 该等到上限（60 秒）再报错，实际 %s", took)
	}
	for job := range e.taskJobs() {
		if !before[job] {
			e.violate("tick.lock 被冻住时起了新任务 %s", job)
		}
	}

	resumed := rng.Intn(2) == 0
	if resumed {
		t.Log("恢复：SIGCONT")
		_ = syscall.Kill(pid, syscall.SIGCONT)
		if out, code, _ := p.wait(200 * time.Second); code != 0 {
			e.violate("放开之后 tick 退出码 %d：\n%s", code, tailOf(out, 1500))
		}
	} else {
		t.Log("恢复：SIGKILL")
		_ = syscall.Kill(pid, syscall.SIGKILL)
		p.wait(30 * time.Second)
		// 只有冻住的 tick 死了锁才放开；这时可能轮到别的 tick 拿，一小会儿后必须有空的时候
		deadline := time.Now().Add(90 * time.Second)
		for e.lockHeld("tick.lock") && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if e.lockHeld("tick.lock") {
			e.violate("拿锁的 tick 死了 90 秒，tick.lock 还被拿着")
		}
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		out, code := e.agentq(200*time.Second, "tick")
		if code == 0 && !strings.Contains(out, "本次跳过") {
			break
		}
		if time.Now().After(deadline) {
			e.violate("恢复后 120 秒 tick 还是接不上（退出码 %d）：\n%s", code, tailOf(out, 1000))
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	e.settleItems(want, 200*time.Second)
	e.finishRepos(dirs)
	for tok, dir := range want {
		if merged, _ := e.conclusion(dir, tok); !merged && resumed {
			b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
			e.violate("SIGCONT 恢复后没有任务丢记录，条目 %s 却没合并：\n%s", tok, tailOf(string(b), 800))
		}
	}
	// 锁放开、任务收完，升级走得通
	if out, code := e.agentq(300*time.Second, "setup", "--yes"); code != 0 {
		e.violate("恢复后 setup 失败（退出码 %d）：\n%s", code, tailOf(out, 1500))
	} else if _, err := os.Lstat(filepath.Join(e.home, ".local", "bin", "agentq")); err != nil {
		e.violate("恢复后 setup 成功，却没有 ~/.local/bin/agentq：%v", err)
	}
	e.checkInvariants("stoplock")
}

// metaOK：meta.json 现在能读出来（坏的可能被收尾的外壳重写了）。
func metaOK(path, id string) bool {
	b, err := os.ReadFile(path)
	var j state.Job
	return err == nil && json.Unmarshal(b, &j) == nil && j.ID == id && j.Status != ""
}

// corruptboth：state.json 和任务的 meta.json 一起写坏。tick 备份坏的状态文件、能重建的重建、重建不了的留痕，那次不启动新任务，
// 之后接活：条目都有结论，不重复合并，交付过的不从头再来，BACKLOG 不乱。
func scenarioCorruptBoth(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("corruptboth")
	e.telegram()
	e.cfg["max_parallel"], e.cfg["per_project"] = 3, 2
	e.limits(3, 2)
	e.writeConfig()
	want, dirs := map[string]string{}, map[string]string{}
	for _, name := range []string{"alpha", "beta"} {
		var lines []string
		for _, mode := range []string{"ok", "slow", "ok"} {
			l, tok := e.item(mode)
			lines = append(lines, l)
			want[tok] = filepath.Join(e.root, name)
		}
		dirs[name] = e.repo(name, lines...)
	}
	// 破坏时还活着的任务：记录全丢了，进程还在，不能再起一个做同一条（假 claude 当场记 DUP）。一个认领了、一个没认领
	e.cfg["max_parallel"], e.cfg["timeout"] = 4, "25s"
	e.limits(4, 2)
	e.writeConfig()
	hangLine, _ := e.item("hang")
	dirs["gamma"] = e.repo("gamma", hangLine)
	claimLine, _ := e.item("claimhang")
	dirs["delta"] = e.repo("delta", claimLine)
	data := filepath.Join(e.home, ".agentq")
	statePath := filepath.Join(data, "state.json")
	// 有时等第一条合并了再破坏：坏掉之后不能再合并一次
	waitMerged := rng.Intn(2) == 0
	deadline := time.Now().Add(40 * time.Second)
	for {
		e.agentq(200*time.Second, "tick")
		st, err := e.state()
		enough := err == nil && len(st.Jobs) > 0
		if waitMerged {
			merged := false
			for tok, dir := range want {
				m, _ := e.conclusion(dir, tok)
				merged = merged || m
			}
			enough = enough && merged
		} else {
			enough = enough && rng.Intn(3) == 0
		}
		if enough {
			break
		}
		if time.Now().After(deadline) {
			e.violate("40 秒没等到要破坏的现场：%v", err)
			return
		}
		time.Sleep(time.Duration(50+rng.Intn(400)) * time.Millisecond)
	}
	st, err := e.state()
	if err != nil {
		e.violate("破坏之前状态文件就读不出：%v", err)
		return
	}
	var kinds []stressCorrupt
	for _, c := range stressCorruptions() {
		switch c.name {
		case "garbage", "truncate", "empty", "null", "emptyobj", "array", "halfjob":
			kinds = append(kinds, c)
		}
	}
	// 拿着 tick.lock 写坏：任务结束后的 tick 可能正在跑，它存状态会把破坏盖掉，这一轮就什么也没测到
	lock, err := fsx.AcquireLock(filepath.Join(data, "tick.lock"), 90*time.Second)
	if err != nil || lock == nil {
		e.violate("90 秒拿不到 tick.lock 来写坏文件：%v", err)
		return
	}
	if st, err = e.state(); err != nil {
		lock.Close()
		e.violate("破坏之前状态文件就读不出：%v", err)
		return
	}
	allMeta := rng.Intn(3) == 0
	badMeta := map[string]string{}
	for _, j := range st.Jobs {
		if allMeta || rng.Intn(2) == 0 || len(badMeta) == 0 {
			c := pick(rng, kinds)
			c.fn(filepath.Join(data, "jobs", j.ID, "meta.json"), rng)
			badMeta[j.ID] = c.name
		}
	}
	stateKind := pick(rng, kinds)
	stateKind.fn(statePath, rng)
	stateBad, _ := os.ReadFile(statePath)
	lock.Close()
	e.noState = true
	t.Logf("破坏 state.json：%s；%d/%d 个 meta.json：%v", stateKind.name, len(badMeta), len(st.Jobs), badMeta)
	// 还没 tick 时看状态的命令不崩（任务结束后的 tick 可能抢先修好，这里不查说了什么）
	for _, c := range [][]string{{"status"}, {"stats"}} {
		e.agentq(120*time.Second, c...)
	}
	out, code := e.agentq(200*time.Second, "tick")
	if code != 0 {
		e.violate("两个都坏了，tick 退出码 %d：\n%s", code, tailOf(out, 1500))
	}
	e.noState = false
	if _, err := e.state(); err != nil {
		e.violate("tick 过后状态文件还读不出：%v", err)
	}
	baks, _ := filepath.Glob(statePath + ".bad-*")
	found := false
	for _, b := range baks {
		if c, err := os.ReadFile(b); err == nil && bytes.Equal(c, stateBad) {
			found = true
		}
	}
	if !found {
		e.violate("坏的状态文件（%s）没有原样备份：%v", stateKind.name, baks)
	}
	// 重建的那次不启动。这次 tick 可能不是修好它的那个（任务结束后的 tick 也可能先修），都查
	logs, _ := os.ReadFile(filepath.Join(data, "launchd.log"))
	outs, _ := os.ReadFile(filepath.Join(e.base, "agentq-outs.log"))
	rebuilt := 0
	for _, l := range strings.Split(string(logs)+"\n"+string(outs), "\n") {
		if strings.Contains(l, "状态文件损坏后按 meta.json 重建") {
			rebuilt++
			if !strings.Contains(l, "启动 0") {
				e.violate("重建状态的那次 tick 启动了新任务：%s", l)
			}
		}
	}
	if rebuilt == 0 {
		e.violate("状态文件（%s）坏了，日志里没有重建的记录", stateKind.name)
	}
	// 重建不了的 meta.json 留痕：当天汇总写清是哪个任务
	nightly, _ := filepath.Glob(filepath.Join(e.home, ".claude", "nightly", "*-agentq.md"))
	var trail strings.Builder
	for _, n := range nightly {
		b, _ := os.ReadFile(n)
		trail.Write(b)
	}
	if !strings.Contains(trail.String(), "状态文件损坏") {
		e.violate("当天汇总没有写状态文件损坏：%v\n%s", nightly, tailOf(trail.String(), 800))
	}
	for id := range badMeta {
		if !metaOK(filepath.Join(data, "jobs", id, "meta.json"), id) && !strings.Contains(trail.String(), id) {
			e.violate("任务 %s 的 meta.json 读不出，当天汇总没写：\n%s", id, tailOf(trail.String(), 800))
		}
	}
	// 接着干
	e.settleItems(want, 200*time.Second)
	e.finishRepos(dirs)
	for _, c := range [][]string{{"status"}, {"stats"}, {"stats", "30"}} {
		if out, code := e.agentq(120*time.Second, c...); code != 0 {
			e.violate("恢复后 agentq %v 退出码 %d：\n%s", c, code, tailOf(out, 800))
		}
	}
	if out, code := e.agentq(200*time.Second, "tick"); code != 0 || !strings.Contains(out, "运行中") {
		e.violate("恢复后 tick 没正常完成（退出码 %d）：\n%s", code, tailOf(out, 1000))
	}
	e.checkInvariants("corruptboth")
}

// longrun：listen 常驻，几十次 tick，假 Telegram 长轮询、发消息、时不时坏一下。内存和文件句柄不能随轮数一直涨。
func scenarioLongRun(t *testing.T, rng *rand.Rand) {
	e := newStressEnv(t, rng, "")
	defer e.finish("longrun")
	e.telegram()
	e.tgCap, e.sameCap, e.modelCap = 3000, 2000, 800
	line, tok := e.item("ok")
	dir := e.repo("alpha", line)
	want := map[string]string{tok: dir}
	l := e.listen()
	if l == nil {
		return
	}
	pid := l.Process.Pid
	type sample struct{ rss, fds int }
	var samples []sample
	bad := []int32{tg5xx, tgGarbage, tgHalf}
	const warm = 10
	rounds := 40 + rng.Intn(11)
	for i := 0; i < rounds; i++ {
		for k := 0; k < 3; k++ {
			e.tg.push(stressOwner, stressOwner, "状态", 0, time.Now())
		}
		if i%5 == 2 { // 不走 Fast Path，进收件箱，起对话任务
			e.tg.push(stressOwner, stressOwner, fmt.Sprintf("第 %d 轮你好", i), 0, time.Now())
		}
		if i%6 == 3 {
			line, tok := e.item("ok")
			e.addTodo(dir, line)
			want[tok] = dir
		}
		if i%7 == 4 {
			e.tg.mode.Store(pick(rng, bad))
			time.Sleep(time.Duration(200+rng.Intn(500)) * time.Millisecond)
			e.tg.mode.Store(tgOK)
		}
		e.agentq(120*time.Second, "tick")
		time.Sleep(time.Duration(100+rng.Intn(300)) * time.Millisecond)
		stat := psField(pid, "stat")
		if stat == "" || strings.HasPrefix(stat, "Z") {
			e.violate("第 %d 轮 listen（pid %d）不在了：%q", i, pid, stat)
			return
		}
		rss, err := strconv.Atoi(psField(pid, "rss"))
		if err != nil || rss <= 0 {
			e.violate("第 %d 轮取不到 listen 的内存：%q %v", i, psField(pid, "rss"), err)
			return
		}
		names, err := openFiles(pid)
		if err != nil || len(names) == 0 {
			e.violate("第 %d 轮取不到 listen 的文件句柄：%d 个，%v", i, len(names), err)
			return
		}
		samples = append(samples, sample{rss, len(names)})
	}
	// 还能答，说明这一路没卡死（出过错的退避最多一分钟）
	n := len(e.tg.sentCopy())
	e.tg.push(stressOwner, stressOwner, "状态", 0, time.Now())
	deadline := time.Now().Add(90 * time.Second)
	for len(e.tg.sentCopy()) == n && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if len(e.tg.sentCopy()) == n {
		e.violate("长跑之后 listen 90 秒没回消息")
	}
	median := func(s []sample, get func(sample) int) int {
		var v []int
		for _, x := range s {
			v = append(v, get(x))
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	rssOf, fdsOf := func(s sample) int { return s.rss }, func(s sample) int { return s.fds }
	early, late := samples[warm:warm+5], samples[len(samples)-5:]
	rss0, rss1, fds0, fds1 := median(early, rssOf), median(late, rssOf), median(early, fdsOf), median(late, fdsOf)
	t.Logf("longrun %d 轮：内存 %d KB → %d KB，文件句柄 %d → %d，发出 %d 条消息", rounds, rss0, rss1, fds0, fds1, len(e.tg.sentCopy()))
	// 宽松阈值：只抓随轮数持续涨的。race 版的内存本来就抖。
	if rss1 > rss0+max(rss0/2, 64<<10) {
		e.violate("listen 内存随轮数涨：第 %d 轮附近 %d KB，最后 %d KB", warm, rss0, rss1)
	}
	if fds1 > fds0+8 {
		names, _ := openFiles(pid)
		e.violate("listen 文件句柄随轮数涨：第 %d 轮附近 %d 个，最后 %d 个：\n%s", warm, fds0, fds1, textfmt.TruncRunes(strings.Join(names, "\n"), 1500))
	}
	e.settleItems(want, 120*time.Second)
	e.finishRepos(map[string]string{"alpha": dir})
	e.checkInvariants("longrun")
}

func stressScenarios() []stressScenario {
	return []stressScenario{
		{"kill", 6, scenarioKill},
		{"killstart", 3, scenarioKillStart},
		{"timelapse", 3, scenarioTimelapse},
		{"modes", 2, scenarioModes},
		{"concurrent", 3, scenarioConcurrent},
		{"corrupt", 6, scenarioCorrupt},
		{"backlog", 4, scenarioBacklog},
		{"messages", 3, scenarioMessages},
		{"env", 5, scenarioEnv},
		{"features", 2, scenarioFeatures},
		{"diskfull", 1, scenarioDiskFull},
		{"cards", 4, scenarioCards},
		{"gate", 4, scenarioGate},
		{"gatequeue", 3, scenarioGateQueue},
		{"discuss", 3, scenarioDiscuss},
		{"notdoing", 2, scenarioNotDoing},
		{"stats", 1, scenarioStats},
		{"configrace", 3, scenarioConfigRace},
		{"codex", 4, scenarioCodex},
		{"opencode", 4, scenarioOpencode},
		{"stoplock", 2, scenarioStopLock},
		{"corruptboth", 4, scenarioCorruptBoth},
		{"longrun", 1, scenarioLongRun},
	}
}

func TestStress(t *testing.T) {
	if os.Getenv("AGENTQ_STRESS") != "1" {
		t.Skip("暴力测试要 AGENTQ_STRESS=1 才跑")
	}
	seed := time.Now().UnixNano() % 1000000
	if v := os.Getenv("AGENTQ_STRESS_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("AGENTQ_STRESS_SEED：%v", err)
		}
		seed = n
	}
	rounds := 30
	if v := os.Getenv("AGENTQ_STRESS_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("AGENTQ_STRESS_ROUNDS：%v", err)
		}
		rounds = n
	}
	only := os.Getenv("AGENTQ_STRESS_ONLY")
	var pool []stressScenario
	for _, s := range stressScenarios() {
		if only != "" && only != s.name {
			continue
		}
		for i := 0; i < s.weight; i++ {
			pool = append(pool, s)
		}
	}
	if len(pool) == 0 {
		t.Fatalf("没有场景 %q", only)
	}
	stressBuild(t)
	start := time.Now()
	fmt.Printf("暴力测试：种子 %d，%d 轮\n", seed, rounds)
	failed := 0
	for round := 0; round < rounds; round++ {
		rng := rand.New(rand.NewSource(seed*1000003 + int64(round)))
		s := pick(rng, pool)
		roundStart := time.Now()
		ok := t.Run(fmt.Sprintf("%03d-%s", round, s.name), func(t *testing.T) { s.fn(t, rng) })
		state := "通过"
		if !ok {
			state = "失败"
			failed++
		}
		fmt.Printf("种子 %d 第 %d 轮 %s %s（%s，累计 %s）\n", seed, round, s.name, state, time.Since(roundStart).Round(time.Second), time.Since(start).Round(time.Second))
	}
	fmt.Printf("暴力测试结束：种子 %d，%d 轮，失败 %d 轮，用时 %s\n", seed, rounds, failed, time.Since(start).Round(time.Second))
}
