package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// viaMirror 把 github.com 的下载地址放到镜像前缀后面；别的地址不动。校验文件不走这里。
func viaMirror(mirror, rawURL string) string {
	if mirror == "" || !strings.HasPrefix(rawURL, "https://github.com/") {
		return rawURL
	}
	return strings.TrimRight(mirror, "/") + "/" + rawURL
}

// checksumFor 从 SHA256SUMS 的内容里取某个文件的校验和，文本和二进制两种写法都认。
func checksumFor(sums, file string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[1] == file || f[1] == "*"+file) {
			return f[0]
		}
	}
	return ""
}

func validVersion(v string) bool {
	if v == "" || strings.HasPrefix(v, "-") {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)) {
			return false
		}
	}
	return true
}

func downloadBytes(ctx context.Context, c *http.Client, rawURL string) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if e != nil {
		return nil, fmt.Errorf("下载 %s：%w", rawURL, e)
	}
	resp, e := c.Do(req)
	if e != nil {
		return nil, fmt.Errorf("下载 %s：%w", rawURL, e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s：HTTP %d", rawURL, resp.StatusCode)
	}
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		return nil, fmt.Errorf("下载 %s：%w", rawURL, e)
	}
	return b, nil
}

// updateHowTo：程序里没编进仓库名时只说明，不猜。
const updateHowTo = `这个程序是从源码构建的，里面没有发布地址，agentq update 不猜。升级办法：
  在源码目录 git pull，再运行 sh install.sh
  或重跑当初那条一键安装命令（Release 附件里的 install.sh）`

// RunUpdate 是 agentq update：取发布地址上的 install.sh，对过校验再运行，真正的升级和一键脚本是同一条路。
// repo 由发行构建写进程序；AGENTQ_RELEASE_BASE 优先，和脚本一致。参数原样交给脚本。
func RunUpdate(repo string, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("update", flag.ContinueOnError)
	f.SetOutput(stderr)
	version := f.String("version", "", "要升到的版本，默认 latest；main 走源码")
	proxy := f.String("proxy", os.Getenv("AGENTQ_PROXY"), "下载走的代理")
	mirror := f.String("mirror", os.Getenv("AGENTQ_MIRROR"), "GitHub 下载镜像前缀；校验文件不走镜像")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprintln(stderr, "agentq update：多余的参数")
		return 2
	}
	want := *version
	if want == "" {
		want = "latest"
	}
	if !validVersion(want) {
		fmt.Fprintf(stderr, "agentq update：版本不对：%s\n", want)
		return 2
	}
	base := strings.TrimRight(os.Getenv("AGENTQ_RELEASE_BASE"), "/")
	switch {
	case base != "":
		if want != "latest" && want != "main" {
			base += "/" + want
		}
	case repo == "":
		fmt.Fprintln(stderr, updateHowTo)
		return 1
	case want == "latest" || want == "main": // main 没有 Release，用最新的脚本去拉源码
		base = "https://github.com/" + repo + "/releases/latest/download"
	default:
		base = "https://github.com/" + repo + "/releases/download/" + want
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if e := runUpdateScript(ctx, base, *proxy, *mirror, args, stdout, stderr); e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(stderr, "agentq update：", e)
		return 1
	}
	return 0
}

func runUpdateScript(ctx context.Context, base, proxy, mirror string, args []string, stdout, stderr io.Writer) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, e := url.Parse(proxy)
		if e != nil {
			return fmt.Errorf("代理地址不对：%w", e)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{Timeout: 2 * time.Minute, Transport: transport}
	script, e := downloadBytes(ctx, client, viaMirror(mirror, base+"/install.sh"))
	if e != nil {
		return e
	}
	sums, e := downloadBytes(ctx, client, base+"/SHA256SUMS")
	if e != nil {
		return fmt.Errorf("拿到了安装脚本，但拿不到校验文件，没有运行：%w", e)
	}
	got := sha256.Sum256(script)
	if want := checksumFor(string(sums), "install.sh"); want == "" || want != hex.EncodeToString(got[:]) {
		return fmt.Errorf("install.sh 校验不过，没有运行：SHA256 是 %s，校验文件里写的是 %q", hex.EncodeToString(got[:]), want)
	}
	dir, e := os.MkdirTemp("", "agentq-update-*")
	if e != nil {
		return fmt.Errorf("建临时目录失败：%w", e)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "install.sh")
	if e = os.WriteFile(path, script, 0o600); e != nil {
		return fmt.Errorf("写安装脚本失败：%w", e)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{path}, args...)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
