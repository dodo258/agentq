package tests

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type privacyFixture struct {
	repo     string
	home     string
	patterns string
}

func privacySourceRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func privacyEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "HOME=") || strings.HasPrefix(value, "XDG_CONFIG_HOME=") || strings.HasPrefix(value, "GIT_CONFIG_GLOBAL=") {
			continue
		}
		env = append(env, value)
	}
	return append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "no-global-git-config"),
	)
}

func privacyID(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name() + fmt.Sprint(time.Now().UnixNano())))
	return hex.EncodeToString(sum[:6])
}

func newPrivacyFixture(t *testing.T, literals map[string]string) privacyFixture {
	t.Helper()
	base := t.TempDir()
	fixture := privacyFixture{
		repo:     filepath.Join(base, "repo"),
		home:     filepath.Join(base, "home"),
		patterns: filepath.Join(base, "private", "privacy-patterns.json"),
	}
	for _, dir := range []string{fixture.repo, fixture.home, filepath.Dir(fixture.patterns)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writePrivacyPatterns(t, fixture.patterns, literals)
	return fixture
}

func writePrivacyPatterns(t *testing.T, path string, literals map[string]string) {
	t.Helper()
	entries := make([]map[string]any, 0, len(literals))
	for kind, literal := range literals {
		entries = append(entries, map[string]any{"type": kind, "literal": literal})
	}
	payload := map[string]any{"version": 1, "patterns": entries}
	writePrivacyJSON(t, path, payload)
}

func writePrivacyJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePrivacyFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runPrivacy(t *testing.T, dir, home string, args ...string) (int, string) {
	t.Helper()
	script := filepath.Join(privacySourceRoot(t), "scripts", "privacy_scan.py")
	command := exec.Command("python3", append([]string{"-I", script}, args...)...)
	command.Dir = dir
	command.Env = privacyEnv(home)
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(output)
	}
	t.Fatalf("运行 privacy_scan.py：%v", err)
	return -1, ""
}

func privacyScanGit(t *testing.T, fixture privacyFixture, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{
		"-C", fixture.repo,
		"-c", "user.name=privacy-test",
		"-c", "user.email=privacy" + string(rune(64)) + "invalid.example",
		"-c", "commit.gpgsign=false",
	}, args...)...)
	command.Env = privacyEnv(fixture.home)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, output)
	}
	return string(output)
}

func initPrivacyGit(t *testing.T, fixture privacyFixture) {
	t.Helper()
	privacyScanGit(t, fixture, "init", "-q", "-b", "main")
}

func requirePrivacyExit(t *testing.T, got, want int, output string) {
	t.Helper()
	if got != want {
		t.Fatalf("privacy_scan.py 退出码 %d，应为 %d：\n%s", got, want, output)
	}
}

func TestPrivacyScanDetectsValuesWithoutPrintingThem(t *testing.T) {
	id := privacyID(t)
	local := "person-" + id
	lineSeparatorPattern := "joined-" + id + string(rune(0x85)) + "value"
	fixture := newPrivacyFixture(t, map[string]string{
		"identity":       local,
		"line-separator": lineSeparatorPattern,
	})
	credential := strings.Join([]string{"sk", "ant", strings.Repeat("Q", 24) + id}, "-")
	cloudKey := strings.Join([]string{"AK", "IA"}, "") + strings.Repeat("A", 16)
	codeToken := strings.Join([]string{"gh", "p_"}, "") + strings.Repeat("b", 20)
	privateKeyHeader := strings.Join([]string{"-----BEGIN", "PRIVATE", "KEY-----"}, " ")
	assignedSecret := strings.Join([]string{"PRODUCT", "AUTH", "TOKEN"}, "_") + `=` + strings.Repeat("c", 24)
	jsonAssignment := `"` + strings.Join([]string{"DB", "PASSWORD"}, "_") + `": "` + strings.Repeat("d", 24) + `"`
	email := "owner-" + id + string(rune(64)) + "mail-" + id + ".example"
	address := strings.Join([]string{"8", "8", "4", "4"}, ".")
	homePath := filepath.Join(string(os.PathSeparator), "Users", local, "project")
	rootPath := filepath.Join(string(os.PathSeparator), "root", local, "project")
	doubleSlash := strings.Repeat(`\`, 2)
	windowsPath := "C:" + doubleSlash + "Users" + doubleSlash + local + doubleSlash + "project"
	values := []string{
		credential, cloudKey, codeToken, privateKeyHeader, assignedSecret, jsonAssignment,
		email, address, homePath, rootPath, windowsPath, local, lineSeparatorPattern,
	}
	content := strings.Join(values, "\n") + "\n"
	writePrivacyFile(t, filepath.Join(fixture.repo, "notes.txt"), []byte(content))
	writePrivacyFile(t, filepath.Join(fixture.repo, local+".txt"), []byte("ordinary\n"))

	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	for _, findingType := range []string{
		"credential-anthropic-token", "credential-aws-access-key", "credential-github-token",
		"credential-private-key", "credential-secret-assignment", "email", "public-ip",
		"absolute-home-path", "local-identity", "local-line-separator",
	} {
		if !strings.Contains(output, ":"+findingType) {
			t.Errorf("输出缺少 %s：\n%s", findingType, output)
		}
	}
	for _, secret := range values {
		if strings.Contains(output, secret) {
			t.Errorf("扫描输出泄露了命中值 %q：\n%s", secret, output)
		}
	}
	if strings.Count(output, ":credential-secret-assignment") < 2 {
		t.Errorf("未同时识别未引号赋值和 JSON 引号键：\n%s", output)
	}
	if strings.Count(output, ":absolute-home-path") < 3 {
		t.Errorf("未同时识别普通 Unix、管理员和 JSON 转义 Windows HOME：\n%s", output)
	}
	if !strings.Contains(output, "file-0001:1:local-identity") {
		t.Errorf("敏感文件名应只显示编号：\n%s", output)
	}
	lineFormat := regexp.MustCompile(`^[^:\n]+:[1-9][0-9]*:[a-z0-9-]+$`)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if !lineFormat.MatchString(line) {
			t.Errorf("扫描结果不是 path:line:type：%q", line)
		}
	}
}

func TestPrivacyScanStagedReadsIndexNotWorkingTree(t *testing.T) {
	secret := "private-" + privacyID(t)
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	initPrivacyGit(t, fixture)
	path := filepath.Join(fixture.repo, "note.txt")
	writePrivacyFile(t, path, []byte("clean\n"))
	privacyScanGit(t, fixture, "add", "note.txt")

	writePrivacyFile(t, path, []byte(secret+"\n"))
	code, output := runPrivacy(t, fixture.repo, fixture.home, "--staged", "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 0, output)
	if output != "" {
		t.Fatalf("干净索引不应有输出：%q", output)
	}

	code, output = runPrivacy(t, fixture.repo, fixture.home, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if strings.Contains(output, secret) {
		t.Fatalf("工作区扫描泄露命中值：%s", output)
	}

	privacyScanGit(t, fixture, "add", "note.txt")
	code, output = runPrivacy(t, fixture.repo, fixture.home, "--staged", "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if !strings.Contains(output, "note.txt:1:local-identity") || strings.Contains(output, secret) {
		t.Fatalf("暂存内容应被拦且不打印值：\n%s", output)
	}
}

func TestPrivacyScanIgnoresGitReplaceObjects(t *testing.T) {
	secret := "private-" + privacyID(t)
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	initPrivacyGit(t, fixture)
	writePrivacyFile(t, filepath.Join(fixture.repo, "leak.txt"), []byte(secret+"\n"))
	writePrivacyFile(t, filepath.Join(fixture.repo, "clean.txt"), []byte("clean\n"))
	privacyScanGit(t, fixture, "add", "leak.txt")
	secretOID := strings.TrimSpace(privacyScanGit(t, fixture, "rev-parse", ":leak.txt"))
	cleanOID := strings.TrimSpace(privacyScanGit(t, fixture, "hash-object", "-w", "clean.txt"))
	privacyScanGit(t, fixture, "replace", secretOID, cleanOID)
	if got := privacyScanGit(t, fixture, "cat-file", "blob", secretOID); got != "clean\n" {
		t.Fatalf("测试前置失败：git replace 未生效，得到 %q", got)
	}

	code, output := runPrivacy(t, fixture.repo, fixture.home, "--staged", "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if !strings.Contains(output, "leak.txt:1:local-identity") || strings.Contains(output, secret) {
		t.Fatalf("scanner 必须忽略 replace ref、读取索引原始 blob：\n%s", output)
	}
}

func TestPrivacyScanStagedManifestComesFromIndex(t *testing.T) {
	secret := "private-" + privacyID(t)
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	initPrivacyGit(t, fixture)
	writePrivacyFile(t, filepath.Join(fixture.repo, "leak.txt"), []byte(secret+"\n"))
	manifest := filepath.Join(fixture.repo, "config", "clean-source.json")
	writePrivacyJSON(t, manifest, map[string]any{"version": 1, "private_paths": []string{}})
	privacyScanGit(t, fixture, "add", "leak.txt", "config/clean-source.json")

	// The worktree manifest cannot hide a value that remains public in the index snapshot.
	writePrivacyJSON(t, manifest, map[string]any{"version": 1, "private_paths": []string{"leak.txt"}})
	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--staged", "--patterns", fixture.patterns, "--manifest", "config/clean-source.json")
	requirePrivacyExit(t, code, 1, output)
	if !strings.Contains(output, "leak.txt:1:local-identity") || strings.Contains(output, secret) {
		t.Fatalf("staged 必须读取索引中的 manifest 和文件：\n%s", output)
	}

	privacyScanGit(t, fixture, "add", "config/clean-source.json")
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--staged", "--patterns", fixture.patterns, "--manifest", "config/clean-source.json")
	requirePrivacyExit(t, code, 0, output)
}

func TestPrivacyRootIncludesIgnoredGeneratedFiles(t *testing.T) {
	secret := "private-" + privacyID(t)
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	initPrivacyGit(t, fixture)
	writePrivacyFile(t, filepath.Join(fixture.repo, ".gitignore"), []byte("generated/\n"))
	privacyScanGit(t, fixture, "add", ".gitignore")
	writePrivacyFile(t, filepath.Join(fixture.repo, "generated", "result.txt"), []byte(secret+"\n"))

	code, output := runPrivacy(t, fixture.repo, fixture.home, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 0, output)
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if !strings.Contains(output, "generated/result.txt:1:local-identity") || strings.Contains(output, secret) {
		t.Fatalf("--root 应扫描被忽略的生成物且不打印值：\n%s", output)
	}
}

func TestPrivacyScanArtifactsRequirePinnedHash(t *testing.T) {
	fixture := newPrivacyFixture(t, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	binary := append(bytes.Repeat([]byte{'A'}, 9000), byte(0x01))
	image := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("image-data")...)
	archive := append([]byte{'P', 'K', 0x03, 0x04}, []byte("archive-data")...)
	files := map[string][]byte{"raw.bin": binary, "image.png": image, "bundle.zip": archive}
	for name, data := range files {
		writePrivacyFile(t, filepath.Join(fixture.repo, name), data)
	}

	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	for _, findingType := range []string{"unreviewed-binary", "unreviewed-media", "unreviewed-archive"} {
		if !strings.Contains(output, ":"+findingType) {
			t.Errorf("未审批文件应报告 %s：\n%s", findingType, output)
		}
	}

	approved := make(map[string]string, len(files))
	for name, data := range files {
		sum := sha256.Sum256(data)
		approved[name] = hex.EncodeToString(sum[:])
	}
	manifest := filepath.Join(fixture.repo, "config", "clean-source.json")
	writePrivacyJSON(t, manifest, map[string]any{
		"version":        1,
		"private_paths":  []string{},
		"approved_files": approved,
	})
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", manifest)
	requirePrivacyExit(t, code, 0, output)

	writePrivacyFile(t, filepath.Join(fixture.repo, "image.png"), append(image, byte('x')))
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", manifest)
	requirePrivacyExit(t, code, 1, output)
	if !strings.Contains(output, "image.png:1:approved-file-hash-mismatch") {
		t.Fatalf("媒体变化后应被哈希门禁拦住：\n%s", output)
	}
}

func TestPrivacyScanPrivateClassificationAndNoTextApprovalBypass(t *testing.T) {
	secret := "private-" + privacyID(t)
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	privateDoc := filepath.Join(fixture.repo, "collaboration", "notes.md")
	publicDoc := filepath.Join(fixture.repo, "public.txt")
	writePrivacyFile(t, privateDoc, []byte(secret+"\n"))
	writePrivacyFile(t, publicDoc, []byte(secret+"\n"))
	publicSum := sha256.Sum256([]byte(secret + "\n"))
	manifest := filepath.Join(fixture.repo, "config", "clean-source.json")
	writePrivacyJSON(t, manifest, map[string]any{
		"version":       1,
		"private_paths": []string{"collaboration/"},
		"approved_files": map[string]string{
			"public.txt": hex.EncodeToString(publicSum[:]),
		},
	})

	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", manifest)
	requirePrivacyExit(t, code, 1, output)
	if strings.Contains(output, "collaboration") || !strings.Contains(output, "public.txt:1:local-identity") {
		t.Fatalf("只有明确私有目录可排除，文本哈希不能绕过个人规则：\n%s", output)
	}

	writePrivacyFile(t, publicDoc, []byte("clean\n"))
	writePrivacyJSON(t, manifest, map[string]any{
		"version":        1,
		"private_paths":  []string{"collaboration/"},
		"approved_files": map[string]string{},
	})
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", manifest)
	requirePrivacyExit(t, code, 0, output)
}

func TestPrivacyScanFailsClosedForInvalidConfiguration(t *testing.T) {
	fixture := newPrivacyFixture(t, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	writePrivacyFile(t, filepath.Join(fixture.repo, "safe.txt"), []byte("safe\n"))

	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", filepath.Join(filepath.Dir(fixture.patterns), "missing.json"))
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "patterns-missing") {
		t.Fatalf("缺失 patterns 应失败关闭：%s", output)
	}

	insidePatterns := filepath.Join(fixture.repo, "patterns.json")
	writePrivacyPatterns(t, insidePatterns, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", insidePatterns)
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "patterns-must-be-external") {
		t.Fatalf("仓库内 patterns 应失败关闭：%s", output)
	}

	invalidPatterns := filepath.Join(filepath.Dir(fixture.patterns), "invalid.json")
	writePrivacyFile(t, invalidPatterns, []byte("{"))
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", invalidPatterns)
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "patterns-invalid-json") {
		t.Fatalf("损坏 patterns 应失败关闭：%s", output)
	}

	missingManifest := filepath.Join(fixture.repo, "config", "missing.json")
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", missingManifest)
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "manifest-missing") {
		t.Fatalf("显式 manifest 缺失应失败关闭：%s", output)
	}

	invalidManifest := filepath.Join(fixture.repo, "config", "clean-source.json")
	writePrivacyJSON(t, invalidManifest, map[string]any{
		"version":       1,
		"private_paths": []string{"config/"},
	})
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", invalidManifest)
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "manifest-cannot-be-private") {
		t.Fatalf("manifest 不能把自身目录列为私有：%s", output)
	}

	writePrivacyJSON(t, invalidManifest, map[string]any{
		"version":  1,
		"patterns": []string{"repository-rules-must-not-replace-local-rules"},
	})
	code, output = runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns, "--manifest", invalidManifest)
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "manifest-invalid-schema") {
		t.Fatalf("仓库 manifest 不能定义个人规则：%s", output)
	}
}

func TestPrivacyScanRedactsC1AndBidiPaths(t *testing.T) {
	fixture := newPrivacyFixture(t, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	unsafeNames := []string{
		"control-" + string(rune(0x85)) + "name.txt",
		"bidi-" + string(rune(0x202e)) + "name.txt",
	}
	for _, name := range unsafeNames {
		writePrivacyFile(t, filepath.Join(fixture.repo, name), []byte("safe\n"))
	}
	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if strings.Count(output, ":unsafe-path") != len(unsafeNames) {
		t.Fatalf("C1 和 bidi 路径都应被标记：\n%s", output)
	}
	for _, name := range unsafeNames {
		if strings.Contains(output, name) {
			t.Fatalf("输出泄露不安全路径 %q：\n%s", name, output)
		}
	}
	if !strings.Contains(output, "file-0001:1:unsafe-path") || !strings.Contains(output, "file-0002:1:unsafe-path") {
		t.Fatalf("不安全路径应只显示文件编号：\n%s", output)
	}
}

func TestPrivacyScanRejectsCaseAliasPatterns(t *testing.T) {
	fixture := newPrivacyFixture(t, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	inside := filepath.Join(fixture.repo, "patterns.json")
	writePrivacyPatterns(t, inside, map[string]string{"identity": "inside-" + privacyID(t)})
	aliasRepo := filepath.Join(filepath.Dir(fixture.repo), strings.ToUpper(filepath.Base(fixture.repo)))
	aliasPatterns := filepath.Join(aliasRepo, "patterns.json")
	realInfo, realErr := os.Stat(inside)
	aliasInfo, aliasErr := os.Stat(aliasPatterns)
	if realErr != nil {
		t.Fatal(realErr)
	}
	if os.IsNotExist(aliasErr) {
		t.Skip("filesystem is case-sensitive")
	}
	if aliasErr != nil {
		t.Fatal(aliasErr)
	}
	if !os.SameFile(realInfo, aliasInfo) {
		t.Skip("case alias does not identify the same file")
	}

	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", aliasPatterns)
	requirePrivacyExit(t, code, 2, output)
	if !strings.Contains(output, "patterns-must-be-external") {
		t.Fatalf("仓库内 patterns 的大小写别名应失败关闭：%s", output)
	}
}

func TestPrivacyScanRejectsExternalSymlink(t *testing.T) {
	fixture := newPrivacyFixture(t, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	external := filepath.Join(filepath.Dir(fixture.repo), "outside.txt")
	writePrivacyFile(t, external, []byte("safe\n"))
	if err := os.Symlink(external, filepath.Join(fixture.repo, "outside-link")); err != nil {
		t.Fatal(err)
	}
	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if !strings.Contains(output, "outside-link:1:external-symlink") || strings.Contains(output, external) {
		t.Fatalf("外部 symlink 应被拦且不打印目标：\n%s", output)
	}
}

func TestPrivacyPreCommitRejectsRealCommit(t *testing.T) {
	secret := "private-" + privacyID(t)
	credential := strings.Join([]string{"sk", "ant", strings.Repeat("Q", 28)}, "-")
	personalPath := filepath.Join(string(os.PathSeparator), "Users", secret, "work")
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	defaultPatterns := filepath.Join(fixture.home, ".agentq", "private", "privacy-patterns.json")
	writePrivacyPatterns(t, defaultPatterns, map[string]string{"identity": secret})
	initPrivacyGit(t, fixture)
	privacyScanGit(t, fixture, "config", "core.hooksPath", "/dev/null")

	sourceRoot := privacySourceRoot(t)
	for _, name := range []string{"privacy_scan.py", "pre-commit"} {
		data, err := os.ReadFile(filepath.Join(sourceRoot, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(fixture.repo, "scripts", name)
		writePrivacyFile(t, destination, data)
		if err := os.Chmod(destination, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writePrivacyFile(t, filepath.Join(fixture.repo, "safe.txt"), []byte("safe\n"))
	privacyScanGit(t, fixture, "add", ".")
	privacyScanGit(t, fixture, "commit", "-q", "-m", "baseline")
	privacyScanGit(t, fixture, "config", "core.hooksPath", "scripts")

	writePrivacyFile(t, filepath.Join(fixture.repo, "leak.txt"), []byte(strings.Join([]string{secret, credential, personalPath}, "\n")+"\n"))
	privacyScanGit(t, fixture, "add", "leak.txt")
	command := exec.Command("git", "-C", fixture.repo,
		"-c", "user.name=privacy-test",
		"-c", "user.email=privacy"+string(rune(64))+"invalid.example",
		"-c", "commit.gpgsign=false",
		"commit", "-m", "must be rejected")
	command.Env = privacyEnv(fixture.home)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("真实 git commit 应被 pre-commit 拒绝：\n%s", output)
	}
	if !strings.Contains(string(output), "leak.txt:1:local-identity") || strings.Contains(string(output), secret) {
		t.Fatalf("hook 应报告位置/类型但不泄露值：\n%s", output)
	}
	if !strings.Contains(string(output), "leak.txt:2:credential-anthropic-token") || !strings.Contains(string(output), "leak.txt:3:absolute-home-path") || strings.Contains(string(output), credential) || strings.Contains(string(output), personalPath) {
		t.Fatalf("凭据形态及个人路径均须拦截且不泄值：\n%s", output)
	}

	// A worktree symlink must not redirect --staged to a different manifest blob.
	strictManifest := filepath.Join(fixture.repo, "config", "clean-source.json")
	alternateManifest := filepath.Join(fixture.repo, "alternate", "clean-source.json")
	writePrivacyJSON(t, strictManifest, map[string]any{"version": 1})
	writePrivacyJSON(t, alternateManifest, map[string]any{
		"version":       1,
		"private_paths": []string{"leak.txt"},
	})
	privacyScanGit(t, fixture, "add", "config/clean-source.json", "alternate/clean-source.json")
	if err := os.RemoveAll(filepath.Join(fixture.repo, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alternate", filepath.Join(fixture.repo, "config")); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("git", "-C", fixture.repo,
		"-c", "user.name=privacy-test",
		"-c", "user.email=privacy"+string(rune(64))+"invalid.example",
		"-c", "commit.gpgsign=false",
		"commit", "-m", "manifest redirect must be rejected")
	command.Env = privacyEnv(fixture.home)
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "leak.txt:1:local-identity") {
		t.Fatalf("worktree symlink 不能把 hook 重定向到另一个索引 manifest：\n%s", output)
	}

	if got := strings.TrimSpace(privacyScanGit(t, fixture, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("被拒提交后仍应只有基线提交，得到 %s", got)
	}
}

func TestPrivacyPreCommitHidesPythonSyntaxSource(t *testing.T) {
	fixture := newPrivacyFixture(t, map[string]string{"sentinel": "not-present-" + privacyID(t)})
	initPrivacyGit(t, fixture)
	privacyScanGit(t, fixture, "config", "core.hooksPath", "/dev/null")
	sourceRoot := privacySourceRoot(t)
	for _, name := range []string{"privacy_scan.py", "pre-commit"} {
		data, err := os.ReadFile(filepath.Join(sourceRoot, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(fixture.repo, "scripts", name)
		writePrivacyFile(t, destination, data)
		if err := os.Chmod(destination, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writePrivacyFile(t, filepath.Join(fixture.repo, "baseline.txt"), []byte("clean\n"))
	privacyScanGit(t, fixture, "add", ".")
	privacyScanGit(t, fixture, "commit", "-q", "-m", "baseline")
	privacyScanGit(t, fixture, "config", "core.hooksPath", "scripts")

	sourceSecret := "source-" + privacyID(t)
	brokenSource := "def broken(: # " + sourceSecret + "\n"
	writePrivacyFile(t, filepath.Join(fixture.repo, "scripts", "privacy_scan.py"), []byte(brokenSource))
	writePrivacyFile(t, filepath.Join(fixture.repo, "candidate.txt"), []byte("clean\n"))
	privacyScanGit(t, fixture, "add", "candidate.txt")
	command := exec.Command("git", "-C", fixture.repo,
		"-c", "user.name=privacy-test",
		"-c", "user.email=privacy"+string(rune(64))+"invalid.example",
		"-c", "commit.gpgsign=false",
		"commit", "-m", "syntax error must be sanitized")
	command.Env = privacyEnv(fixture.home)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("scanner 语法错误时 commit 应失败：\n%s", output)
	}
	text := string(output)
	if !strings.Contains(text, "privacy-scan:error:scanner-execution-failed") ||
		strings.Contains(text, sourceSecret) || strings.Contains(text, "def broken") || strings.Contains(text, "SyntaxError") {
		t.Fatalf("hook 应隐藏 Python 语法错误源码：\n%s", output)
	}
	if got := strings.TrimSpace(privacyScanGit(t, fixture, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("语法错误提交后仍应只有基线提交，得到 %s", got)
	}
}

func TestPrivacySource(t *testing.T) {
	root := privacySourceRoot(t)
	fixture := newPrivacyFixture(t, map[string]string{"source-sentinel": "not-present-" + privacyID(t)})
	args := []string{"--patterns", fixture.patterns}
	manifest := filepath.Join(root, "config", "clean-source.json")
	if _, err := os.Stat(manifest); err == nil {
		args = append(args, "--manifest", manifest)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	code, output := runPrivacy(t, root, fixture.home, args...)
	if code != 0 {
		t.Fatalf("tracked 清洁源隐私门禁未通过（退出码 %d）：\n%s", code, output)
	}
	if output != "" {
		t.Fatalf("清洁源扫描成功时不应有输出：%q", output)
	}
}

func TestPrivacyOutputNeverIncludesMatchedBytes(t *testing.T) {
	secret := "private-" + privacyID(t)
	fixture := newPrivacyFixture(t, map[string]string{"identity": secret})
	writePrivacyFile(t, filepath.Join(fixture.repo, "value.txt"), []byte(secret+"\n"))
	code, output := runPrivacy(t, fixture.repo, fixture.home,
		"--root", fixture.repo, "--patterns", fixture.patterns)
	requirePrivacyExit(t, code, 1, output)
	if bytes.Contains([]byte(output), []byte(secret)) {
		t.Fatalf("输出包含匹配字节：%s", output)
	}
}
