package tests

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

// install.sh 的测试：临时 HOME、假 claude、假 launchctl，httptest 当 Release 服务器，程序是带 agentq_fake_deps 标记真实构建的（不联网取 Waza）。
// 不读写真实的 ~/.agentq、~/projects、~/.claude。真实下载 Waza 的那段放在 opt-in 的 TestInstallShReleaseAcceptance。

// 假 claude：能力探测的两个参数之外，其余和集成测试的 fakeClaude 一样（任务参数记进 AGENTQ_FAKE_LOG）。
var installFakeClaude = strings.Replace(fakeClaude, "#!/bin/sh\n", `#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
esac
`, 1)

// 假 launchctl：记录调用，用 $HOME/fake-launchd 下的文件表示服务已装上。
const installFakeLaunchctl = `#!/bin/sh
echo "$*" >> "$HOME/fake-launchctl-calls"
d=$HOME/fake-launchd
mkdir -p "$d"
case "$1" in
  print) [ -f "$d/${2##*/}" ] || { echo 'Could not find service'; exit 113; } ;;
  bootstrap) : > "$d/$(basename "$3" .plist)" ;;
  bootout) rm -f "$d/${2##*/}" ;;
esac
`

// 假 go：go build -o <输出> . 时把 $FAKE_GO_BINARY 拷过去，并记下工作目录和参数；FAKE_GO_FAIL 时报错。
const installFakeGo = `#!/bin/sh
echo "PWD=$PWD ARGS=$*" >> "$FAKE_LOG_DIR/go-calls"
[ -z "$FAKE_GO_FAIL" ] || { echo 'fake go: build failed' >&2; exit 1; }
cp "$FAKE_GO_BINARY" "$3"
`

// 假 git：clone 记下参数并建目录（FAKE_CLONE_FAIL 时报错），别的交给真 git。
// 真 git 的路径写进脚本：setup 探测时只传白名单环境变量，FAKE_* 传不过去。
const installFakeGit = `#!/bin/sh
if [ "$1" = clone ]; then
  echo "$*" >> "$FAKE_LOG_DIR/clone-calls"
  [ -z "$FAKE_CLONE_FAIL" ] || { echo 'fatal: could not read from remote repository' >&2; exit 128; }
  for last in "$@"; do :; done
  mkdir -p "$last"
  exit 0
fi
exec REAL_GIT "$@"
`

var installBinaries sync.Map // 版本号 -> 构建好的程序路径；同一次测试运行共用，省编译时间
var installBuildMu sync.Mutex
var tarGzCache sync.Map // 名字加内容哈希 -> 压好的包，只读

// installBinary 真实构建一个带 agentq_fake_deps 标记、写了版本号的程序。
func installBinary(t *testing.T, version string) []byte {
	t.Helper()
	installBuildMu.Lock() // 并行的用例不能同时写同一个输出文件
	defer installBuildMu.Unlock()
	if p, ok := installBinaries.Load(version); ok {
		return testkit.ReadBytes(t, p.(string))
	}
	out := filepath.Join(filepath.Dir(agentqBin), "fake-deps-"+version)
	cmd := exec.Command("go", "build", "-tags", "agentq_fake_deps", "-ldflags", "-X main.version="+version, "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建测试程序失败：%v\n%s", err, b)
	}
	installBinaries.Store(version, out)
	return testkit.ReadBytes(t, out)
}

// tarGz 打包。同样的内容只压一次：-race 下 gzip 压一次 12MB 的程序要 1 秒多，比装一遍还贵，几乎每个安装用例都要一份。
func tarGz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	key := name + "\x00" + sha256Hex(data)
	if b, ok := tarGzCache.Load(key); ok {
		return b.([]byte)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	w := tar.NewWriter(gz)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarGzCache.Store(key, buf.Bytes())
	return buf.Bytes()
}

// installKit 是一次 install.sh 运行的环境。PATH 只有 bin（假命令）和 tools（挑出来的真命令），没有 go、python3 和真 claude。
type installKit struct {
	t      *testing.T
	home   string
	bin    string
	tools  string
	log    string
	script string
	env    []string
	server *httptest.Server

	mu        sync.Mutex
	hits      []string
	files     map[string][]byte
	redirects map[string]string
}

// fillRepo 像 build-release.sh 一样把仓库名填进脚本。
func (k *installKit) fillRepo(repo string) {
	k.t.Helper()
	text := string(testkit.ReadBytes(k.t, k.script))
	filled := strings.Replace(text, "\tgithub_repo=__AGENTQ_REPO__\n", "\tgithub_repo="+repo+"\n", 1)
	if filled == text {
		k.t.Fatal("脚本里没有占位那一行")
	}
	testkit.WriteInstall(k.t, k.script, filled, 0o644)
}

// unsetBase 去掉 AGENTQ_RELEASE_BASE，走脚本自己的仓库名。
func (k *installKit) unsetBase() {
	var kept []string
	for _, kv := range k.env {
		if !strings.HasPrefix(kv, "AGENTQ_RELEASE_BASE=") {
			kept = append(kept, kv)
		}
	}
	k.env = kept
}

// useFakeCurl：记下参数，把 https://github.com 换成本地服务器后交给真 curl。
func (k *installKit) useFakeCurl() {
	k.t.Helper()
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		k.t.Skip("这台机器没有 curl")
	}
	k.writeBin("curl", `#!/bin/sh
echo "$*" >> "$FAKE_LOG_DIR/curl-calls"
n=$#
while [ "$n" -gt 1 ]; do set -- "$@" "$1"; shift; n=$((n-1)); done
url=$1
shift
case $url in https://github.com/*) url="`+k.server.URL+`${url#https://github.com}" ;; esac
exec `+fsx.ShQuote(realCurl)+` "$@" "$url"
`)
}

var installToolNames = []string{
	"sh", "bash", "git", "ps", "sed", "env", "date", "mkdir", "rm", "ls", "cat", "sleep", "dirname", "basename", "awk", "grep",
	"tar", "gzip", "uname", "mktemp", "mv", "cp", "chmod", "curl", "wget", "sha256sum", "shasum", "id", "head", "tail", "tr", "cut", "touch", "pwd",
}

// newInstallKit：without 里的命令不放进 PATH，用来模拟「缺」。
func newInstallKit(t *testing.T, without ...string) *installKit {
	t.Helper()
	k := &installKit{t: t, home: t.TempDir(), bin: t.TempDir(), tools: t.TempDir(), log: t.TempDir(), files: map[string][]byte{}}
	skip := map[string]bool{}
	for _, name := range without {
		skip[name] = true
	}
	for _, name := range installToolNames {
		if skip[name] {
			continue
		}
		if p, err := exec.LookPath(name); err == nil {
			if err := os.Symlink(p, filepath.Join(k.tools, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !skip["claude"] {
		k.writeBin("claude", installFakeClaude)
	}
	dir := t.TempDir() // 不是 checkout：目录里没有 go.mod
	k.script = filepath.Join(dir, "install.sh")
	testkit.WriteInstall(t, k.script, string(testkit.ReadBytes(t, "install.sh")), 0o644)
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.hits = append(k.hits, r.URL.Path)
		if to, ok := k.redirects[r.URL.Path]; ok {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		b, ok := k.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(k.server.Close)
	k.env = []string{
		"HOME=" + k.home, "PATH=" + k.bin + ":" + k.tools, "TMPDIR=" + t.TempDir(), "LANG=en_US.UTF-8", // 和真实用户一样是 UTF-8
		"AGENTQ_RELEASE_BASE=" + k.server.URL, "AGENTQ_FAKE_LOG=" + k.log, "FAKE_LOG_DIR=" + k.log,
		"AGENTQ_MACNOTIFY_LOG=" + filepath.Join(k.log, "macnotify.log"),
	}
	return k
}

func (k *installKit) writeBin(name, content string) {
	k.t.Helper()
	testkit.WriteInstall(k.t, filepath.Join(k.bin, name), content, 0o755)
}

func (k *installKit) platformFile() string {
	return "agentq_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
}

// serveRelease 把程序打成 tar 放到服务器上，校验和是对的。
func (k *installKit) serveRelease(version string) {
	k.t.Helper()
	data := tarGz(k.t, "agentq", installBinary(k.t, version))
	k.mu.Lock()
	defer k.mu.Unlock()
	k.files["/"+k.platformFile()] = data
	k.files["/SHA256SUMS"] = []byte(sha256Hex(data) + "  " + k.platformFile() + "\n")
}

func (k *installKit) setFile(path string, data []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.files[path] = data
}

func (k *installKit) requests() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.hits...)
}

func (k *installKit) exec(stdin io.Reader, dir string, name string, args ...string) (string, int) {
	k.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = k.env
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 没有控制终端：开发者在真终端里跑测试时也打不开 /dev/tty
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		k.t.Fatal(err)
	}
	return string(b), code
}

// run 执行 sh install.sh 参数。
func (k *installKit) run(args ...string) (string, int) {
	k.t.Helper()
	return k.exec(nil, k.home, "/bin/sh", append([]string{k.script}, args...)...)
}

// piped 模拟 curl | sh -s -- 参数：$0 是 sh，脚本从标准输入读。
func (k *installKit) piped(args ...string) (string, int) {
	k.t.Helper()
	return k.exec(bytes.NewReader(testkit.ReadBytes(k.t, k.script)), k.home, "/bin/sh", append([]string{"-s", "--"}, args...)...)
}

func (k *installKit) mustInstall(args ...string) string {
	k.t.Helper()
	out, code := k.run(args...)
	if code != 0 {
		k.t.Fatalf("install.sh 退出码 %d：\n%s", code, out)
	}
	return out
}

func (k *installKit) receiptPath() string {
	return filepath.Join(k.home, ".local", "share", "agentq", "receipt.json")
}

func (k *installKit) binPath() string { return filepath.Join(k.home, ".local", "bin", "agentq") }

func (k *installKit) configPath() string { return filepath.Join(k.home, ".agentq", "config.json") }

func (k *installKit) assertNotInstalled() {
	k.t.Helper()
	if exists(k.receiptPath()) {
		k.t.Error("不该有安装记录")
	}
	if _, err := os.Lstat(k.binPath()); err == nil {
		k.t.Error("不该装上程序")
	}
}

func (k *installKit) receipt() setup.InstallReceipt {
	k.t.Helper()
	var r setup.InstallReceipt
	if err := json.Unmarshal(testkit.ReadBytes(k.t, k.receiptPath()), &r); err != nil {
		k.t.Fatal(err)
	}
	return r
}

// agentq 执行装好的程序；服务调用走假 launchctl。
func (k *installKit) agentq(dir string, args ...string) (string, int) {
	k.t.Helper()
	return k.exec(nil, dir, k.binPath(), args...)
}

func (k *installKit) useFakeLaunchctl() {
	k.t.Helper()
	p := filepath.Join(k.bin, "launchctl")
	testkit.WriteInstall(k.t, p, installFakeLaunchctl, 0o755)
	k.env = append(k.env, "AGENTQ_TEST_LAUNCHCTL="+p)
}

func (k *installKit) useFakeGoAndGit(version string) {
	k.t.Helper()
	installBinary(k.t, version)
	p, _ := installBinaries.Load(version)
	k.writeBin("go", installFakeGo)
	realGit, err := exec.LookPath("git")
	if err != nil {
		k.t.Fatal(err)
	}
	k.writeBin("git", strings.Replace(installFakeGit, "REAL_GIT", fsx.ShQuote(realGit), 1))
	k.env = append(k.env, "FAKE_GO_BINARY="+p.(string))
}

func (k *installKit) logFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(k.log, name))
	return string(b)
}

// 新装：校验通过，setup 真的跑了（程序、技能、回执都在），打印 PATH 那一行和接下来三步。
func TestInstallShFreshInstall(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	out := k.mustInstall()
	testkit.AssertContains(t, out, "校验通过", "agentq v1.0.0", `export PATH="$HOME/.local/bin:$PATH"`, "agentq start")
	testkit.AssertContains(t, out, "Warning: python3 not found") // PATH 里没有 python3，也要装成
	r := k.receipt()
	if r.Scope != "full" || r.Extras {
		t.Fatalf("回执不对：%+v", r)
	}
	if !exists(filepath.Join(k.home, ".claude", "skills", "agent-task", "SKILL.md")) {
		t.Fatal("技能没装上")
	}
	if got, code := k.agentq(k.home, "version"); code != 0 || !strings.Contains(got, "v1.0.0") {
		t.Fatalf("装好的程序：%d %s", code, got)
	}
	cfg, _, err := setup.ReadInstallConfig(k.home)
	if err != nil || !cfg.Paused || cfg.PermissionMode != "default" {
		t.Fatalf("新配置应暂停、权限 default：%+v %v", cfg, err)
	}
	tmpLeft, _ := filepath.Glob(filepath.Join(strings.TrimPrefix(k.env[2], "TMPDIR="), "agentq-install.*"))
	if len(tmpLeft) != 0 {
		t.Fatalf("临时目录没清理：%v", tmpLeft)
	}
}

// PATH 已有 ~/.local/bin 时不打印那一行。
func TestInstallShNoPathHintWhenOnPath(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	k.env[1] = "PATH=" + k.bin + ":" + filepath.Join(k.home, ".local", "bin") + ":" + k.tools
	if out := k.mustInstall(); strings.Contains(out, "export PATH") {
		t.Fatalf("不该打印：\n%s", out)
	}
}

// curl | sh -s -- --extras：参数透传；重跑升级保留已选范围、手改配置和任务数据，说法换成版本和 doctor。
func TestInstallShPipedArgsAndUpgradeKeepData(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	out, code := k.piped("--extras")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	if !k.receipt().Extras {
		t.Fatal("--extras 没传给 setup")
	}
	first := k.receipt().Generation

	custom := strings.Replace(string(testkit.ReadBytes(t, k.configPath())), "{", "{\n  \"user_extension\" :   \"keep  me\",", 1)
	testkit.WriteInstall(t, k.configPath(), custom, 0o600)
	job := filepath.Join(k.home, ".agentq", "jobs", "old", "artifact")
	testkit.WriteInstall(t, job, "keep data", 0o600)

	k.serveRelease("v2.0.0")
	out = k.mustInstall()
	testkit.AssertContains(t, out, "已升级", "agentq v1.0.0", "agentq v2.0.0", "agentq doctor")
	if strings.Contains(out, "agentq init") {
		t.Fatalf("升级不该再讲上手三步：\n%s", out)
	}
	testkit.AssertBytes(t, k.configPath(), custom)
	testkit.AssertBytes(t, job, "keep data")
	r := k.receipt()
	if !r.Extras || r.Generation == first {
		t.Fatalf("升级后回执不对：%+v", r)
	}
	if got, _ := k.agentq(k.home, "version"); !strings.Contains(got, "v2.0.0") {
		t.Fatalf("没换成新版本：%s", got)
	}
}

// 校验不过、校验文件缺失或没有这一条、缺校验工具：退出，没装，也不换别的来源（有 Go 和 Git 也不去拉源码）。
func TestInstallShRefusesWithoutVerifiedChecksum(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		without []string
		prepare func(k *installKit)
		want    string
	}{
		{"校验和错误", nil, func(k *installKit) {
			k.setFile("/SHA256SUMS", []byte(strings.Repeat("0", 64)+"  "+k.platformFile()+"\n"))
		}, "校验不过"},
		{"校验文件拿不到", nil, func(k *installKit) {
			k.mu.Lock()
			defer k.mu.Unlock()
			delete(k.files, "/SHA256SUMS")
		}, "拿不到校验文件"},
		{"校验文件里没有这一条", nil, func(k *installKit) {
			k.setFile("/SHA256SUMS", []byte(strings.Repeat("0", 64)+"  agentq_other_arch.tar.gz\n"))
		}, "校验不过"},
		{"校验文件是空的", nil, func(k *installKit) { k.setFile("/SHA256SUMS", nil) }, "校验不过"},
		{"缺校验工具", []string{"sha256sum", "shasum"}, func(k *installKit) {}, "校验不了"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newInstallKit(t, c.without...)
			k.useFakeGoAndGit("v1.0.0")
			k.serveRelease("v1.0.0")
			c.prepare(k)
			out, code := k.run()
			if code == 0 {
				t.Fatalf("应退出非 0：\n%s", out)
			}
			testkit.AssertContains(t, out, c.want, "没有安装")
			k.assertNotInstalled()
			if k.logFile("clone-calls") != "" || k.logFile("go-calls") != "" {
				t.Fatalf("校验失败不该换来源：%s %s", k.logFile("clone-calls"), k.logFile("go-calls"))
			}
		})
	}
}

// tar 坏了、里面没有程序、程序跑不起来：校验和对也不装。
func TestInstallShBadArchive(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		data []byte
		want string
	}{
		"不是 tar": {[]byte("not a tarball"), "解压"},
		"没有程序":   {tarGz(t, "README", []byte("hi")), "解压"},
		"程序跑不起来": {tarGz(t, "agentq", []byte("not a program")), "跑不起来"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			k := newInstallKit(t)
			k.setFile("/"+k.platformFile(), c.data)
			k.setFile("/SHA256SUMS", []byte(sha256Hex(c.data)+"  "+k.platformFile()+"\n"))
			out, code := k.run()
			if code == 0 {
				t.Fatalf("应退出非 0：\n%s", out)
			}
			testkit.AssertContains(t, out, c.want)
			k.assertNotInstalled()
		})
	}
}

// 下载不到又没 Go：说清楚缺什么；不留下半截安装。
func TestInstallShNoReleaseNoGo(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	out, code := k.run()
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "拿不到程序", "下载不到发布版", k.server.URL+"/"+k.platformFile(), "没有 Go", "go.dev/dl", "sh install.sh")
	k.assertNotInstalled()
}

// 没有 curl 和 wget：跳过下载，说清楚。
func TestInstallShNoDownloader(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t, "curl", "wget")
	out, code := k.run()
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "没有 curl 或 wget", "没有 Go")
	if len(k.requests()) != 0 {
		t.Fatal("不该有下载")
	}
}

// 没有 curl 只有 wget：同样能装。这台机器没有 wget 时跳过。
func TestInstallShWgetFallback(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("这台机器没有 wget")
	}
	k := newInstallKit(t, "curl")
	k.serveRelease("v1.0.0")
	k.mustInstall()
	if !exists(k.receiptPath()) {
		t.Fatal("没装上")
	}
}

// 下载不到、有 Go：临时目录 clone --depth=1 后构建，再走 setup。clone 失败时说清楚。
func TestInstallShFallsBackToSource(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeGoAndGit("v3.0.0")
	out := k.mustInstall()
	testkit.AssertContains(t, out, "从源码构建", "agentq v3.0.0")
	clone := k.logFile("clone-calls")
	testkit.AssertContains(t, clone, "--depth=1", "agentq.git")
	if strings.Contains(clone, "--branch") {
		t.Fatalf("latest 不该带 --branch：%s", clone)
	}
	if !exists(k.receiptPath()) {
		t.Fatal("没装上")
	}

	k = newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeGoAndGit("v3.0.0")
	k.env = append(k.env, "AGENTQ_VERSION=v3.0.0")
	k.mustInstall()
	testkit.AssertContains(t, k.logFile("clone-calls"), "--branch v3.0.0")

	k = newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeGoAndGit("v3.0.0")
	k.env = append(k.env, "FAKE_CLONE_FAIL=1")
	out, code := k.run()
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "拿不到程序", "拉不到源码", "could not read")
	k.assertNotInstalled()

	k = newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeGoAndGit("v3.0.0")
	k.env = append(k.env, "FAKE_GO_FAIL=1")
	out, code = k.run()
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "拿不到程序", "源码构建失败")
	k.assertNotInstalled()
}

// 在本仓 checkout 里运行：用本地 go build，不下载；没 Go 时才下载；构建失败不偷偷换成下载的版本；curl | sh 不把当前目录当源码。
func TestInstallShCheckout(t *testing.T) {
	t.Parallel()
	checkout := func(t *testing.T, k *installKit) string {
		dir := t.TempDir()
		testkit.WriteInstall(t, filepath.Join(dir, "go.mod"), "module agentq\n\ngo 1.26.3\n", 0o644)
		testkit.WriteInstall(t, filepath.Join(dir, "main.go"), "package main\n", 0o644)
		testkit.WriteInstall(t, filepath.Join(dir, "install.sh"), string(testkit.ReadBytes(t, k.script)), 0o644)
		k.script = filepath.Join(dir, "install.sh")
		return dir
	}
	t.Run("本地构建", func(t *testing.T) {
		k := newInstallKit(t)
		k.useFakeGoAndGit("v5.0.0")
		k.serveRelease("v1.0.0")
		dir := checkout(t, k)
		out := k.mustInstall()
		testkit.AssertContains(t, out, "从本地源码构建", "agentq v5.0.0")
		testkit.AssertContains(t, k.logFile("go-calls"), "PWD="+dir, "build -o")
		if len(k.requests()) != 0 {
			t.Fatalf("不该下载：%v", k.requests())
		}
	})
	t.Run("没 Go 就下载", func(t *testing.T) {
		k := newInstallKit(t)
		k.serveRelease("v1.0.0")
		checkout(t, k)
		testkit.AssertContains(t, k.mustInstall(), "没有 Go", "校验通过", "agentq v1.0.0")
	})
	t.Run("构建失败不换来源", func(t *testing.T) {
		k := newInstallKit(t)
		k.useFakeGoAndGit("v5.0.0")
		k.serveRelease("v1.0.0")
		checkout(t, k)
		k.env = append(k.env, "FAKE_GO_FAIL=1")
		out, code := k.run()
		if code == 0 {
			t.Fatalf("应退出非 0：\n%s", out)
		}
		testkit.AssertContains(t, out, "本地构建失败")
		if len(k.requests()) != 0 {
			t.Fatal("不该换成下载")
		}
		k.assertNotInstalled()
	})
	t.Run("别的项目的 go.mod 不算", func(t *testing.T) {
		k := newInstallKit(t)
		k.useFakeGoAndGit("v5.0.0")
		k.serveRelease("v1.0.0")
		dir := checkout(t, k)
		testkit.WriteInstall(t, filepath.Join(dir, "go.mod"), "module other\n", 0o644)
		testkit.AssertContains(t, k.mustInstall(), "校验通过")
		if k.logFile("go-calls") != "" {
			t.Fatal("别的项目不该构建")
		}
	})
	t.Run("curl 管道不看当前目录", func(t *testing.T) {
		k := newInstallKit(t)
		k.useFakeGoAndGit("v5.0.0")
		k.serveRelease("v1.0.0")
		dir := checkout(t, k)
		out, code := k.exec(bytes.NewReader(testkit.ReadBytes(t, k.script)), dir, "/bin/sh", "-s")
		if code != 0 {
			t.Fatalf("%d\n%s", code, out)
		}
		testkit.AssertContains(t, out, "校验通过")
		if k.logFile("go-calls") != "" {
			t.Fatal("当前目录恰好是源码时也不该构建")
		}
	})
}

// 缺 Claude CLI、没登录（没有终端）：agentq 照装，不替用户装 CLI、不要 token，给官方命令，退出 0。有终端的情况见 install_cli_test.go。
func TestInstallShClaudePrerequisite(t *testing.T) {
	t.Parallel()
	t.Run("没有 CLI", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		k.serveRelease("v1.0.0")
		out := k.mustInstall()
		testkit.AssertContains(t, out, "curl -fsSL https://claude.ai/install.sh | bash", "claude auth login")
		if strings.Contains(strings.ToLower(out), "token") {
			t.Fatalf("不该提 token：\n%s", out)
		}
		if !exists(k.receiptPath()) {
			t.Fatal("agentq 应该装上")
		}
	})
	t.Run("没登录", func(t *testing.T) {
		k := newInstallKit(t)
		k.env = append(k.env, "AGENTQ_FAKE_LOGGED_OUT=1")
		k.serveRelease("v1.0.0")
		out := k.mustInstall()
		testkit.AssertContains(t, out, "还没登录", "claude auth login")
		if !exists(k.receiptPath()) {
			t.Fatal("agentq 应该装上")
		}
	})
	t.Run("--claude 指定的路径不对仍然报错", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		k.serveRelease("v1.0.0")
		out, code := k.run("--claude", filepath.Join(k.home, "nope"))
		if code == 0 {
			t.Fatalf("应退出非 0：\n%s", out)
		}
		k.assertNotInstalled()
	})
	t.Run("配置里自定义的路径不对仍然报错", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		k.serveRelease("v1.0.0")
		testkit.WriteInstall(t, k.configPath(), fmt.Sprintf("{\"claude_path\": %q}\n", filepath.Join(k.home, "nope")), 0o600)
		if out, code := k.run(); code == 0 {
			t.Fatalf("应退出非 0：\n%s", out)
		}
		k.assertNotInstalled()
	})
	t.Run("不在 PATH 但在官方安装位置", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		testkit.WriteInstall(t, filepath.Join(k.home, ".local", "bin", "claude"), installFakeClaude, 0o755)
		k.serveRelease("v1.0.0")
		k.mustInstall()
		if !exists(k.receiptPath()) {
			t.Fatal("没装上")
		}
	})
	t.Run("已有配置交给 setup 判断", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		k.serveRelease("v1.0.0")
		cli := filepath.Join(k.home, "custom place", "claude")
		testkit.WriteInstall(t, cli, installFakeClaude, 0o755)
		testkit.WriteInstall(t, k.configPath(), fmt.Sprintf("{\"claude_path\": %q}\n", cli), 0o600)
		k.mustInstall()
		if !exists(k.receiptPath()) {
			t.Fatal("配置里的 claude_path 应该够用")
		}
	})
	t.Run("--claude 交给 setup 判断", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		k.serveRelease("v1.0.0")
		cli := filepath.Join(k.home, "custom place", "claude")
		testkit.WriteInstall(t, cli, installFakeClaude, 0o755)
		k.mustInstall("--claude", cli)
		cfg, _, err := setup.ReadInstallConfig(k.home)
		if err != nil || cfg.ClaudePath != cli {
			t.Fatalf("%+v %v", cfg, err)
		}
	})
	t.Run("只装技能不要 CLI", func(t *testing.T) {
		k := newInstallKit(t, "claude")
		k.serveRelease("v1.0.0")
		out := k.mustInstall("--scope", "skills")
		testkit.AssertContains(t, out, "技能装好了")
		if strings.Contains(out, "agentq init") {
			t.Fatalf("这个范围没有后台，不该讲三步：\n%s", out)
		}
		if _, err := os.Lstat(k.binPath()); err == nil {
			t.Fatal("这个范围不该装程序")
		}
	})
}

// 缺 Git：按平台给说明，不下载。
func TestInstallShMissingGit(t *testing.T) {
	t.Parallel()
	for os_, want := range map[string]string{"Darwin": "xcode-select --install", "Linux": "包管理器"} {
		t.Run(os_, func(t *testing.T) {
			k := newInstallKit(t, "git")
			k.writeBin("uname", fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s ;; *) echo arm64 ;; esac\n", os_))
			out, code := k.run()
			if code == 0 {
				t.Fatalf("应退出非 0：\n%s", out)
			}
			testkit.AssertContains(t, out, "需要 Git", want)
			if len(k.requests()) != 0 {
				t.Fatal("前提不满足不该下载")
			}
		})
	}
}

// 平台：四个组合取对文件名，不支持的给明确说明。
func TestInstallShPlatforms(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ os, arch, want string }{
		{"Darwin", "arm64", "/agentq_darwin_arm64.tar.gz"},
		{"Darwin", "x86_64", "/agentq_darwin_amd64.tar.gz"},
		{"Linux", "aarch64", "/agentq_linux_arm64.tar.gz"},
		{"Linux", "arm64", "/agentq_linux_arm64.tar.gz"},
		{"Linux", "x86_64", "/agentq_linux_amd64.tar.gz"},
	} {
		t.Run(c.os+"_"+c.arch, func(t *testing.T) {
			k := newInstallKit(t)
			k.writeBin("uname", fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s ;; *) echo %s ;; esac\n", c.os, c.arch))
			if out, code := k.run(); code == 0 {
				t.Fatalf("没放文件应失败：\n%s", out)
			}
			if got := k.requests(); len(got) == 0 || got[0] != c.want {
				t.Fatalf("取的文件不对：%v", got)
			}
		})
	}
	for _, c := range []struct{ os, arch, want string }{
		{"FreeBSD", "amd64", "不支持 FreeBSD"},
		{"MINGW64_NT-10.0", "x86_64", "不支持 MINGW64"},
		{"Linux", "riscv64", "不支持 riscv64"},
	} {
		t.Run(c.os+"_"+c.arch, func(t *testing.T) {
			k := newInstallKit(t)
			k.writeBin("uname", fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s ;; *) echo %s ;; esac\n", c.os, c.arch))
			out, code := k.run()
			if code == 0 {
				t.Fatalf("应退出非 0：\n%s", out)
			}
			testkit.AssertContains(t, out, c.want, "macOS", "Linux")
			if len(k.requests()) != 0 {
				t.Fatal("不该下载")
			}
		})
	}
}

// AGENTQ_VERSION：取那个版本的目录；非法字符拒绝。
func TestInstallShVersion(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.env = append(k.env, "AGENTQ_VERSION=v1.2.3")
	data := tarGz(t, "agentq", installBinary(t, "v1.2.3"))
	k.setFile("/v1.2.3/"+k.platformFile(), data)
	k.setFile("/v1.2.3/SHA256SUMS", []byte(sha256Hex(data)+" *"+k.platformFile()+"\n")) // 二进制模式的写法也认
	testkit.AssertContains(t, k.mustInstall(), "agentq v1.2.3")

	for _, bad := range []string{"../../etc", "--help", "v1 2"} {
		k = newInstallKit(t)
		k.env = append(k.env, "AGENTQ_VERSION="+bad)
		out, code := k.run()
		if code == 0 || !strings.Contains(out, "AGENTQ_VERSION") {
			t.Fatalf("%q 应拒绝：%d\n%s", bad, code, out)
		}
		if len(k.requests()) != 0 {
			t.Fatalf("%q 不该下载", bad)
		}
	}
}

// --plan：不激活，不讲上手；--help：什么都不做。
func TestInstallShPlanAndHelp(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	out := k.mustInstall("--plan")
	testkit.AssertContains(t, out, "只看了计划", "Plan only")
	if strings.Contains(out, "agentq init") {
		t.Fatalf("不该讲三步：\n%s", out)
	}
	k.assertNotInstalled()

	k = newInstallKit(t)
	out, code := k.run("--help")
	if code != 0 || !strings.Contains(out, "用法") {
		t.Fatalf("%d\n%s", code, out)
	}
	if len(k.requests()) != 0 {
		t.Fatal("--help 不该下载")
	}
}

// curl 只下载到一半：截断在任何位置，sh 都不执行任何动作（没有请求、HOME 没有变化、不调假 claude、不打印进度）。
func TestInstallShTruncatedDoesNothing(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	full := testkit.ReadBytes(t, k.script)
	if !bytes.HasSuffix(full, []byte("\n{ main \"$@\"; }\n")) {
		t.Fatal("脚本最后一行应该是调用 main 的花括号")
	}
	var cuts []int
	for i, b := range full {
		if b == '\n' {
			cuts = append(cuts, i+1)
		}
	}
	cuts = cuts[:len(cuts)-1] // 最后一个是完整文件
	for n := len(full) - 2; n > len(full)-40; n-- {
		cuts = append(cuts, n) // 逐字节截断最后一行：`main`、`{ main "$@"` 这些前缀都不能跑
	}
	for _, n := range cuts {
		cut := filepath.Join(t.TempDir(), "install.sh")
		testkit.WriteInstall(t, cut, string(full[:n]), 0o644)
		for mode, run := range map[string]func() (string, int){
			"文件": func() (string, int) { return k.exec(nil, k.home, "/bin/sh", cut) },
			"管道": func() (string, int) { return k.exec(bytes.NewReader(full[:n]), k.home, "/bin/sh", "-s", "--") },
		} {
			out, _ := run()
			if strings.Contains(out, "下载") || strings.Contains(out, "agentq 安装：") {
				t.Fatalf("%s 截断在第 %d 字节不该有动作：\n%s", mode, n, out)
			}
			entries, err := os.ReadDir(k.home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("%s 截断在第 %d 字节动了 HOME：%v", mode, n, entries)
			}
			if got := k.requests(); len(got) != 0 {
				t.Fatalf("%s 截断在第 %d 字节发了请求：%v", mode, n, got)
			}
		}
	}
	// 对照：完整的脚本确实会动作，上面的断言不是空转。
	k.mustInstall()
	if len(k.requests()) == 0 {
		t.Fatal("完整脚本应该下载")
	}
}

// 老用户：只装了二进制的旧版，setup 不覆盖，脚本说清怎么腾位置；照做后装上，配置逐字节保留；没有 permission_mode 键的 start 不问。
func TestInstallShLegacyBinaryUser(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.useFakeLaunchctl()
	k.serveRelease("v1.0.0")
	// start 那次 tick 会巡逻；机器磁盘紧张时巡逻起一个后台分析任务，要等它写完再删临时目录。
	e := &testEnv{t: t, home: k.home, root: filepath.Join(k.home, "projects"), fake: filepath.Join(k.bin, "claude"), fakeLg: k.log}
	t.Cleanup(e.waitShells)
	testkit.WriteInstall(t, k.binPath(), "#!/bin/sh\n# 旧版\nexit 0\n", 0o755)
	guard := filepath.Join(k.home, ".agentq", "tick-guard.sh")
	testkit.WriteInstall(t, guard, "#!/bin/sh\n# 旧保护脚本\n", 0o755)
	legacy := "{\n  \"claude_path\": \"" + filepath.Join(k.bin, "claude") + "\",\n  \"projects_root\": \"~/projects\",\n  \"paused\": true,\n  \"mine\":   1\n}\n"
	testkit.WriteInstall(t, k.configPath(), legacy, 0o600)

	out, code := k.run()
	if code == 0 {
		t.Fatalf("旧二进制在时应停下：\n%s", out)
	}
	testkit.AssertContains(t, out, "not owned by agentq", "agentq uninstall", "mv ~/.local/bin/agentq", "tick-guard.sh", "配置和任务数据都保留")
	testkit.AssertBytes(t, k.binPath(), "#!/bin/sh\n# 旧版\nexit 0\n")
	testkit.AssertBytes(t, k.configPath(), legacy)
	if exists(k.receiptPath()) {
		t.Fatal("不该有回执")
	}

	// 照提示做（uninstall 是旧版自己的命令，假的旧版没有服务要卸）
	if err := os.Rename(k.binPath(), k.binPath()+".legacy"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(guard, guard+".legacy"); err != nil {
		t.Fatal(err)
	}
	k.mustInstall()
	testkit.AssertBytes(t, k.configPath(), legacy)
	got, code := k.agentq(k.home, "start")
	if code != 0 {
		t.Fatalf("老配置 start：%d\n%s", code, got)
	}
	if strings.Contains(got, "所有文件") {
		t.Fatalf("没有 permission_mode 键的不该问：\n%s", got)
	}
	want, err := config.SetJSONKey([]byte(legacy), "paused", "false")
	if err != nil {
		t.Fatal(err)
	}
	testkit.AssertBytes(t, k.configPath(), string(want))
}

// 端到端：install → init → add → start（假 claude、假 launchctl），任务真的被拉起，claude 收到 --permission-mode bypassPermissions；
// 任务结束后重跑 install.sh 升级，服务和开关都留着。
func TestOnboardingEndToEnd(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.useFakeLaunchctl()
	k.serveRelease("v1.0.0")
	e := &testEnv{t: t, home: k.home, root: filepath.Join(k.home, "projects"), fake: filepath.Join(k.bin, "claude"), fakeLg: k.log}
	t.Cleanup(e.waitShells)
	k.mustInstall()

	project := filepath.Join(k.home, "projects", "my-app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn := func(args ...string) string {
		out, code := k.exec(nil, project, filepath.Join(k.tools, "git"), args...)
		if code != 0 {
			t.Fatalf("git %v：\n%s", args, out)
		}
		return strings.TrimSpace(out)
	}
	gitIn("init", "-q", "-b", "main")
	gitIn("config", "user.name", "t")
	gitIn("config", "user.email", "t")

	if out, code := k.agentq(project, "init"); code != 0 {
		t.Fatalf("init：%d\n%s", code, out)
	}
	title := "登录失败时显示具体错误"
	if out, code := k.agentq(project, "add", title+" SLEEP=1"); code != 0 {
		t.Fatalf("add：%d\n%s", code, out)
	} else {
		testkit.AssertContains(t, out, "agentq start")
	}

	// 标准输入是 /dev/null（不是终端）、没带 --accept-bypass：拒绝，什么都不改，任务不启动。
	before := string(testkit.ReadBytes(t, k.configPath()))
	out, code := k.agentq(project, "start")
	if code == 0 {
		t.Fatalf("非终端应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "--accept-bypass", "所有文件")
	testkit.AssertBytes(t, k.configPath(), before)

	out, code = k.agentq(project, "start", "--accept-bypass")
	if code != 0 {
		t.Fatalf("start：%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "my-app", title, "agentq status", "agentq pause")
	testkit.AssertBytes(t, k.configPath(), func() string {
		b, err := config.SetJSONKey([]byte(before), "permission_mode", `"bypassPermissions"`)
		if err == nil {
			b, err = config.SetJSONKey(b, "paused", "false")
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}())

	// 假 claude 真的被拉起，命令行带完全权限
	deadline := time.Now().Add(20 * time.Second)
	var args string
	for time.Now().Before(deadline) && args == "" {
		logs, _ := filepath.Glob(filepath.Join(k.log, "*.txt"))
		for _, f := range logs {
			if b := testkit.ReadBytes(t, f); strings.Contains(string(b), "PPID[") {
				args = string(b)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if args == "" {
		t.Fatalf("假 claude 没被拉起：\n%s", out)
	}
	testkit.AssertContains(t, args, "ARG[--permission-mode]\nARG[bypassPermissions]\n", "PWD["+testkit.RealPath(t, project)+"]", title)

	calls := string(testkit.ReadBytes(t, filepath.Join(k.home, "fake-launchctl-calls")))
	testkit.AssertContains(t, calls, "bootstrap")
	if strings.Contains(calls, ".listen") {
		t.Fatalf("不该碰 Telegram listen：\n%s", calls)
	}

	// 再跑一次 start：可重复，不换代，不重复接同一条。
	gen := k.receipt().Generation
	if out, code := k.agentq(project, "start"); code != 0 {
		t.Fatalf("重复 start：%d\n%s", code, out)
	}
	if k.receipt().Generation != gen {
		t.Fatal("重复 start 不该换代")
	}
	e.waitShells()

	// 任务收尾后重跑 install.sh：升级不丢服务和开关。
	waitNoRunningJobs(t, k)
	k.serveRelease("v2.0.0")
	cfgBefore := string(testkit.ReadBytes(t, k.configPath()))
	testkit.AssertContains(t, k.mustInstall(), "已升级", "v2.0.0")
	testkit.AssertBytes(t, k.configPath(), cfgBefore)
	if len(k.receipt().Services) != 3 {
		t.Fatalf("升级丢了服务：%v", k.receipt().Services)
	}
	if out, code := k.agentq(project, "doctor"); code != 0 {
		t.Fatalf("doctor：%d\n%s", code, out)
	}
}

// setup --listen 透传到同一引擎：装上监听服务，升级时保留；单独 --listen 被拒绝。
func TestInstallShListenFlag(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.useFakeLaunchctl()
	k.serveRelease("v1.0.0")
	out, code := k.run("--listen")
	if code == 0 {
		t.Fatalf("没选 --services 应拒绝：\n%s", out)
	}
	testkit.AssertContains(t, out, "--listen requires --services")
	k.assertNotInstalled()

	k.mustInstall("--services", "--listen")
	label := config.LaunchdLabel + ".listen"
	services := func() string { return strings.Join(k.receipt().Services, " ") }
	testkit.AssertContains(t, services(), label)
	if !exists(filepath.Join(k.home, "fake-launchd", label)) {
		t.Fatal("监听服务没装上")
	}
	k.serveRelease("v2.0.0")
	testkit.AssertContains(t, k.mustInstall(), "已升级")
	testkit.AssertContains(t, services(), label)
}

func waitNoRunningJobs(t *testing.T, k *installKit) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := setup.InstallerIdle(k.home); err == nil {
			return
		}
		if out, code := k.agentq(k.home, "tick"); code != 0 {
			t.Logf("tick：%d %s", code, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("任务一直没收尾")
}

// bootstrap.sh：没有 python3 也往下走（只给一行提示）。源码树是临时拷的，go 是假的。
func TestBootstrapWorksWithoutPython(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	stub := filepath.Join(k.log, "stub-agentq")
	testkit.WriteInstall(t, stub, "#!/bin/sh\necho \"stub agentq $*\"\n", 0o755)
	k.writeBin("go", installFakeGo)
	k.env = append(k.env, "FAKE_GO_BINARY="+stub)
	src := t.TempDir()
	script := filepath.Join(src, "scripts", "bootstrap.sh")
	testkit.WriteInstall(t, script, string(testkit.ReadBytes(t, filepath.Join("scripts", "bootstrap.sh"))), 0o755)
	out, code := k.exec(nil, src, "/bin/sh", script, "--yes")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "Python 3 not found", "stub agentq setup --yes")
}

// Linux 上 sh 多半是 dash，比 macOS 的 sh 严格：装一遍、截断一遍。
func TestInstallShUnderDash(t *testing.T) {
	t.Parallel()
	const dash = "/bin/dash"
	if _, err := os.Stat(dash); err != nil {
		t.Skip("这台机器没有 dash")
	}
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	full := testkit.ReadBytes(t, k.script)
	for _, n := range []int{len(full) / 2, len(full) - 2, len(full) - 8, len(full) - 12} {
		out, _ := k.exec(bytes.NewReader(full[:n]), k.home, dash, "-s", "--")
		if strings.Contains(out, "下载") || len(k.requests()) != 0 {
			t.Fatalf("截断在第 %d 字节不该有动作：\n%s", n, out)
		}
	}
	out, code := k.exec(bytes.NewReader(full), k.home, dash, "-s", "--", "--extras")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "校验通过", "agentq start")
	if !k.receipt().Extras {
		t.Fatal("参数没传给 setup")
	}
}

// 原文件没填仓库名、也没给下载地址：不发请求，说明要用 Release 附件的脚本或 clone；有 Go 也拉不了源码。
func TestInstallShUnfilledPlaceholder(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.useFakeCurl()
	k.useFakeGoAndGit("v1.0.0")
	k.unsetBase()
	out, code := k.run()
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "没填下载地址", "releases/latest/download/install.sh", "git clone", "sh install.sh", "拉不了源码")
	if strings.Contains(out, "__AGENTQ_REPO__") {
		t.Fatalf("不该把占位符露给用户：\n%s", out)
	}
	if k.logFile("curl-calls") != "" || k.logFile("clone-calls") != "" {
		t.Fatalf("不该联网：%s %s", k.logFile("curl-calls"), k.logFile("clone-calls"))
	}
	k.assertNotInstalled()
}

// 没填占位但给了 AGENTQ_RELEASE_BASE：照常下载（测试和镜像）。
func TestInstallShUnfilledWithBase(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	k.mustInstall()
	if !exists(k.receiptPath()) {
		t.Fatal("没装上")
	}
}

// 填好仓库名：latest 和指定版本都取 GitHub 的 Release 地址，带 -L。
func TestInstallShFilledRepo(t *testing.T) {
	t.Parallel()
	data := tarGz(t, "agentq", installBinary(t, "v1.0.0"))
	serve := func(k *installKit, prefix string) {
		k.setFile(prefix+"/"+k.platformFile(), data)
		k.setFile(prefix+"/SHA256SUMS", []byte(sha256Hex(data)+"  "+k.platformFile()+"\n"))
	}
	k := newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeCurl()
	k.unsetBase()
	serve(k, "/some-owner/agentq/releases/latest/download")
	testkit.AssertContains(t, k.mustInstall(), "校验通过", "v1.0.0")
	calls := k.logFile("curl-calls")
	testkit.AssertContains(t, calls, "-fsSL", "https://github.com/some-owner/agentq/releases/latest/download/"+k.platformFile(), "https://github.com/some-owner/agentq/releases/latest/download/SHA256SUMS")

	k = newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeCurl()
	k.unsetBase()
	k.env = append(k.env, "AGENTQ_VERSION=v1.0.0")
	serve(k, "/some-owner/agentq/releases/download/v1.0.0")
	k.mustInstall()
	testkit.AssertContains(t, k.logFile("curl-calls"), "https://github.com/some-owner/agentq/releases/download/v1.0.0/"+k.platformFile())
}

// AGENTQ_RELEASE_BASE 优先于仓库名。
func TestInstallShBaseOverridesRepo(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	k.fillRepo("some-owner/agentq")
	k.useFakeCurl()
	k.serveRelease("v1.0.0")
	k.mustInstall()
	calls := k.logFile("curl-calls")
	if strings.Contains(calls, "github.com") {
		t.Fatalf("有下载地址时不该去 GitHub：%s", calls)
	}
	testkit.AssertContains(t, calls, k.server.URL+"/"+k.platformFile())
}

// GitHub 的 latest 是重定向：真 curl 要跟过去，程序和校验文件都一样。
func TestInstallShFollowsRedirects(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t)
	data := tarGz(t, "agentq", installBinary(t, "v1.0.0"))
	k.setFile("/cdn/"+k.platformFile(), data)
	k.setFile("/cdn/SHA256SUMS", []byte(sha256Hex(data)+"  "+k.platformFile()+"\n"))
	k.mu.Lock()
	k.redirects = map[string]string{"/" + k.platformFile(): "/cdn/" + k.platformFile(), "/SHA256SUMS": "/cdn/SHA256SUMS"}
	k.mu.Unlock()
	testkit.AssertContains(t, k.mustInstall(), "校验通过")
	if !exists(k.receiptPath()) {
		t.Fatal("没装上")
	}
}

// opt-in：发行构建的真程序 + 真实联网取锁定的 Waza，走完 Release 附件的 install.sh 和升级。其余仍是临时 HOME、假 claude。
func TestInstallShReleaseAcceptance(t *testing.T) {
	if os.Getenv("AGENTQ_BOOTSTRAP_ACCEPTANCE") != "1" {
		t.Skip("opt-in network acceptance for install.sh")
	}
	k := newInstallKit(t)
	dist := t.TempDir()
	cmd := exec.Command("/bin/sh", filepath.Join("scripts", "build-release.sh"), "v0.0.0-acceptance", dist, "some-owner/agentq")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	testkit.WriteInstall(t, k.script, string(testkit.ReadBytes(t, filepath.Join(dist, "install.sh"))), 0o644)
	k.setFile("/"+k.platformFile(), testkit.ReadBytes(t, filepath.Join(dist, k.platformFile())))
	k.setFile("/SHA256SUMS", testkit.ReadBytes(t, filepath.Join(dist, "SHA256SUMS")))
	testkit.AssertContains(t, k.mustInstall(), "校验通过", "v0.0.0-acceptance")
	for _, name := range []string{"check", "think", "hunt", "ui"} {
		if !exists(filepath.Join(k.home, ".claude", "skills", name, "SKILL.md")) {
			t.Fatalf("%s 没装上", name)
		}
	}
	testkit.AssertContains(t, k.mustInstall(), "已升级")
	if out, code := k.agentq(k.home, "doctor"); code != 0 {
		t.Fatalf("doctor：%d\n%s", code, out)
	}
}
