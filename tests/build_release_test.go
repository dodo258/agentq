package tests

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agentq/internal/testkit"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// 发行构建：四个 tar.gz 和 SHA256SUMS，包里只有 agentq，本平台的程序带版本号；CGO 关闭后仍能编译。
func TestBuildReleaseScript(t *testing.T) {
	out := filepath.Join(t.TempDir(), "dist with space")
	cmd := exec.Command("/bin/sh", filepath.Join("scripts", "build-release.sh"), "v9.8.7-test", out, "some-owner/agentq")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(testkit.ReadBytes(t, filepath.Join(out, "SHA256SUMS")))), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("校验文件格式不对：%q", line)
		}
		sums[f[1]] = f[0]
	}
	for _, target := range []string{"darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64"} {
		name := "agentq_" + target + ".tar.gz"
		data := testkit.ReadBytes(t, filepath.Join(out, name))
		if sums[name] != sha256Hex(data) {
			t.Fatalf("%s 校验和不对：%q", name, sums[name])
		}
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		h, err := tr.Next()
		if err != nil || h.Name != "agentq" || h.Mode&0o111 == 0 {
			t.Fatalf("%s：%+v %v", name, h, err)
		}
		bin, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Next(); err != io.EOF {
			t.Fatalf("%s 里不该有别的文件：%v", name, err)
		}
		if target == runtime.GOOS+"_"+runtime.GOARCH {
			p := filepath.Join(t.TempDir(), "agentq")
			testkit.WriteInstall(t, p, string(bin), 0o755)
			if v, err := exec.Command(p, "version").CombinedOutput(); err != nil || strings.TrimSpace(string(v)) != "agentq v9.8.7-test" {
				t.Fatalf("版本号没写进去：%v %s", err, v)
			}
			// 仓库名编进了程序：agentq update 知道去哪取。代理指向没人听的端口，不会真的联网。
			up := exec.Command(p, "update")
			up.Env = []string{"HOME=" + t.TempDir(), "HTTPS_PROXY=http://127.0.0.1:1"}
			if b, err := up.CombinedOutput(); err == nil || !strings.Contains(string(b), "https://github.com/some-owner/agentq/releases/latest/download/install.sh") {
				t.Fatalf("仓库名没编进程序：%v %s", err, b)
			}
		}
	}
	if len(sums) != 6 {
		t.Fatalf("校验文件应有 6 条：%v", sums)
	}
	// VERSION：自动更新据它判断有没有新版本，也写进了校验文件
	if ver := testkit.ReadBytes(t, filepath.Join(out, "VERSION")); string(ver) != "v9.8.7-test\n" || sums["VERSION"] != sha256Hex(ver) {
		t.Fatalf("VERSION 不对：%q %q", ver, sums["VERSION"])
	}
	script := testkit.ReadBytes(t, filepath.Join(out, "install.sh"))
	if sums["install.sh"] != sha256Hex(script) {
		t.Fatalf("install.sh 校验和不对：%q", sums["install.sh"])
	}
	if !strings.Contains(string(script), "\tgithub_repo=some-owner/agentq\n") || strings.Contains(string(script), "github_repo=__") {
		t.Fatal("仓库名没填进副本")
	}
	if strings.Contains(string(testkit.ReadBytes(t, "install.sh")), "some-owner") {
		t.Fatal("不该改仓库里的原文件")
	}
	// 不给 owner/repo：不产出 install.sh，上次留下的也不留
	cmd = exec.Command("/bin/sh", filepath.Join("scripts", "build-release.sh"), "v9.8.7-test", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if exists(filepath.Join(out, "install.sh")) || strings.Contains(string(testkit.ReadBytes(t, filepath.Join(out, "SHA256SUMS"))), "install.sh") {
		t.Fatal("没给 owner/repo 不该有 install.sh")
	}
	if !strings.Contains(string(testkit.ReadBytes(t, filepath.Join(out, "SHA256SUMS"))), "  VERSION\n") {
		t.Fatal("没给 owner/repo 也要有 VERSION")
	}
	for _, bad := range [][]string{{"v1"}, {"bad version", out}, {"--help", out}, {"v1", out, "noslash"}, {"v1", out, "a/b/c"}, {"v1", out, "a/b;c"}, {"v1", out, "-x/y"}, {"v1", out, ""}, {"v1", out, "a/b", "extra"}} {
		cmd := exec.Command("/bin/sh", append([]string{filepath.Join("scripts", "build-release.sh")}, bad...)...)
		if err := cmd.Run(); err == nil {
			t.Fatalf("%v 应拒绝", bad)
		}
	}
}
