package setup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// agentq update --check：VERSION 对过 SHA256SUMS 才打印；对不上、拿不到都退出非 0，不装。--saved-net 沿用记下的代理（代理地址不对时报错就说明读到了）。
func TestUpdateCheck(t *testing.T) {
	files := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTQ_RELEASE_BASE", srv.URL)
	t.Setenv("AGENTQ_PROXY", "")
	t.Setenv("AGENTQ_MIRROR", "")
	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := RunUpdate("", args, &out, &errOut)
		return out.String(), errOut.String(), code
	}

	ver := []byte("v2.0.0\n")
	files["/VERSION"] = ver
	files["/SHA256SUMS"] = []byte(sumOf(ver) + "  VERSION\n")
	if out, errOut, code := run("--check"); code != 0 || out != "v2.0.0\n" {
		t.Fatalf("要打印版本：%d %q %s", code, out, errOut)
	}
	files["/SHA256SUMS"] = []byte(strings.Repeat("0", 64) + "  VERSION\n")
	if out, errOut, code := run("--check"); code == 0 || out != "" || !strings.Contains(errOut, "校验不过") {
		t.Fatalf("校验不过要拒绝：%d %q %s", code, out, errOut)
	}
	delete(files, "/VERSION")
	if _, errOut, code := run("--check"); code == 0 || !strings.Contains(errOut, "404") {
		t.Fatalf("拿不到要报错：%d %s", code, errOut)
	}

	// 一键安装记下的代理：--saved-net 才用
	t.Setenv("AGENTQ_PROXY", "http://bad host")
	if err := recordUpdateNet(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTQ_PROXY", "")
	files["/VERSION"] = ver
	files["/SHA256SUMS"] = []byte(sumOf(ver) + "  VERSION\n")
	if _, errOut, code := run("--check"); code != 0 {
		t.Fatalf("不带 --saved-net 不用记下的代理：%s", errOut)
	}
	if _, errOut, code := run("--check", "--saved-net"); code == 0 || !strings.Contains(errOut, "代理地址不对") {
		t.Fatalf("--saved-net 要用记下的代理：%d %s", code, errOut)
	}
	// 再装一次没用代理和镜像：记录删掉
	if err := recordUpdateNet(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(UpdateNetPath(home)); !os.IsNotExist(err) {
		t.Fatalf("没用代理和镜像要删掉记录：%v", err)
	}
}
