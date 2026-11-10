package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"agentq/internal/fsx"
)

// ConsentTextVersion 是 agentq start --welcome 装完告知那段话的版本（第 2 版起安装即同意，只告知不问）。文字改了就加一：不重新问，只在 doctor 里说明。
const ConsentTextVersion = 2

// Consent 是 ~/.agentq/consent.json：在什么时候、以什么方式同意了哪一版文字。怎么问、算不算同意在 onboard，这里只管读写，因为 doctor 也要读。
type Consent struct {
	TextVersion int    `json:"text_version"`
	Time        string `json:"time"`
	Method      string `json:"method"`
}

// ConsentPath 是同意记录的位置。
func ConsentPath(home string) string { return filepath.Join(home, ".agentq", "consent.json") }

// ReadConsent 读同意记录；没有文件返回 nil。
func ReadConsent(home string) (*Consent, error) {
	b, e := os.ReadFile(ConsentPath(home))
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, fmt.Errorf("读同意记录失败：%w", e)
	}
	var c Consent
	if e = json.Unmarshal(b, &c); e != nil {
		return nil, fmt.Errorf("同意记录 %s 格式不对：%w", ConsentPath(home), e)
	}
	return &c, nil
}

// WriteConsent 原子写同意记录。
func WriteConsent(home string, c Consent) error {
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return fmt.Errorf("编码同意记录失败：%w", e)
	}
	if e = fsx.WriteFileAtomic(ConsentPath(home), append(b, '\n')); e != nil {
		return fmt.Errorf("写同意记录失败：%w", e)
	}
	return nil
}

// consentNote 是 doctor 里的那一行：没有记录或版本一致都不说。
func consentNote(home string) string {
	c, e := ReadConsent(home)
	if e != nil {
		return e.Error()
	}
	if c == nil || c.TextVersion == ConsentTextVersion {
		return ""
	}
	return fmt.Sprintf("同意文字更新过：你同意的是第 %d 版，现在是第 %d 版。不用重新同意。", c.TextVersion, ConsentTextVersion)
}
