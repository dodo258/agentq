package tell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentq/internal/lock"
)

// Listen 收用户发来的消息，存进 dir 再交给 on，处理完由它删：重启不重收，也不会丢。只认 TELEGRAM_CHAT_ID 那个人的私聊。
// 上次没处理完的先交出去，again 为真。
func (t Teller) Listen(ctx context.Context, dir string, log func(string, ...any), on func(file, text string, again bool)) error {
	if !t.Ready() {
		return errors.New("没配 Telegram：" + t.File)
	}
	token, chat := t.secrets()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	held, err := lock.Flock(filepath.Join(dir, "lock"), false)
	if err != nil || held == nil {
		return errors.Join(err, errors.New("已经有一个在收消息了"))
	}
	defer held.Close()
	next, offset := 0, filepath.Join(dir, "offset")
	if b, err := os.ReadFile(offset); err == nil {
		next, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.txt"))
	for _, f := range left {
		if b, err := os.ReadFile(f); err == nil {
			on(f, string(b), true)
		}
	}
	for ctx.Err() == nil {
		got, err := t.poll(ctx, token, next)
		if err != nil {
			if ctx.Err() == nil {
				log("收消息：%s", strings.ReplaceAll(err.Error(), token, "…"))
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			continue
		}
		for _, u := range got {
			next = u.ID + 1
			m, file, text := u.Message, filepath.Join(dir, strconv.Itoa(u.ID)+".txt"), u.Message.Text
			mine := m.Chat.Type == "private" && strconv.FormatInt(m.Chat.ID, 10) == chat && m.From.ID == m.Chat.ID && strings.TrimSpace(text) != ""
			if mine && m.Reply != nil && m.Reply.Text != "" {
				text = "（用户在回这条消息：" + m.Reply.Text + "）\n" + text
			}
			if mine { // 先存消息再记收到哪：记不下就停，不然会丢或者做两遍
				if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
					return err
				}
			}
			if err := os.WriteFile(offset, []byte(strconv.Itoa(next)), 0o600); err != nil {
				return err
			}
			if mine {
				on(file, text, false)
			}
		}
	}
	return ctx.Err()
}

type update struct {
	ID      int `json:"update_id"`
	Message struct {
		Text string `json:"text"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Reply *struct {
			Text string `json:"text"`
		} `json:"reply_to_message"`
	} `json:"message"`
}

func (t Teller) poll(ctx context.Context, token string, next int) ([]update, error) {
	ctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	q := url.Values{"timeout": {"50"}, "offset": {strconv.Itoa(next)}, "allowed_updates": {`["message"]`}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.API+"/bot"+token+"/getUpdates?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		OK     bool     `json:"ok"`
		Result []update `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.OK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body.Result, nil
}
