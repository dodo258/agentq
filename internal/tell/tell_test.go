package tell

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if len(got) != 2 || !strings.HasPrefix(got[0], "<b>短的🟢</b>\n<i>") {
		t.Fatalf("%d 条 %.50q", len(got), got)
	}
	if n := len(utf16.Encode([]rune(got[1]))); n > 4096 || n < 2900 {
		t.Errorf("长 %d", n)
	}
}

// 排成旧版那样：标题加粗；每段段名加粗，项目名只加粗名字；内容进竖条块；末尾小字时间；用户写的尖括号不会变成标签。
func TestRender(t *testing.T) {
	got := Render("🟢 做好了 · 11月26日\n\n记账本　统计页 <x>\n怎么看：打开看板\n用了 41m\n\n⚠️ 留意\n以后会慢", time.Date(2026, 11, 26, 5, 51, 0, 0, time.Local))
	want := "<b>🟢 做好了 · 11月26日</b>\n\n<b>记账本</b>　统计页 &lt;x&gt;\n<blockquote>怎么看：打开看板\n用了 41m</blockquote>\n\n<b>⚠️ 留意</b>\n<blockquote>以后会慢</blockquote>\n<i>05:51 · agentq</i>"
	if got != want {
		t.Errorf("%s", got)
	}
}
