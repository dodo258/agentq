package textfmt

import (
	"testing"
)

func TestFmtTokens(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1234: "1.2K", 6388024: "6.39M", 1_234_567_890: "1.23B"} {
		if got := FmtTokens(n); got != want {
			t.Errorf("fmtTokens(%d) = %q，应为 %q", n, got, want)
		}
	}
}
