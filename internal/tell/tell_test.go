package tell

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// 太长的截短、空的不发：Telegram 都不收，留着会一直补发。
func TestSayFits(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		q.ParseForm()
		got = append(got, q.Form.Get("text"))
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	secret := filepath.Join(dir, "tg")
	if err := os.WriteFile(secret, []byte("TELEGRAM_BOT_TOKEN=tok\nTELEGRAM_CHAT_ID=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	say := Teller{File: secret, API: srv.URL, Out: filepath.Join(dir, "outbox")}.Say
	long := ""
	for range 3400 { // 一个表情算两个字
		long += "🟢"
	}
	for _, text := range []string{"", " \n", "短的🟢", long} {
		if err := say(text); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0] != "<b>短的🟢</b>" {
		t.Fatalf("%d 条 %.50q", len(got), got)
	}
	if n := len(utf16.Encode([]rune(got[1]))); n > 4096 || n < 2900 {
		t.Errorf("长 %d", n)
	}
}
