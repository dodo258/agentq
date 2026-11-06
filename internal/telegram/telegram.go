// Package telegram 是 Telegram Bot API 客户端：收发消息、附件。
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentq/internal/env"
	"agentq/internal/textfmt"
)

// Service 是 Telegram 客户端的模块实例。TgDownAt 是进程内缓存，所以每个进程只有一份。
type Service struct {
	*env.Env
	TelegramAPI string
	// Compress 为空时用 ffmpeg（没有就用系统自带的 avconvert）压缩太大的录屏；测试里替换掉。
	Compress func(src, dir string) (string, error)
	// tgDownAt：Telegram 最近一次调用超时的时间（见 tgDownFor）。
	TgDownAt time.Time
}

func HasAttachment(m *TgMsg) bool {
	return len(m.Photo) > 0 || m.Video != nil || m.Document != nil || m.Animation != nil || m.VideoNote != nil || m.Voice != nil || m.Audio != nil
}

const compressTimeout = 10 * time.Minute

// mediaKind 按扩展名分：photo、video 或 document。
func mediaKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return "photo"
	case ".mp4", ".mov", ".m4v":
		return "video"
	}
	return "document"
}

// PrepareMedia 检查要发的文件：能直接发返回原路径；录屏超过 50MB 返回压缩后的文件（存到 ~/.agentq/notify/media/）；
// 实在发不了返回 note（一句带文件路径的说明，调用方改发这句话）。文件不在、是目录时返回错误。
func (a *Service) PrepareMedia(path string) (send, note string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("文件不在：%s", path)
	}
	if fi.IsDir() {
		return "", "", fmt.Errorf("%s 是目录，不是文件", path)
	}
	if fi.Size() <= tgUploadMax {
		return path, "", nil
	}
	shown := a.Tilde(path)
	if mediaKind(path) != "video" {
		return "", fmt.Sprintf("文件 %s 有 %s，超过 Telegram 给机器人的 50 MB 上限，没发；文件在 %s", filepath.Base(path), textfmt.HumanSize(fi.Size()), shown), nil
	}
	compress := a.Compress
	if compress == nil {
		compress = compressVideo
	}
	dir := filepath.Join(a.DataDir(), "notify", "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	out, cerr := compress(path, dir)
	if cerr == nil {
		if ofi, err := os.Stat(out); err == nil && ofi.Size() <= tgUploadMax {
			a.Logf("推送：录屏 %s 有 %s，压缩到 %s 再发（%s）", shown, textfmt.HumanSize(fi.Size()), textfmt.HumanSize(ofi.Size()), a.Tilde(out))
			return out, "", nil
		} else if err == nil {
			cerr = fmt.Errorf("压到 %s 还是超过 50 MB", textfmt.HumanSize(ofi.Size()))
			os.Remove(out) // 发不了，不留着占地方
		} else {
			cerr = err
		}
	}
	return "", fmt.Sprintf("录屏 %s 有 %s，超过 Telegram 给机器人的 50 MB 上限，%s，没发；文件在 %s",
		filepath.Base(path), textfmt.HumanSize(fi.Size()), textfmt.OneLine(cerr.Error(), 120), shown), nil
}

// compressVideo 把录屏压小一些，返回压得最小的那个结果：有 ffmpeg 用 H.264 先 crf 28 再 crf 32（宽最多 1280），
// 没有就用 /usr/bin/avconvert 依次试 1280x720、960x540、640x480，每次压完不超过 50MB 就停。
func compressVideo(src, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compressTimeout)
	defer cancel()
	base := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
	type try struct {
		out  string
		name string
		args []string
	}
	var tries []try
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		for _, crf := range []string{"28", "32"} {
			out := base + "-crf" + crf + ".mp4"
			tries = append(tries, try{out, ff, []string{"-y", "-loglevel", "error", "-i", src, "-vf", "scale='min(1280,iw)':-2",
				"-c:v", "libx264", "-crf", crf, "-preset", "veryfast", "-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", out}})
		}
	} else if _, err := os.Stat("/usr/bin/avconvert"); err == nil {
		for _, p := range []string{"Preset1280x720", "Preset960x540", "Preset640x480"} {
			out := base + "-" + strings.TrimPrefix(p, "Preset") + ".mp4"
			tries = append(tries, try{out, "/usr/bin/avconvert", []string{"--source", src, "--preset", p, "--output", out, "--replace"}})
		}
	} else {
		return "", errors.New("这台电脑上没有 ffmpeg 也没有 avconvert，压不了")
	}
	best, bestSize := "", int64(-1)
	var lastErr error
	for _, t := range tries {
		if out, err := exec.CommandContext(ctx, t.name, t.args...).CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("%s 压缩失败：%v %s", filepath.Base(t.name), err, textfmt.OneLine(string(out), 120))
			os.Remove(t.out)
			continue
		}
		fi, err := os.Stat(t.out)
		if err != nil {
			lastErr = err
			continue
		}
		if bestSize < 0 || fi.Size() < bestSize {
			if best != "" {
				os.Remove(best)
			}
			best, bestSize = t.out, fi.Size()
		} else {
			os.Remove(t.out)
		}
		if bestSize <= tgUploadMax {
			break
		}
	}
	if best == "" {
		return "", lastErr
	}
	return best, nil
}

// TgSendFile 把一个本地文件发给用户，caption 是一句纯文本说明（可空），replyTo > 0 时挂在那条消息下面。
// 太大发不了时改发一句带路径的话（不算失败）。sendPhoto/sendVideo 说参数不对（如截图太长，宽高比超过 20）时改按文件再发一次。
func (a *Service) TgSendFile(c *TgCreds, path, caption string, replyTo int64) error {
	send, note, err := a.PrepareMedia(path)
	if err != nil {
		return err
	}
	if note != "" {
		if caption != "" {
			note = caption + "\n" + note
		}
		_, err := a.TgSend(c, textfmt.Esc(note), replyTo)
		return err
	}
	form := url.Values{"chat_id": {strconv.FormatInt(c.ChatID, 10)}}
	if caption != "" {
		form.Set("caption", textfmt.TruncRunes(caption, 1000))
	}
	if replyTo > 0 {
		form.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo))
	}
	method, field := "sendDocument", "document"
	switch mediaKind(send) {
	case "photo":
		if fi, err := os.Stat(send); err == nil && fi.Size() <= tgPhotoMax {
			method, field = "sendPhoto", "photo"
		}
	case "video":
		method, field = "sendVideo", "video"
		form.Set("supports_streaming", "true")
	}
	err = a.tgUpload(c, method, field, send, form)
	var te *TgError
	if method != "sendDocument" && errors.As(err, &te) && te.Code == 400 {
		form.Del("supports_streaming")
		err = a.tgUpload(c, "sendDocument", "document", send, form)
	}
	return err
}

type TgCreds struct {
	Token  string
	ChatID int64
}

func (a *Service) TelegramEnvPath() string {
	return filepath.Join(a.DataDir(), "secrets", "telegram.env")
}

// TelegramCreds：文件不存在返回 (nil, nil)，表示没配 Telegram（推送改用本机通知，也不收消息）；
// 存在但权限不是只有自己能读、缺键、chat id 不是数字时返回错误。
func (a *Service) TelegramCreds() (*TgCreds, error) {
	path := a.TelegramEnvPath()
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s 的权限是 %o，别的用户也能读，先 chmod 600 再用", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	tok, id := vals["TELEGRAM_BOT_TOKEN"], vals["TELEGRAM_CHAT_ID"]
	if tok == "" || id == "" {
		return nil, fmt.Errorf("%s 里缺 TELEGRAM_BOT_TOKEN 或 TELEGRAM_CHAT_ID（agentq notify setup 重新配）", path)
	}
	chat, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s 里的 TELEGRAM_CHAT_ID 不是数字", path)
	}
	return &TgCreds{Token: tok, ChatID: chat}, nil
}

var tokenRe = regexp.MustCompile(`bot\d+:[A-Za-z0-9_-]+`)

// Redact 把文字里的 token 打码（Go 的 http 报错会带上完整 URL，URL 里有 token）。
func (c *TgCreds) Redact(s string) string {
	if c != nil && c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "***")
	}
	return tokenRe.ReplaceAllString(s, "bot***")
}

// telegramAPI：环境变量 AGENTQ_TELEGRAM_API 可以换成自建的 Bot API 服务（测试里指向假服务；单元测试设 App.TelegramAPI）。
func (a *Service) telegramAPI() string {
	if a.TelegramAPI != "" {
		return strings.TrimRight(a.TelegramAPI, "/")
	}
	if v := os.Getenv("AGENTQ_TELEGRAM_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.telegram.org"
}

type TgError struct {
	Code int
	Desc string
}

func (e *TgError) Error() string { return fmt.Sprintf("Telegram 报错 %d：%s", e.Code, e.Desc) }

func (a *Service) tgCall(c *TgCreds, method string, form url.Values) (json.RawMessage, error) {
	return a.tgCallTimeout(c, method, form, TgCallWait)
}

// TgCallWait：一次普通调用最多等多久。测试里改小。
var TgCallWait = 20 * time.Second

// TgDownFor：一次调用超时之后，这么久内不再发（调用方常拿着 notify.lock、tick.lock）：攒了十几条要发时一条条等超时，
// 锁能被占好几分钟，notify send 拿不到锁，消息就丢了。消息都留在队列里，下次再发。收消息不受影响，长轮询通了就解除。
const TgDownFor = time.Minute

var errTgDown = errors.New("Telegram 刚才超时了，这会儿先不发，等下次再试")

func (a *Service) TgIsDown() bool {
	return !a.TgDownAt.IsZero() && time.Since(a.TgDownAt) < TgDownFor
}

// tgCallTimeout：长轮询的 getUpdates 要等 timeout 秒才返回，HTTP 超时得比它长。
func (a *Service) tgCallTimeout(c *TgCreds, method string, form url.Values, timeout time.Duration) (json.RawMessage, error) {
	if method != "getUpdates" && a.TgIsDown() {
		return nil, errTgDown
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.PostForm(a.telegramAPI()+"/bot"+c.Token+"/"+method, form)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			a.TgDownAt = time.Now()
		}
		return nil, errors.New(c.Redact(err.Error()))
	}
	if method == "getUpdates" {
		a.TgDownAt = time.Time{}
	}
	return tgParse(c, method, resp)
}

func tgParse(c *TgCreds, method string, resp *http.Response) (json.RawMessage, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, errors.New(c.Redact(err.Error()))
	}
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("Telegram %s 的返回看不懂（HTTP %d）：%s", method, resp.StatusCode, c.Redact(textfmt.OneLine(string(body), 200)))
	}
	if !r.OK {
		return nil, &TgError{Code: r.ErrorCode, Desc: c.Redact(r.Description)}
	}
	return r.Result, nil
}

// TgMaxRunes：留出余量的单条消息长度上限（接口上限 4096 字，按解析后的字数算）。
const TgMaxRunes = 3800

// TgSend 发一条 HTML 消息，replyTo > 0 时作为对那条消息的回复。HTML 解析不了（400 can't parse entities）时
// 去掉标签按纯文本重发一次，免得一个没转义的尖括号让整条消息丢掉。返回发出消息的 message_id（读不出时为 0）。
func (a *Service) TgSend(c *TgCreds, text string, replyTo int64) (int64, error) {
	return a.TgSendMarkup(c, text, replyTo, "")
}

// TgSendMarkup 同 tgSend，markup 非空时带上按钮（只有「要你定」的卡片用，见 internal/chat/cards.go）。
func (a *Service) TgSendMarkup(c *TgCreds, text string, replyTo int64, markup string) (int64, error) {
	form := url.Values{
		"chat_id":              {strconv.FormatInt(c.ChatID, 10)},
		"text":                 {text},
		"parse_mode":           {"HTML"},
		"link_preview_options": {`{"is_disabled":true}`},
	}
	if replyTo > 0 {
		form.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo))
	}
	if markup != "" {
		form.Set("reply_markup", markup)
	}
	raw, err := a.tgCall(c, "sendMessage", form)
	var te *TgError
	if errors.As(err, &te) && te.Code == 400 && strings.Contains(strings.ToLower(te.Desc), "parse") {
		form.Del("parse_mode")
		form.Set("text", textfmt.HTMLToPlain(text))
		raw, err = a.tgCall(c, "sendMessage", form)
	}
	// 太长（各处都有上限，这里是最后一道）：截断重发，免得这条消息永远发不出去
	if errors.As(err, &te) && te.Code == 400 && strings.Contains(strings.ToLower(te.Desc), "too long") {
		form.Del("parse_mode")
		form.Set("text", textfmt.TruncRunes(textfmt.HTMLToPlain(text), TgMaxRunes)+"…（太长，后面的截掉了）")
		raw, err = a.tgCall(c, "sendMessage", form)
	}
	if err != nil {
		return 0, err
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	if json.Unmarshal(raw, &sent) != nil {
		return 0, nil
	}
	return sent.MessageID, nil
}

// TgAnswer 回应按钮：不回应时客户端的进度条一直转。text 上限 200 字，空的什么都不显示。
func (a *Service) TgAnswer(c *TgCreds, callbackID, text string) error {
	form := url.Values{"callback_query_id": {callbackID}}
	if text != "" {
		form.Set("text", textfmt.TruncRunes(text, 190))
	}
	_, err := a.tgCall(c, "answerCallbackQuery", form)
	return err
}

// TgEdit 把消息改成 text，并去掉按钮。
func (a *Service) TgEdit(c *TgCreds, messageID int64, text string) error {
	_, err := a.tgCall(c, "editMessageText", url.Values{
		"chat_id":              {strconv.FormatInt(c.ChatID, 10)},
		"message_id":           {strconv.FormatInt(messageID, 10)},
		"text":                 {text},
		"parse_mode":           {"HTML"},
		"link_preview_options": {`{"is_disabled":true}`},
		"reply_markup":         {`{"inline_keyboard":[]}`},
	})
	return err
}

type TgMsg struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	ReplyTo *TgMsg `json:"reply_to_message"`
	// From：发消息的人。提交审核这种事不只看 chat id，还要是用户本人（见 internal/release/release_submit.go）。
	From *tgUser `json:"from"`
	// 附件：photo 是同一张图的几种尺寸（最后一个最大）；同一组相册的几条消息 media_group_id 相同。
	Photo        []TgFile `json:"photo"`
	Video        *TgFile  `json:"video"`
	Document     *TgFile  `json:"document"`
	Animation    *TgFile  `json:"animation"`
	VideoNote    *TgFile  `json:"video_note"`
	Voice        *TgFile  `json:"voice"`
	Audio        *TgFile  `json:"audio"`
	MediaGroupID string   `json:"media_group_id"`
}

type tgUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type TgFile struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FileName string `json:"file_name"`
}

type TgUpdate struct {
	UpdateID int64  `json:"update_id"`
	Message  *TgMsg `json:"message"`
	// CallbackQuery：用户点了消息上的按钮（见 internal/chat/cards.go）。
	CallbackQuery *TgCallback `json:"callback_query"`
}

type TgCallback struct {
	ID      string `json:"id"`
	From    tgUser `json:"from"`
	Message *TgMsg `json:"message"`
	Data    string `json:"data"`
}

// TgUpdates 取一次更新。wait 为 0 是短轮询（tick 兜底收消息时用，它拿着锁不能挂起）；
// 大于 0 是长轮询：没有新消息时服务器最多挂 wait 秒，有消息立刻返回（agentq listen 用）。
func (a *Service) TgUpdates(c *TgCreds, offset int64, wait int) ([]TgUpdate, error) {
	form := url.Values{"timeout": {strconv.Itoa(wait)}, "limit": {"50"}, "allowed_updates": {`["message","callback_query"]`}}
	if offset > 0 {
		form.Set("offset", strconv.FormatInt(offset, 10))
	}
	raw, err := a.tgCallTimeout(c, "getUpdates", form, time.Duration(wait)*time.Second+20*time.Second)
	if err != nil {
		return nil, err
	}
	var ups []TgUpdate
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("getUpdates 的结果看不懂：%v", err)
	}
	return ups, nil
}

// 官方上限（core.telegram.org/bots/api「Sending files」「getFile」）：机器人能下载的文件最大 20MB；
// multipart 上传图片最大 10MB、其他文件 50MB；sendPhoto 的宽高之和不超过 10000、宽高比不超过 20。
const (
	TgDownloadMax = 20 << 20
	tgPhotoMax    = 10 << 20
	tgUploadMax   = 50 << 20
	tgFileTimeout = 5 * time.Minute
)

// TgDownload 用 getFile 拿到文件路径再下载，存成 dest。超过 tgDownloadMax 的不下载（调用方先按 file_size 挡一次）。
// 返回 Telegram 那边的文件路径（带扩展名，没有文件名时用它补扩展名）。
func (a *Service) TgDownload(c *TgCreds, fileID, dest string) (string, error) {
	raw, err := a.tgCall(c, "getFile", url.Values{"file_id": {fileID}})
	if err != nil {
		return "", err
	}
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.FilePath == "" {
		return "", fmt.Errorf("getFile 没给出文件路径")
	}
	if f.FileSize > TgDownloadMax {
		return f.FilePath, fmt.Errorf("文件 %s，超过机器人能下载的 20 MB", textfmt.HumanSize(f.FileSize))
	}
	client := &http.Client{Timeout: tgFileTimeout}
	resp, err := client.Get(a.telegramAPI() + "/file/bot" + c.Token + "/" + f.FilePath)
	if err != nil {
		return f.FilePath, errors.New(c.Redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return f.FilePath, fmt.Errorf("下载文件 HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return f.FilePath, err
	}
	out, err := os.Create(dest)
	if err != nil {
		return f.FilePath, err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, TgDownloadMax+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > TgDownloadMax {
		err = fmt.Errorf("文件超过 20 MB")
	}
	if err != nil {
		os.Remove(dest)
		return f.FilePath, errors.New(c.Redact(err.Error()))
	}
	return f.FilePath, nil
}

// tgUpload 用 multipart 上传一个本地文件（sendPhoto、sendVideo、sendDocument），field 是放文件的参数名。
func (a *Service) tgUpload(c *TgCreds, method, field, path string, form url.Values) error {
	if a.TgIsDown() {
		return errTgDown
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		for k, vs := range form {
			for _, v := range vs {
				if err == nil {
					err = mw.WriteField(k, v)
				}
			}
		}
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile(field, filepath.Base(path)); err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	client := &http.Client{Timeout: tgFileTimeout}
	resp, err := client.Post(a.telegramAPI()+"/bot"+c.Token+"/"+method, mw.FormDataContentType(), pr)
	if err != nil {
		pr.CloseWithError(err)
		return errors.New(c.Redact(err.Error()))
	}
	_, err = tgParse(c, method, resp)
	return err
}
