package onboard

import (
	"bytes"
	"strings"
	"testing"
)

// 只有空行算同意。
func TestAskConsent(t *testing.T) {
	for in, want := range map[string]bool{
		"\n":    true,
		"\r\n":  true,
		"":      false, // EOF
		"n\n":   false,
		"y\n":   false,
		"yes\n": false,
		" \n":   false,
		"\t\n":  false,
		"n":     false, // 没有换行就 EOF
		"\n\n":  true,  // 只看第一行
		"x\n\n": false,
	} {
		var out bytes.Buffer
		got, err := askConsent(strings.NewReader(in), &out)
		if err != nil || got != want {
			t.Errorf("%q：%v %v，想要 %v", in, got, err, want)
		}
		if !strings.Contains(out.String(), consentPrompt) {
			t.Errorf("%q：没有问：%q", in, out.String())
		}
	}
}
