package release

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/testkit"
)

// 抢记录：同时很多个，只有一个抢到。
func TestClaimOfferOnlyOne(t *testing.T) {
	t.Parallel()
	a := &Service{Env: &env.Env{Home: t.TempDir(), Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}}
	os.MkdirAll(a.DataDir(), 0o700)
	o := &ReleaseOffer{ID: "x", MessageID: 7, Status: OfferOffered, OfferedAt: time.Now()}
	b, _ := json.Marshal(&ReleaseStore{Offers: []*ReleaseOffer{o}})
	os.WriteFile(a.ReleasePath(), b, 0o600)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, why := a.claimOffer("x", 7, time.Now()); why == "" {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("抢到 %d 次", ok.Load())
	}
}

func TestParseReleaseOutput(t *testing.T) {
	ok := `{"ready":true,"version":"1.2.0","changes":["a","b","c"],"blocker":""}`
	v, cs, bl, err := parseReleaseOutput(backend.Result{Structured: json.RawMessage(ok)})
	if err != nil || v != "1.2.0" || len(cs) != 3 || bl != "" {
		t.Fatalf("%s %v %s %v", v, cs, bl, err)
	}
	_, _, bl, err = parseReleaseOutput(backend.Result{Structured: json.RawMessage(`{"ready":false,"version":"","changes":[],"blocker":"缺截图（进行中：x）"}`)})
	if err != nil || !strings.Contains(bl, "缺截图") || strings.Contains(bl, "（进行中") {
		t.Fatalf("%q %v", bl, err)
	}
	if _, _, bl, err := parseReleaseOutput(backend.Result{Structured: json.RawMessage(`{"ready":false,"version":"","changes":[],"blocker":""}`)}); err != nil || bl == "" {
		t.Fatalf("没写原因也要有一句：%q %v", bl, err)
	}
	// 清洗：一句一行、中和标记
	_, cs, _, err = parseReleaseOutput(backend.Result{Structured: json.RawMessage(`{"ready":true,"version":"1.0","changes":["第一行\n第二行 <b>x</b>"],"blocker":""}`)})
	if err != nil || strings.Contains(cs[0], "\n") {
		t.Fatalf("%q %v", cs, err)
	}
	for name, raw := range map[string]string{
		"缺失":       ``,
		"null":     `null`,
		"没有 ready": `{"version":"1.0","changes":["a"],"blocker":""}`,
		"多出字段":     `{"ready":true,"version":"1.0","changes":["a"],"blocker":"","x":1}`,
		"版本号乱写":    `{"ready":true,"version":"latest","changes":["a"],"blocker":""}`,
		"版本号带命令":   `{"ready":true,"version":"1.0;rm","changes":["a"],"blocker":""}`,
		"没有改动":     `{"ready":true,"version":"1.0","changes":[],"blocker":""}`,
		"改动太多":     `{"ready":true,"version":"1.0","changes":["a","b","c","d"],"blocker":""}`,
		"空句":       `{"ready":true,"version":"1.0","changes":["  "],"blocker":""}`,
		"后面还有":     ok + ` {}`,
		"类型不对":     `{"ready":"true","version":"1.0","changes":["a"],"blocker":""}`,
	} {
		if v, _, _, err := parseReleaseOutput(backend.Result{Structured: json.RawMessage(raw)}); err == nil {
			t.Errorf("%s：应报错，得到 %q", name, v)
		}
	}
}

func TestReleaseOfferTextEscapes(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.Local)
	got := releaseOfferText(ReleaseOffer{Project: "a<b>", Version: "1.0", Changes: []string{"修 <script> & 好了"}}, now)
	if strings.Contains(got, "<script>") || strings.Contains(got, "a<b>") || !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(got, "📦 <b>") || !strings.Contains(got, "准备好了") || !strings.Contains(got, ReleaseReplyHint) {
		t.Fatalf("%s", got)
	}
}

func TestReleaseConfig(t *testing.T) {
	d := config.Default()
	if d.ReleaseAssistEnabled || d.ReleaseBudgetUSD != 30 {
		t.Fatalf("%+v", d)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	for _, cfg := range []string{`{"release_budget_usd":0}`, `{"release_budget_usd":-1}`, `{"release_budget_usd":5000}`} {
		os.WriteFile(path, []byte(cfg), 0o644)
		if _, err := config.Load(path); err == nil {
			t.Errorf("%s 应报错", cfg)
		}
	}
	if !json.Valid([]byte(releaseSchema)) || !json.Valid([]byte(testkit.ClaudeDeny(backend.DenyStoreSubmit))) {
		t.Fatal("schema 或 deny 不是合法 JSON")
	}
	for _, want := range []string{"reviewSubmission", "appStoreVersionSubmission"} {
		if !strings.Contains(testkit.ClaudeDeny(backend.DenyStoreSubmit), want) || !strings.Contains(releasePromptHead, want) {
			t.Errorf("缺 %s", want)
		}
	}
}

// newReleaseApp 建带临时 HOME 的实例。这里的用例只用 release.json 和日志，不用接下层模块。
func newReleaseApp(t *testing.T) *Service {
	return &Service{Env: &env.Env{Home: t.TempDir(), Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}}
}

// offers 里有 null：JSON 能解析，后面每个循环都会崩。按坏文件处理：status 不崩，load 时备份并重置。
func TestReleaseNullOfferIsCorrupt(t *testing.T) {
	a := newReleaseApp(t)
	a.Cfg.ReleaseAssistEnabled = true
	p := a.ReleasePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"offers":[null,{"project":"p","version":"1.0","status":"offered"}]}`)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.ReleaseStatus(); !strings.Contains(got, "读不了") {
		t.Fatalf("status：%q", got)
	}
	s, err := a.loadRelease()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Offers) != 0 || s.UnknownSince.IsZero() {
		t.Fatalf("应重置并记下 UnknownSince：%+v", s)
	}
	baks, _ := filepath.Glob(p + ".bad-*")
	if len(baks) != 1 {
		t.Fatalf("应有 1 个备份：%v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); !bytes.Equal(b, raw) {
		t.Fatalf("备份内容不对：%s", b)
	}
}
