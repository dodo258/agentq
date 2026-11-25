// Package tell 给用户发消息：配了 Telegram 就发，没配只记日志。
package tell

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"agentq/internal/lock"
)

// Teller 发消息。File 放 TELEGRAM_BOT_TOKEN 和 TELEGRAM_CHAT_ID。
type Teller struct {
	File string
	// API 是接口地址，测试里换掉。Out 放还没发出去的。
	API, Out string
}

func (t Teller) secrets() (token, chat string) {
	b, _ := os.ReadFile(t.File)
	for _, m := range secretRe.FindAllStringSubmatch(string(b), -1) {
		if v := strings.Trim(m[2], `"' `); m[1] == "BOT_TOKEN" {
			token = v
		} else {
			chat = v
		}
	}
	return token, chat
}

var secretRe = regexp.MustCompile(`(?m)^\s*(?:export )?TELEGRAM_(BOT_TOKEN|CHAT_ID)=(.*)$`)

// Ready：token 和 chat id 都配了。
func (t Teller) Ready() bool {
	token, chat := t.secrets()
	return token != "" && chat != ""
}

// Say 先记下再发：网不通、被杀了，Flush 会补发。没配置、没内容就不发。
func (t Teller) Say(text string) error {
	if !t.Ready() || strings.TrimSpace(text) == "" {
		return nil
	}
	if u := utf16.Encode([]rune(text)); len(u) > 3000 { // Telegram 按 UTF-16 算长度，太长的不收
		text = string(utf16.Decode(u[:3000])) + "…"
	}
	file := filepath.Join(t.Out, fmt.Sprintf("%d.txt", time.Now().UnixNano()))
	if err := errors.Join(os.MkdirAll(t.Out, 0o700), os.WriteFile(file, []byte(text), 0o600)); err != nil {
		return err
	}
	return t.Flush()
}

// errRefused：Telegram 不收这条，多半是 chat id 填错了。
var errRefused = errors.New("发消息：Telegram 不收")

// Flush 把没发出去的按先后发掉。不收的留着、报出来，不挡着后面的。
func (t Teller) Flush() error {
	files, _ := filepath.Glob(filepath.Join(t.Out, "*.txt"))
	if len(files) == 0 {
		return nil
	}
	held, err := lock.Flock(filepath.Join(t.Out, "lock"), true)
	if err != nil {
		return err
	}
	defer held.Close()
	var refused error
	for _, f := range files {
		b, err := os.ReadFile(f)
		if os.IsNotExist(err) { // 别的进程刚发掉
			continue
		}
		if err == nil {
			err = t.send(string(b))
		}
		if err == nil {
			err = os.Remove(f)
		}
		if errors.Is(err, errRefused) {
			refused = err
		} else if err != nil {
			return err
		}
	}
	return refused
}

// send 发一条。报错里不带 token。
func (t Teller) send(text string) error {
	token, chat := t.secrets()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	head, rest, _ := strings.Cut(html.EscapeString(text), "\n") // 第一行加粗当标题，手机上一眼分得开
	form := url.Values{"chat_id": {chat}, "text": {strings.TrimSpace("<b>" + head + "</b>\n" + rest)}, "parse_mode": {"HTML"}, "disable_web_page_preview": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.API+"/bot"+token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("发消息：地址不对")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("发消息：%s", strings.ReplaceAll(err.Error(), token, "…"))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	why, _ := io.ReadAll(io.LimitReader(resp.Body, 500)) // 读不出原因就只报状态码
	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("%w：%s（检查 %s）", errRefused, why, t.File)
	}
	return fmt.Errorf("发消息：HTTP %d %s", resp.StatusCode, why)
}
