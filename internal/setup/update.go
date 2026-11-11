package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"agentq/internal/fsx"
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
// repo 由发行构建写进程序；AGENTQ_RELEASE_BASE 优先，和脚本一致。
// --check 只看发布上的版本：取 VERSION 对过 SHA256SUMS 再打印，不装。--saved-net 是自动更新用的：沿用一键安装时记下的代理和镜像（UpdateNetPath）。
func RunUpdate(repo string, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("update", flag.ContinueOnError)
	f.SetOutput(stderr)
	version := f.String("version", "", "要升到的版本，默认 latest；main 走源码")
	proxy := f.String("proxy", os.Getenv("AGENTQ_PROXY"), "下载走的代理")
	mirror := f.String("mirror", os.Getenv("AGENTQ_MIRROR"), "GitHub 下载镜像前缀；校验文件不走镜像")
	check := f.Bool("check", false, "只看发布上的最新版本（对过校验），不装")
	savedNet := f.Bool("saved-net", false, "沿用一键安装时用过的 --proxy、--mirror（自动更新用）")
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
	if *savedNet {
		if home, e := os.UserHomeDir(); e == nil {
			saved, e := ReadUpdateNet(home)
			if e != nil {
				fmt.Fprintln(stderr, "agentq update：", e)
				return 1
			}
			if *proxy == "" {
				*proxy = saved.Proxy
			}
			if *mirror == "" {
				*mirror = saved.Mirror
			}
		}
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
	client, e := updateClient(*proxy)
	if e != nil {
		fmt.Fprintln(stderr, "agentq update：", e)
		return 1
	}
	// 下载有超时；安装脚本不设：安装事务第一步就卸载服务，中途被杀会留下没做完的事务、服务全停。脚本里联网的步骤自己有超时。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if *check {
		v, e := releaseVersion(ctx, client, base, *mirror)
		if e != nil {
			fmt.Fprintln(stderr, "agentq update：", e)
			return 1
		}
		fmt.Fprintln(stdout, v)
		return 0
	}
	script, e := fetchScript(ctx, client, base, *mirror)
	if e != nil {
		fmt.Fprintln(stderr, "agentq update：", e)
		return 1
	}
	var scriptArgs []string // 只把脚本认的参数交给它
	if *version != "" {
		scriptArgs = append(scriptArgs, "--version", *version)
	}
	if *proxy != "" {
		scriptArgs = append(scriptArgs, "--proxy", *proxy)
	}
	if *mirror != "" {
		scriptArgs = append(scriptArgs, "--mirror", *mirror)
	}
	if e := runScript(script, scriptArgs, stdout, stderr); e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(stderr, "agentq update：", e)
		return 1
	}
	return 0
}

func updateClient(proxy string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, e := url.Parse(proxy)
		if e != nil {
			return nil, fmt.Errorf("代理地址不对：%w", e)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: 2 * time.Minute, Transport: transport}, nil
}

// verified 取 base/<name>（github.com 上的走镜像）和 base/SHA256SUMS（不走镜像），对上了才返回内容。
func verified(ctx context.Context, client *http.Client, base, mirror, name string) ([]byte, error) {
	data, e := downloadBytes(ctx, client, viaMirror(mirror, base+"/"+name))
	if e != nil {
		return nil, e
	}
	sums, e := downloadBytes(ctx, client, base+"/SHA256SUMS")
	if e != nil {
		return nil, fmt.Errorf("拿到了 %s，但拿不到校验文件：%w", name, e)
	}
	got := sha256.Sum256(data)
	if want := checksumFor(string(sums), name); want == "" || want != hex.EncodeToString(got[:]) {
		return nil, fmt.Errorf("%s 校验不过：SHA256 是 %s，校验文件里写的是 %q", name, hex.EncodeToString(got[:]), want)
	}
	return data, nil
}

// releaseVersion 读发布里的 VERSION（scripts/build-release.sh 产出，写进了 SHA256SUMS）。
func releaseVersion(ctx context.Context, client *http.Client, base, mirror string) (string, error) {
	b, e := verified(ctx, client, base, mirror, "VERSION")
	if e != nil {
		return "", e
	}
	v := strings.TrimSpace(string(b))
	if !validVersion(v) {
		return "", fmt.Errorf("发布里的 VERSION 不是版本号：%.80q", v)
	}
	return v, nil
}

func fetchScript(ctx context.Context, client *http.Client, base, mirror string) ([]byte, error) {
	script, e := verified(ctx, client, base, mirror, "install.sh")
	if e != nil {
		return nil, fmt.Errorf("没有运行安装脚本：%w", e)
	}
	return script, nil
}

func runScript(script []byte, args []string, stdout, stderr io.Writer) error {
	dir, e := os.MkdirTemp("", "agentq-update-*")
	if e != nil {
		return fmt.Errorf("建临时目录失败：%w", e)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "install.sh")
	if e = os.WriteFile(path, script, 0o600); e != nil {
		return fmt.Errorf("写安装脚本失败：%w", e)
	}
	cmd := exec.Command("/bin/sh", append([]string{path}, args...)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// UpdateNet 是一键安装时用过的代理和镜像（~/.agentq/update-net.json，用户自己的状态）：agentq update --saved-net 沿用。
type UpdateNet struct {
	Proxy  string `json:"proxy,omitempty"`
	Mirror string `json:"mirror,omitempty"`
}

// UpdateNetPath 是记代理和镜像的文件。
func UpdateNetPath(home string) string { return filepath.Join(home, ".agentq", "update-net.json") }

// ReadUpdateNet 读记下的代理和镜像；没有文件就是都没用。
func ReadUpdateNet(home string) (UpdateNet, error) {
	var n UpdateNet
	b, e := os.ReadFile(UpdateNetPath(home))
	if errors.Is(e, os.ErrNotExist) {
		return n, nil
	}
	if e != nil {
		return n, fmt.Errorf("读 %s：%w", UpdateNetPath(home), e)
	}
	if e = json.Unmarshal(b, &n); e != nil {
		return n, fmt.Errorf("%s 格式不对：%w", UpdateNetPath(home), e)
	}
	return n, nil
}

// recordUpdateNet：一键脚本（install.sh 带 --defer-engine 调 setup）装完时记下这次用的代理和镜像，都没用就删掉旧的记录。
func recordUpdateNet(home string) error {
	n := UpdateNet{Proxy: os.Getenv("AGENTQ_PROXY"), Mirror: os.Getenv("AGENTQ_MIRROR")}
	if n == (UpdateNet{}) {
		if e := os.Remove(UpdateNetPath(home)); e != nil && !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("删 %s：%w", UpdateNetPath(home), e)
		}
		return nil
	}
	b, e := json.Marshal(n)
	if e != nil {
		return fmt.Errorf("编码代理和镜像记录：%w", e)
	}
	if e = os.MkdirAll(filepath.Dir(UpdateNetPath(home)), 0o700); e != nil {
		return fmt.Errorf("建 %s：%w", filepath.Dir(UpdateNetPath(home)), e)
	}
	if e = fsx.WriteFileAtomic(UpdateNetPath(home), append(b, '\n')); e != nil {
		return fmt.Errorf("写 %s：%w", UpdateNetPath(home), e)
	}
	return nil
}
