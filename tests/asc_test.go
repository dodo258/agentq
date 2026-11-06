package tests

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agentq/internal/asc"
	"agentq/internal/config"
	"agentq/internal/env"
)

// ---- 假 ASC ----

const (
	fakeASCKeyID  = "TESTKEY123"
	fakeASCIssuer = "11111111-2222-3333-4444-555555555555"
)

// testASCKey 运行时生成私钥（仓库里不放任何密钥），返回私钥和 PEM。
func testASCKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// fakeASC 是假的 App Store Connect：每个请求都按官方规则验 JWT（kid、iss、aud、有效期 ≤20 分钟、ES256 定长签名），
// lists 里的路径按 cursor 翻页（next 是绝对地址），fail 里的路径按次返回错误状态码。
type fakeASC struct {
	t        *testing.T
	srv      *httptest.Server
	pub      *ecdsa.PublicKey
	mu       sync.Mutex
	pageSize int
	lists    map[string][]map[string]any
	included map[string][]map[string]any
	fail     map[string][]int // 路径 → 接下来依次返回的状态码，0 表示这次正常
	calls    []string
	queries  map[string]url.Values // 路径 → 最近一次的查询参数
	badJWT   []string
	hang     map[string]time.Duration
	handle   func(w http.ResponseWriter, r *http.Request, body []byte) bool
	writes   []string // POST、PATCH 的「方法 路径」
	bodies   []string
}

func newFakeASC(t *testing.T, pub *ecdsa.PublicKey) *fakeASC {
	f := &fakeASC{t: t, pub: pub, pageSize: 200, lists: map[string][]map[string]any{}, included: map[string][]map[string]any{},
		fail: map[string][]int{}, queries: map[string]url.Values{}, hang: map[string]time.Duration{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func ascJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func ascErrBody(code int) map[string]any {
	return map[string]any{"errors": []map[string]any{{"status": strconv.Itoa(code), "code": "FAKE_ERROR", "title": "假错误", "detail": "假的错误说明 " + strconv.Itoa(code)}}}
}

func (f *fakeASC) checkJWT(r *http.Request) string {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "没有 Bearer"
	}
	parts := strings.Split(auth, ".")
	if len(parts) != 3 {
		return "不是三段"
	}
	dec := base64.RawURLEncoding
	var head struct{ Alg, Kid, Typ string }
	var pay struct {
		Iss      string
		Iat, Exp int64
		Aud      string
	}
	hb, _ := dec.DecodeString(parts[0])
	pb, _ := dec.DecodeString(parts[1])
	if json.Unmarshal(hb, &head) != nil || json.Unmarshal(pb, &pay) != nil {
		return "头或载荷不是 JSON"
	}
	if head.Alg != "ES256" || head.Kid != fakeASCKeyID || head.Typ != "JWT" {
		return "头不对"
	}
	if pay.Iss != fakeASCIssuer || pay.Aud != "appstoreconnect-v1" {
		return "载荷不对"
	}
	if pay.Exp <= pay.Iat || pay.Exp-pay.Iat > 1200 {
		return "有效期不对"
	}
	sig, err := dec.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return "签名不是 64 字节定长"
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(f.pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return "签名验不过"
	}
	return ""
}

func (f *fakeASC) serve(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.queries[r.URL.Path] = r.URL.Query()
	if why := f.checkJWT(r); why != "" {
		f.badJWT = append(f.badJWT, why)
		f.mu.Unlock()
		ascJSON(w, 401, ascErrBody(401))
		return
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, buf.String())
	}
	if d := f.hang[r.URL.Path]; d > 0 {
		f.mu.Unlock()
		select {
		case <-time.After(d):
		case <-r.Context().Done():
		}
		return
	}
	if q := f.fail[r.URL.Path]; len(q) > 0 {
		code := q[0]
		f.fail[r.URL.Path] = q[1:]
		if code != 0 {
			f.mu.Unlock()
			ascJSON(w, code, ascErrBody(code))
			return
		}
	}
	handle := f.handle
	f.mu.Unlock()
	if handle != nil && handle(w, r, buf.Bytes()) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	all, ok := f.lists[r.URL.Path]
	if !ok {
		all = []map[string]any{}
		if r.Method != http.MethodGet {
			ascJSON(w, 404, ascErrBody(404))
			return
		}
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	end := min(start+f.pageSize, len(all))
	page := all[min(start, len(all)):end]
	links := map[string]any{"self": f.srv.URL + r.URL.Path}
	if end < len(all) {
		links["next"] = fmt.Sprintf("%s%s?cursor=%d&limit=%d", f.srv.URL, r.URL.Path, end, f.pageSize)
	}
	doc := map[string]any{"data": page, "links": links, "meta": map[string]any{"paging": map[string]any{"total": len(all), "limit": f.pageSize}}}
	if inc := f.included[r.URL.Path]; inc != nil {
		doc["included"] = inc
	}
	ascJSON(w, 200, doc)
}

func (f *fakeASC) countCalls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeASC) writesList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeASC) bad() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.badJWT...)
}

func (f *fakeASC) setList(path string, items ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists[path] = append([]map[string]any{}, items...)
}

func (f *fakeASC) failNext(path string, codes ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[path] = codes
}

func (f *fakeASC) setHandle(h func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handle = h
}

func ascRes(typ, id string, attrs map[string]any) map[string]any {
	return map[string]any{"type": typ, "id": id, "attributes": attrs}
}

// testASCApp 建一个连着假 ASC 的 App（不起任何进程），私钥写进临时 HOME。
func testASCApp(t *testing.T) (*App, *fakeASC, *bytes.Buffer, []byte) {
	t.Helper()
	key, pemBytes := testASCKey(t)
	f := newFakeASC(t, &key.PublicKey)
	home := t.TempDir()
	writeASCConfig(t, home, pemBytes)
	out := &bytes.Buffer{}
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: out, Cfg: config.Default()}, ASC: &asc.Service{ASCAPI: f.srv.URL}})
	return a, f, out, pemBytes
}

func writeASCConfig(t *testing.T, home string, pemBytes []byte) {
	t.Helper()
	dir := filepath.Join(home, ".appstoreconnect")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AuthKey_TEST.p8"), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("export ASC_KEY_ID=%q\nASC_ISSUER_ID=%s\nASC_KEY_PATH=~/.appstoreconnect/AuthKey_TEST.p8\n", fakeASCKeyID, fakeASCIssuer)
	if err := os.WriteFile(filepath.Join(dir, "config.env"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testASCClient(t *testing.T) (*asc.Client, *fakeASC) {
	t.Helper()
	a, f, _, _ := testASCApp(t)
	c, err := a.ASC.NewASC()
	if err != nil {
		t.Fatal(err)
	}
	c.Backoff = func(int) time.Duration { return time.Millisecond }
	return c, f
}

// ---- 配置和私钥 ----

func TestParseASCKey(t *testing.T) {
	_, good := testASCKey(t)
	if _, err := asc.ParseASCKey(good); err != nil {
		t.Fatal(err)
	}
	body := bytes.TrimSpace(good)
	garbage := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("SECRETBYTES-not-a-key")})
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384der, _ := x509.MarshalPKCS8PrivateKey(p384)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaDer, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	for name, b := range map[string][]byte{
		"不是 PEM": []byte("SECRETBYTES"),
		"空":      nil,
		"内容不是密钥": garbage,
		"P-384":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p384der}),
		"RSA":    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDer}),
		"PEM 半截": body[:len(body)/2],
	} {
		_, err := asc.ParseASCKey(b)
		if err == nil {
			t.Errorf("%s：应报错", name)
			continue
		}
		if strings.Contains(err.Error(), "SECRETBYTES") || strings.Contains(err.Error(), string(body[30:60])) {
			t.Errorf("%s：错误信息带了内容：%v", name, err)
		}
	}
}

func TestSignASCToken(t *testing.T) {
	key, _ := testASCKey(t)
	now := time.Unix(1_800_000_000, 0)
	// 反复签：r、s 高位是 0 时也必须是定长 64 字节（ASN.1 或变长就错了）
	for i := 0; i < 300; i++ {
		tok, err := asc.SignASCToken(key, "KID", "ISS", now, asc.AscTokenLife)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			t.Fatalf("%q", tok)
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil || len(sig) != 64 {
			t.Fatalf("签名 %d 字节 %v", len(sig), err)
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if !ecdsa.Verify(&key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			t.Fatal("签名验不过")
		}
		if i == 0 {
			hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
			pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var head map[string]string
			var pay map[string]any
			json.Unmarshal(hb, &head)
			json.Unmarshal(pb, &pay)
			if head["alg"] != "ES256" || head["kid"] != "KID" || head["typ"] != "JWT" {
				t.Errorf("头 %v", head)
			}
			if pay["iss"] != "ISS" || pay["aud"] != "appstoreconnect-v1" || pay["iat"] != float64(now.Unix()) || pay["exp"] != float64(now.Add(asc.AscTokenLife).Unix()) {
				t.Errorf("载荷 %v", pay)
			}
		}
	}
	if asc.AscTokenLife > 20*time.Minute {
		t.Errorf("有效期 %s 超过官方上限", asc.AscTokenLife)
	}
	if _, err := asc.SignASCToken(key, "K", "I", now, 21*time.Minute); err == nil {
		t.Error("超过 20 分钟应拒绝")
	}
	if _, err := asc.SignASCToken(key, "K", "I", now, 0); err == nil {
		t.Error("0 应拒绝")
	}
}

// token 复用，快过期才换；时钟往回跳也换。
func TestASCTokenReuse(t *testing.T) {
	c, _ := testASCClient(t)
	now := time.Unix(1_800_000_000, 0)
	c.Now = func() time.Time { return now }
	first, err := c.Bearer()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if again, _ := c.Bearer(); again != first {
		t.Error("10 分钟内应复用")
	}
	now = now.Add(5 * time.Minute)
	if again, _ := c.Bearer(); again == first {
		t.Error("快过期应换新的")
	}
	mid, _ := c.Bearer()
	now = now.Add(-2 * time.Hour)
	if again, _ := c.Bearer(); again == mid {
		t.Error("时钟往回跳应换新的")
	}
}

func TestNewASCMissing(t *testing.T) {
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir(), Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}})
	if _, err := a.ASC.NewASC(); !errors.Is(err, asc.ErrASCNotConfigured) {
		t.Fatalf("没配：%v", err)
	}
	dir := filepath.Join(a.Home, ".appstoreconnect")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(a.ASC.AscConfigPath(), []byte("ASC_KEY_ID=k\nASC_ISSUER_ID=i\nASC_KEY_PATH=/nonexistent/k.p8\n"), 0o600)
	if _, err := a.ASC.NewASC(); err == nil || errors.Is(err, asc.ErrASCNotConfigured) {
		t.Fatalf("私钥文件不在：%v", err)
	}
}

// ---- 客户端 ----

func TestASCListPaging(t *testing.T) {
	c, f := testASCClient(t)
	f.pageSize = 2
	var items []map[string]any
	for i := 0; i < 5; i++ {
		items = append(items, ascRes("x", strconv.Itoa(i), nil))
	}
	f.setList("/v1/things", items...)
	l, err := c.List(context.Background(), "/v1/things", url.Values{"limit": {"2"}}, 10, nil)
	if err != nil || len(l.Items) != 5 || l.Truncated {
		t.Fatalf("%d %v %v", len(l.Items), l.Truncated, err)
	}
	if n := f.countCalls("GET /v1/things"); n != 3 {
		t.Errorf("翻 %d 页，应 3 页", n)
	}
	// 页数上限：只读两页，标记 Truncated
	l, err = c.List(context.Background(), "/v1/things", nil, 2, nil)
	if err != nil || len(l.Items) != 4 || !l.Truncated {
		t.Fatalf("上限：%d %v %v", len(l.Items), l.Truncated, err)
	}
	// stop：第一页后就停
	l, err = c.List(context.Background(), "/v1/things", nil, 10, func([]asc.Resource) bool { return true })
	if err != nil || len(l.Items) != 2 || l.Truncated {
		t.Fatalf("stop：%d %v %v", len(l.Items), l.Truncated, err)
	}
	if b := f.bad(); len(b) != 0 {
		t.Errorf("JWT 不合规：%v", b)
	}
}

// 下一页地址不在基地址上：不跟，token 不发到别处。
func TestASCListRefusesForeignNext(t *testing.T) {
	c, f := testASCClient(t)
	var evil int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evil++ }))
	defer other.Close()
	f.setHandle(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		ascJSON(w, 200, map[string]any{"data": []any{}, "links": map[string]any{"next": other.URL + "/v1/things?cursor=1"}})
		return true
	})
	if _, err := c.List(context.Background(), "/v1/things", nil, 5, nil); err == nil {
		t.Fatal("应报错")
	}
	if evil != 0 {
		t.Fatal("token 发到了别的地址")
	}
}

func TestASCRedirectNotFollowed(t *testing.T) {
	c, f := testASCClient(t)
	var evil int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evil++ }))
	defer other.Close()
	f.setHandle(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		return true
	})
	if _, err := c.List(context.Background(), "/v1/things", nil, 5, nil); err == nil {
		t.Fatal("跳转应当报错")
	}
	if evil != 0 {
		t.Fatal("跟随了跳转")
	}
}

func TestASCErrors(t *testing.T) {
	cases := []struct {
		name  string
		codes []int
		calls int
		ok    bool
		want  int
	}{
		{"401 不重试", []int{401}, 1, false, 401},
		{"403 不重试", []int{403}, 1, false, 403},
		{"429 不重试", []int{429, 0}, 1, false, 429},
		{"500 重试后成功", []int{500, 0}, 2, true, 0},
		{"503 两次后成功", []int{503, 500, 0}, 3, true, 0},
		{"500 一直失败", []int{500, 500, 500, 0}, 3, false, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := testASCClient(t)
			f.failNext("/v1/things", tc.codes...)
			_, err := c.List(context.Background(), "/v1/things", nil, 2, nil)
			if n := f.countCalls("GET /v1/things"); n != tc.calls {
				t.Errorf("调了 %d 次，应 %d 次", n, tc.calls)
			}
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var ae *asc.Error
			if !errors.As(err, &ae) || ae.Status != tc.want || !strings.Contains(err.Error(), "FAKE_ERROR") {
				t.Fatalf("%v", err)
			}
			if strings.Contains(err.Error(), "Bearer") || strings.Contains(err.Error(), c.Token) {
				t.Fatalf("错误信息带了 token：%v", err)
			}
		})
	}
}

func TestASCTimeout(t *testing.T) {
	c, f := testASCClient(t)
	f.mu.Lock()
	f.hang["/v1/things"] = 5 * time.Second
	f.mu.Unlock()
	c.Http.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := c.List(context.Background(), "/v1/things", nil, 2, nil)
	if err == nil {
		t.Fatal("应超时")
	}
	if strings.Contains(err.Error(), c.Token) || strings.Contains(err.Error(), "?") {
		t.Fatalf("错误信息不该带 token 或查询参数：%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("超时后不该等这么久：%s", time.Since(start))
	}
	if n := f.countCalls("GET /v1/things"); n != asc.AscGetTries {
		t.Errorf("超时算连接问题，应重试到 %d 次，实际 %d", asc.AscGetTries, n)
	}
	// ctx 取消：立刻返回
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.List(ctx, "/v1/things", nil, 2, nil); err == nil {
		t.Fatal("ctx 取消应报错")
	}
}

func TestASCBadJSON(t *testing.T) {
	c, f := testASCClient(t)
	f.setHandle(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		w.Write([]byte("<html>not json"))
		return true
	})
	if _, err := c.List(context.Background(), "/v1/things", nil, 2, nil); err == nil {
		t.Fatal("应报错")
	}
	f.setHandle(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		w.Write([]byte(`{"data":{"id":"x"}}`))
		return true
	})
	if _, err := c.List(context.Background(), "/v1/things", nil, 2, nil); err == nil {
		t.Fatal("data 不是列表应报错")
	}
}

// ---- bundle id ----
