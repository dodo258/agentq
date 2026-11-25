package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

const fakeVersion = "v0.0.0-test"

const oldHelp = "agentq：说一句需求，做到合进主分支"

// 假程序：只会 version 和 --help，用来代替真的 agentq 打包。
const fakeMain = `package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	fmt.Println("agentq：说一句需求，做到合进主分支")
}
`

var (
	relOnce sync.Once
	relDir  string
	relErr  error
	scratch string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if scratch != "" {
		os.RemoveAll(scratch)
	}
	os.Exit(code)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// release 在一个假项目里跑真的 scripts/release.sh，整个测试进程只打一次包。
func release(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("只在 macOS 上测")
	}
	relOnce.Do(func() {
		var err error
		scratch, err = os.MkdirTemp("", "agentq-release-test-")
		if err != nil {
			relErr = err
			return
		}
		proj := filepath.Join(scratch, "proj")
		for _, f := range [][2]string{{"../scripts/release.sh", "scripts/release.sh"}, {"../install.sh", "install.sh"}, {"../.agentq/verify", ".agentq/verify"}} {
			b, err := os.ReadFile(f[0])
			if err != nil {
				relErr = err
				return
			}
			dst := filepath.Join(proj, f[1])
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := os.WriteFile(dst, b, 0o755); err != nil {
				relErr = err
				return
			}
		}
		os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module fake\n\ngo 1.27.4\n"), 0o644)
		os.WriteFile(filepath.Join(proj, "main.go"), []byte(fakeMain), 0o644)
		relDir = filepath.Join(scratch, "out")
		cmd := exec.Command("sh", filepath.Join(proj, "scripts/release.sh"), fakeVersion, relDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			relErr = fmt.Errorf("release.sh: %v\n%s", err, out)
		}
	})
	if relErr != nil {
		t.Fatal(relErr)
	}
	return relDir
}

// copyRelease 复制一份假 Release，给要改坏它的测试用。
func copyRelease(t *testing.T) string {
	t.Helper()
	src := release(t)
	dst := t.TempDir()
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		write(t, filepath.Join(dst, e.Name()), mustRead(t, filepath.Join(src, e.Name())), 0o644)
	}
	return dst
}

func hostArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

type result struct {
	out  string
	code int
}

// install 像 `curl … | sh` 那样把脚本从标准输入喂给 /bin/sh，HOME 和 TMPDIR 都是临时的。
func install(t *testing.T, script []byte, home, tmp, base string, extra ...string) result {
	t.Helper()
	cmd := exec.Command("/bin/sh")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = append([]string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "AGENTQ_RELEASE_BASE=" + base}, extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return result{string(out), code}
}

func installScript(t *testing.T) []byte { return mustRead(t, "../install.sh") }

// snapshot 记下目录里每个文件的类型、权限、内容或软链目标，用来证明「没改任何东西」。
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			dst, _ := os.Readlink(p)
			lines = append(lines, fmt.Sprintf("%s link %s", rel, dst))
		case info.Mode().IsRegular():
			sum := sha256.Sum256(mustRead(t, p))
			lines = append(lines, fmt.Sprintf("%s file %v %x", rel, info.Mode(), sum[:6]))
		default:
			lines = append(lines, fmt.Sprintf("%s %v", rel, info.Mode()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func fileURL(dir string) string { return "file://" + dir }

func run(t *testing.T, name string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReleaseArtifacts(t *testing.T) {
	dir := release(t)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"SHA256SUMS", "agentq_darwin_amd64.tar.gz", "agentq_darwin_arm64.tar.gz", "install.sh"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("产出的文件 = %v，应为 %v（不能有 VERSION）", names, want)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dir, "install.sh")), installScript(t)) {
		t.Error("install.sh 要原样拷")
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(mustRead(t, filepath.Join(dir, "SHA256SUMS")))), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("SHA256SUMS 行不对：%q", line)
		}
		sums[f[1]] = f[0]
	}
	for _, n := range []string{"agentq_darwin_amd64.tar.gz", "agentq_darwin_arm64.tar.gz", "install.sh"} {
		sum := sha256.Sum256(mustRead(t, filepath.Join(dir, n)))
		if sums[n] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s 的校验值对不上：%s", n, sums[n])
		}
	}
	for _, arch := range []string{"arm64", "amd64"} {
		zr, err := gzip.NewReader(bytes.NewReader(mustRead(t, filepath.Join(dir, "agentq_darwin_"+arch+".tar.gz"))))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(zr)
		var members []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			members = append(members, h.Name)
			if h.Mode&0o111 == 0 {
				t.Errorf("%s 里的 agentq 不可执行", arch)
			}
		}
		if len(members) != 1 || strings.TrimPrefix(members[0], "./") != "agentq" {
			t.Errorf("%s 包里应只有 agentq：%v", arch, members)
		}
	}
}

func TestReleaseRejectsBadArgs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("只在 macOS 上测")
	}
	for _, args := range [][]string{{}, {"v1.2.3"}, {"1.2.3", "x"}, {"v1.2", "x"}, {"v1.2.3;rm", "x"}, {"v1.2.3\nv2.0.0", "x"}, {"v1.2.3-", "x"}} {
		out := filepath.Join(t.TempDir(), "out")
		if len(args) == 2 {
			args[1] = out
		}
		cmd := exec.Command("sh", append([]string{"../scripts/release.sh"}, args...)...)
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 2 {
			t.Errorf("参数 %q 应退出 2，实得 %v", args, err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("参数 %q 不该建输出目录", args)
		}
	}
}

func TestInstallFresh(t *testing.T) {
	rel := release(t)
	home, tmp := t.TempDir(), t.TempDir()
	r := install(t, installScript(t), home, tmp, fileURL(rel))
	if r.code != 0 {
		t.Fatalf("退出 %d：\n%s", r.code, r.out)
	}
	bin := filepath.Join(home, ".local/bin")
	ents, _ := os.ReadDir(bin)
	if len(ents) != 1 || ents[0].Name() != "agentq" {
		t.Fatalf("~/.local/bin 里应只有 agentq：%v", ents)
	}
	info, _ := os.Lstat(filepath.Join(bin, "agentq"))
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Errorf("agentq 应是 755 的普通文件：%v", info.Mode())
	}
	if got := run(t, filepath.Join(bin, "agentq"), []string{}, "version"); got != fakeVersion {
		t.Errorf("agentq version = %q", got)
	}
	for _, s := range []string{fakeVersion, "agentq install", `export PATH="$HOME/.local/bin:$PATH"`} {
		if !strings.Contains(r.out, s) {
			t.Errorf("输出里应有 %q：\n%s", s, r.out)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("临时目录没清干净：%v", left)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 1 {
		t.Errorf("HOME 里除 .local 不该有别的：%v", ents)
	}
	// 再装一次（这回 agentq 已经是这一版）：照样成功。
	if r := install(t, installScript(t), home, tmp, fileURL(rel), "PATH=/usr/bin:/bin:"+bin); r.code != 0 || strings.Contains(r.out, "export PATH") {
		t.Errorf("PATH 里已有时不该再提示；退出 %d：\n%s", r.code, r.out)
	}
}

func TestInstallUpgradeReplaces(t *testing.T) {
	rel := release(t)
	home, tmp := t.TempDir(), t.TempDir()
	plist := filepath.Join(home, "Library/LaunchAgents/cc.agentq.resume.plist")
	write(t, plist, []byte("keep"), 0o644)
	old := filepath.Join(home, ".local/bin/agentq")
	write(t, old, []byte("#!/bin/sh\necho '"+oldHelp+"'\n"), 0o755)
	r := install(t, installScript(t), home, tmp, fileURL(rel))
	if r.code != 0 {
		t.Fatalf("退出 %d：\n%s", r.code, r.out)
	}
	if got := run(t, old, []string{}, "version"); got != fakeVersion {
		t.Errorf("没换上新程序：%q", got)
	}
	if !strings.Contains(r.out, "再跑一次 agentq install") {
		t.Errorf("升级时应提示再跑 agentq install：\n%s", r.out)
	}
	if string(mustRead(t, plist)) != "keep" {
		t.Error("服务文件被改了")
	}
}

func TestInstallRejectsBrokenDownload(t *testing.T) {
	good := release(t)
	tgz := "agentq_darwin_" + hostArch() + ".tar.gz"
	cases := map[string]func(dir string){
		"改坏一个字节": func(dir string) {
			p := filepath.Join(dir, tgz)
			b := mustRead(t, p)
			b[len(b)/2] ^= 1
			write(t, p, b, 0o644)
		},
		"校验值被改": func(dir string) {
			p := filepath.Join(dir, "SHA256SUMS")
			s := string(mustRead(t, p))
			c := "0"
			if s[0] == '0' {
				c = "1"
			}
			s = c + s[1:]
			write(t, p, []byte(s), 0o644)
		},
		"缺这个包的条目": func(dir string) {
			p := filepath.Join(dir, "SHA256SUMS")
			var keep []string
			for _, l := range strings.Split(string(mustRead(t, p)), "\n") {
				if !strings.Contains(l, tgz) {
					keep = append(keep, l)
				}
			}
			write(t, p, []byte(strings.Join(keep, "\n")), 0o644)
		},
		"没有 SHA256SUMS": func(dir string) { os.Remove(filepath.Join(dir, "SHA256SUMS")) },
		"没有包":           func(dir string) { os.Remove(filepath.Join(dir, tgz)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, n := range []string{tgz, "SHA256SUMS"} {
				write(t, filepath.Join(dir, n), mustRead(t, filepath.Join(good, n)), 0o644)
			}
			mutate(dir)
			home, tmp := t.TempDir(), t.TempDir()
			before := snapshot(t, home)
			r := install(t, installScript(t), home, tmp, fileURL(dir))
			if r.code == 0 {
				t.Fatalf("应该拒绝：\n%s", r.out)
			}
			if after := snapshot(t, home); after != before {
				t.Errorf("HOME 被改了：\n%s", after)
			}
			if left, _ := os.ReadDir(tmp); len(left) != 0 {
				t.Errorf("临时目录没清干净：%v", left)
			}
		})
	}
	t.Run("校验值对不上时报出两个值", func(t *testing.T) {
		dir := copyRelease(t)
		p := filepath.Join(dir, "agentq_darwin_"+hostArch()+".tar.gz")
		b := mustRead(t, p)
		b[len(b)/2] ^= 1
		write(t, p, b, 0o644)
		r := install(t, installScript(t), t.TempDir(), t.TempDir(), fileURL(dir))
		sum := sha256.Sum256(b)
		if r.code == 0 || !strings.Contains(r.out, hex.EncodeToString(sum[:])) || !strings.Contains(r.out, "应为") {
			t.Errorf("应报出应有值和实得值；退出 %d：\n%s", r.code, r.out)
		}
	})
}

func TestInstallLeavesBinUnchangedOnFailure(t *testing.T) {
	dir := copyRelease(t)
	p := filepath.Join(dir, "agentq_darwin_"+hostArch()+".tar.gz")
	b := mustRead(t, p)
	b[10] ^= 1
	write(t, p, b, 0o644)
	home := t.TempDir()
	write(t, filepath.Join(home, ".local/bin/agentq"), []byte("#!/bin/sh\necho '"+oldHelp+"'\n"), 0o755)
	before := snapshot(t, home)
	if r := install(t, installScript(t), home, t.TempDir(), fileURL(dir)); r.code == 0 {
		t.Fatalf("应该拒绝：\n%s", r.out)
	}
	if after := snapshot(t, home); after != before {
		t.Errorf("~/.local/bin 变了：\n%s", after)
	}
}

// 下载到的是旧版（有版本号，但不是这一版）：不换掉新版，也不装。
func TestInstallRejectsOldDownload(t *testing.T) {
	dir := copyRelease(t)
	tgz := filepath.Join(dir, "agentq_darwin_"+hostArch()+".tar.gz")
	stage := t.TempDir()
	write(t, filepath.Join(stage, "agentq"), []byte("#!/bin/sh\n[ \"$1\" = version ] && echo 'agentq v0.3.0' || echo 'agentq：后台调度器，把各项目 docs/BACKLOG.md「待做」交给 claude -p 做到合并'\n"), 0o755)
	run(t, "/usr/bin/tar", []string{"COPYFILE_DISABLE=1"}, "-czf", tgz, "-C", stage, "agentq")
	sum := sha256.Sum256(mustRead(t, tgz))
	write(t, filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+filepath.Base(tgz)+"\n"), 0o644)
	for _, have := range []bool{true, false} {
		home, tmp := t.TempDir(), t.TempDir()
		if have {
			write(t, filepath.Join(home, ".local/bin/agentq"), []byte("#!/bin/sh\necho '"+oldHelp+"'\n"), 0o755)
			write(t, filepath.Join(home, "Library/LaunchAgents/cc.agentq.resume.plist"), []byte("keep"), 0o644)
		}
		before := snapshot(t, home)
		if r := install(t, installScript(t), home, tmp, fileURL(dir)); r.code == 0 || !strings.Contains(r.out, "不是这一版") || !strings.Contains(r.out, "agentq v0.3.0") {
			t.Errorf("应该拒绝并说清；退出 %d：\n%s", r.code, r.out)
		}
		if after := snapshot(t, home); after != before {
			t.Errorf("HOME 被改了：\n%s\n--- 原来\n%s", after, before)
		}
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Errorf("临时目录没清干净：%v", left)
		}
	}
}

func TestInstallRejectsOldVersion(t *testing.T) {
	rel := release(t)
	cases := map[string]func(home string){
		"软链": func(home string) {
			write(t, filepath.Join(home, ".local/share/agentq/generations/1/agentq"), []byte("old"), 0o755)
			os.MkdirAll(filepath.Join(home, ".local/bin"), 0o755)
			os.Symlink(filepath.Join(home, ".local/share/agentq/generations/1/agentq"), filepath.Join(home, ".local/bin/agentq"))
		},
		"旧服务文件": func(home string) {
			write(t, filepath.Join(home, "Library/LaunchAgents/io.agentq.agentq.plist"), []byte("old"), 0o644)
		},
		"别的 agentq": func(home string) {
			write(t, filepath.Join(home, ".local/bin/agentq"), []byte("#!/bin/sh\necho something else\n"), 0o755)
		},
		"跑不起来的 agentq": func(home string) {
			write(t, filepath.Join(home, ".local/bin/agentq"), []byte("garbage"), 0o755)
		},
		"目录": func(home string) {
			os.MkdirAll(filepath.Join(home, ".local/bin/agentq"), 0o755)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			home, tmp := t.TempDir(), t.TempDir()
			setup(home)
			before := snapshot(t, home)
			// 地址指向不存在的地方：证明拒绝发生在下载之前。
			r := install(t, installScript(t), home, tmp, fileURL(filepath.Join(rel, "nope")))
			if r.code == 0 {
				t.Fatalf("应该拒绝：\n%s", r.out)
			}
			if strings.Contains(r.out, "下载失败") {
				t.Errorf("应在下载之前拒绝：\n%s", r.out)
			}
			want := "agentq remove --yes"
			if name == "别的 agentq" || name == "跑不起来的 agentq" || name == "目录" {
				want = "不是这一版"
			}
			if !strings.Contains(r.out, want) {
				t.Errorf("输出里应有 %q：\n%s", want, r.out)
			}
			if after := snapshot(t, home); after != before {
				t.Errorf("HOME 被改了：\n%s\n--- 原来\n%s", after, before)
			}
		})
	}
}

func TestInstallTruncatedScriptDoesNothing(t *testing.T) {
	rel := release(t)
	full := installScript(t)
	lastLine := bytes.LastIndex(bytes.TrimRight(full, "\n"), []byte("\n")) + 1
	if !bytes.HasPrefix(full[lastLine:], []byte("main ")) {
		t.Fatalf("install.sh 最后一行应是调用 main：%q", full[lastLine:])
	}
	for cut := 1; cut <= lastLine; cut += 97 {
		home, tmp := t.TempDir(), t.TempDir()
		r := install(t, full[:cut], home, tmp, fileURL(rel))
		if ents, _ := os.ReadDir(home); len(ents) != 0 {
			t.Fatalf("截到 %d 字节时 HOME 里多了东西：%v\n%s", cut, ents, r.out)
		}
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Fatalf("截到 %d 字节时留了临时文件：%v", cut, left)
		}
	}
	// 截到正好是最后一行之前：函数都定义了，但不调用，什么都不做。
	home := t.TempDir()
	if r := install(t, full[:lastLine], home, t.TempDir(), fileURL(rel)); r.code != 0 {
		t.Errorf("只定义不调用应退出 0：%d\n%s", r.code, r.out)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("HOME 里多了东西：%v", ents)
	}
}

func TestInstallRejectsBadVersionEnv(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("只在 macOS 上测")
	}
	for _, v := range []string{"1.2.3", "v1;x", "latest"} {
		home := t.TempDir()
		cmd := exec.Command("/bin/sh")
		cmd.Stdin = bytes.NewReader(installScript(t))
		cmd.Env = []string{"HOME=" + home, "TMPDIR=" + t.TempDir(), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "AGENTQ_VERSION=" + v}
		if err := cmd.Run(); err == nil {
			t.Errorf("AGENTQ_VERSION=%q 应拒绝", v)
		}
		if ents, _ := os.ReadDir(home); len(ents) != 0 {
			t.Errorf("HOME 里多了东西：%v", ents)
		}
	}
}

func TestReleaseFiles(t *testing.T) {
	const checkout = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1"
	const setupGo = "actions/setup-go@d8cbb8045d7f43706b9177cceb36c1f91a6644d5 # v7.1.0"
	ci := string(mustRead(t, "../.github/workflows/ci.yml"))
	rel := string(mustRead(t, "../.github/workflows/release.yml"))
	for name, y := range map[string]string{"ci.yml": ci, "release.yml": rel} {
		for _, s := range []string{checkout, setupGo, "go-version-file: go.mod", "cache: false", ".agentq/verify", "runs-on: macos-latest", "timeout-minutes: 20"} {
			if !strings.Contains(y, s) {
				t.Errorf("%s 里缺 %q", name, s)
			}
		}
		if n := strings.Count(y, "uses:"); n != strings.Count(y, "@3d3c42e5aac5ba805825da76410c181273ba90b1")+strings.Count(y, "@d8cbb8045d7f43706b9177cceb36c1f91a6644d5") {
			t.Errorf("%s 里有没按提交号固定的 action", name)
		}
		if !strings.Contains(y, "permissions:\n  contents: read\n") {
			t.Errorf("%s 顶层应只给 contents: read", name)
		}
	}
	if strings.Contains(ci, "contents: write") {
		t.Error("ci.yml 不该有写权限")
	}
	if strings.Count(rel, "contents: write") != 1 {
		t.Error("release.yml 只有发布 job 有 contents: write")
	}
	if i, j := strings.Index(rel, "contents: write"), strings.Index(rel, "publish:"); j < 0 || i < j {
		t.Error("contents: write 应在 publish job 里")
	}
	for _, s := range []string{"tags: ['v*']", "needs: verify", `scripts/release.sh "$GITHUB_REF_NAME" dist`, "--verify-tag", "--generate-notes"} {
		if !strings.Contains(rel, s) {
			t.Errorf("release.yml 里缺 %q", s)
		}
	}
	if !strings.Contains(ci, "branches: [main]") || !strings.Contains(ci, "pull_request:") {
		t.Error("ci.yml 应在 push main 和 pull_request 时跑")
	}
	if !strings.HasPrefix(string(mustRead(t, "../LICENSE")), "MIT License\n\nCopyright (c) 2026 agentq contributors") {
		t.Error("LICENSE 应是 MIT")
	}
	if !strings.Contains(string(mustRead(t, "../.gitattributes")), "docs/specs/ export-ignore") {
		t.Error(".gitattributes 应挡住 docs/specs")
	}
	if !strings.Contains(string(mustRead(t, "../.gitignore")), "dist/") {
		t.Error(".gitignore 应有 dist/")
	}
}
