// Package asc 是 App Store Connect 客户端：鉴权、分页读、把 App 映射到项目。这里只有读；写请求只在发版助手的 send 里。
package asc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/textfmt"
)

// Service 建 App Store Connect 客户端用的模块实例。
type Service struct {
	*env.Env
	// ASCAPI 非空时替换 App Store Connect 的基地址（测试里指向假服务）；真实运行用官方地址。
	ASCAPI string
}

// App Store Connect API 客户端（只用标准库）。
// 私钥只在这个文件里解析和使用：不打印、不写日志、不进错误信息、不进模型、不进 meta；配置文件按文本解析，绝不执行 shell。
const (
	ascDefaultAPI  = "https://api.appstoreconnect.apple.com"
	ascAudience    = "appstoreconnect-v1"
	AscTokenLife   = 15 * time.Minute // 官方：超过 20 分钟的 token 无效（少数只读资源除外，这里用不到）
	ascTokenMargin = time.Minute      // 离过期不到这么久就换新的
	ascTokenMax    = 20 * time.Minute
	ascHTTPTimeout = 30 * time.Second
	ascMaxBody     = 32 << 20
	AscGetTries    = 3 // 只读请求遇到 5xx 或连接问题最多试几次；429 不重试（官方建议记下来以后再处理）
)

// ErrASCNotConfigured：没有 ~/.appstoreconnect/config.env。是正常情况，不算出错。
var ErrASCNotConfigured = errors.New("没配 App Store Connect（~/.appstoreconnect/config.env 不存在）")

type ascCreds struct {
	KeyID, IssuerID, KeyPath string
}

func (a *Service) AscConfigPath() string {
	return filepath.Join(a.Home, ".appstoreconnect", "config.env")
}

func (a *Service) ascAPI() string {
	if a.ASCAPI != "" {
		return strings.TrimRight(a.ASCAPI, "/")
	}
	if v := os.Getenv("AGENTQ_ASC_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return ascDefaultAPI
}

// parseASCEnv 解析 config.env：只认 KEY=VALUE（可带 export、成对引号、行尾注释），不展开变量（路径开头的 ~、$HOME 除外），不执行任何东西。
// 报错只提键名，不带值。
func parseASCEnv(text, home string) (ascCreds, error) {
	vals := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				v = v[1 : end+1]
			}
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		vals[strings.TrimSpace(k)] = v
	}
	c := ascCreds{KeyID: vals["ASC_KEY_ID"], IssuerID: vals["ASC_ISSUER_ID"], KeyPath: vals["ASC_KEY_PATH"]}
	var missing []string
	for _, f := range []struct{ name, val string }{{"ASC_KEY_ID", c.KeyID}, {"ASC_ISSUER_ID", c.IssuerID}, {"ASC_KEY_PATH", c.KeyPath}} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return ascCreds{}, errors.New("config.env 缺 " + strings.Join(missing, "、"))
	}
	for _, prefix := range []string{"~/", "$HOME/", "${HOME}/"} {
		if rest, ok := strings.CutPrefix(c.KeyPath, prefix); ok {
			c.KeyPath = filepath.Join(home, rest)
			break
		}
	}
	if strings.Contains(c.KeyPath, "$") || strings.Contains(c.KeyPath, "`") {
		return ascCreds{}, errors.New("config.env 的 ASC_KEY_PATH 里有变量或命令替换：这里不执行 shell，请写完整路径")
	}
	if !filepath.IsAbs(c.KeyPath) {
		c.KeyPath = filepath.Join(home, ".appstoreconnect", c.KeyPath)
	}
	return c, nil
}

// ParseASCKey 解析 .p8（PKCS#8 PEM 的 P-256 私钥）。错误信息不带文件内容，也不包底层错误。
func ParseASCKey(b []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("App Store Connect 私钥不是 PEM 格式")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("App Store Connect 私钥不是 PKCS#8 格式")
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("App Store Connect 私钥不是 P-256 椭圆曲线密钥")
	}
	return ec, nil
}

// SignASCToken 按官方「Generating Tokens for API Requests」签 JWT。ES256 的签名是 r、s 各 32 字节定长拼接（RFC 7518 §3.4），不是 ASN.1。
func SignASCToken(key *ecdsa.PrivateKey, kid, iss string, now time.Time, life time.Duration) (string, error) {
	if life <= 0 || life > ascTokenMax {
		return "", fmt.Errorf("token 有效期 %s 不在 (0, %s] 内", life, ascTokenMax)
	}
	head, err := json.Marshal(map[string]string{"alg": "ES256", "kid": kid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{"iss": iss, "iat": now.Unix(), "exp": now.Add(life).Unix(), "aud": ascAudience})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(head) + "." + enc.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", errors.New("签 App Store Connect token 失败")
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig), nil
}

// Error：接口返回了非 2xx。Detail 来自 Apple 的 ErrorResponse。
type Error struct {
	Method, Path string
	Status       int
	Code, Detail string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("App Store Connect %s %s：HTTP %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Detail != "" {
		msg += "：" + textfmt.OneLine(e.Detail, 200)
	}
	return msg
}

// Resource 是 JSON:API 的一条资源。
type Resource struct {
	Type          string                  `json:"type"`
	ID            string                  `json:"id"`
	Attributes    json.RawMessage         `json:"attributes"`
	Relationships map[string]Relationship `json:"relationships"`
}

type Relationship struct {
	Data json.RawMessage `json:"data"`
}

// Related 取一对一关系指向的资源 id，没有返回空。
func (r Resource) Related(name string) string {
	rel, ok := r.Relationships[name]
	if !ok || len(rel.Data) == 0 {
		return ""
	}
	var d struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(rel.Data, &d) != nil {
		return ""
	}
	return d.ID
}

// Doc 是一次响应。
type Doc struct {
	Data     json.RawMessage `json:"data"`
	Included []Resource      `json:"included"`
	Links    struct {
		Next string `json:"next"`
	} `json:"links"`
}

func (d *Doc) Many() ([]Resource, error) {
	var out []Resource
	if err := json.Unmarshal(d.Data, &out); err != nil {
		return nil, fmt.Errorf("App Store Connect 响应的 data 不是列表：%w", err)
	}
	return out, nil
}

func (d *Doc) One() (Resource, error) {
	var out Resource
	if err := json.Unmarshal(d.Data, &out); err != nil {
		return out, fmt.Errorf("App Store Connect 响应的 data 不是单条资源：%w", err)
	}
	return out, nil
}

// Client：一个进程里顺序使用，不并发。
type Client struct {
	base    *url.URL
	Http    *http.Client
	creds   ascCreds
	key     *ecdsa.PrivateKey
	Now     func() time.Time
	Token   string
	expires time.Time
	Backoff func(try int) time.Duration
}

// NewASC 读配置和私钥。没配时返回 errASCNotConfigured。
func (a *Service) NewASC() (*Client, error) {
	b, err := os.ReadFile(a.AscConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrASCNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("读 App Store Connect 配置：%w", err)
	}
	creds, err := parseASCEnv(string(b), a.Home)
	if err != nil {
		return nil, err
	}
	pemBytes, err := os.ReadFile(creds.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("读 App Store Connect 私钥文件：%w", err)
	}
	key, err := ParseASCKey(pemBytes)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(a.ascAPI())
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("App Store Connect 接口地址不对：%q", a.ascAPI())
	}
	return &Client{
		base:  base,
		creds: creds,
		key:   key,
		Now:   a.Now,
		Http: &http.Client{
			Timeout: ascHTTPTimeout,
			// 不跟随跳转：官方接口不跳转，跟了可能把请求发到别处
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Backoff: func(try int) time.Duration { return time.Duration(try+1) * 2 * time.Second },
	}, nil
}

func (c *Client) Bearer() (string, error) {
	now := c.Now()
	if c.Token != "" && now.Add(ascTokenMargin).Before(c.expires) && !now.Before(c.expires.Add(-ascTokenMax)) {
		return c.Token, nil
	}
	t, err := SignASCToken(c.key, c.creds.KeyID, c.creds.IssuerID, now, AscTokenLife)
	if err != nil {
		return "", err
	}
	c.Token, c.expires = t, now.Add(AscTokenLife)
	return t, nil
}

// RoundTrip 发一次请求。返回的错误里只有方法和路径，没有 token。
func (c *Client) RoundTrip(ctx context.Context, method, target string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("建 App Store Connect 请求：%w", err)
	}
	tok, err := c.Bearer()
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("App Store Connect %s %s：%w", method, req.URL.Path, stripURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, ascMaxBody))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读 App Store Connect 响应 %s %s：%w", method, req.URL.Path, err)
	}
	return resp.StatusCode, data, nil
}

// stripURL：*url.Error 带完整地址（含查询参数），日志里不需要，只留底层原因。
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func HTTPError(method, path string, status int, data []byte) *Error {
	e := &Error{Method: method, Path: path, Status: status}
	var er struct {
		Errors []struct {
			Code, Title, Detail string
		} `json:"errors"`
	}
	if json.Unmarshal(data, &er) == nil && len(er.Errors) > 0 {
		e.Code, e.Detail = er.Errors[0].Code, er.Errors[0].Detail
		if e.Detail == "" {
			e.Detail = er.Errors[0].Title
		}
	}
	return e
}

func (c *Client) Decode(method, path string, data []byte) (*Doc, error) {
	var d Doc
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("App Store Connect %s %s 的响应看不懂：%w", method, path, err)
	}
	return &d, nil
}

// GetURL 发只读请求：5xx、连接错误和超时短退避后重试，429 和其他 4xx 不重试。
func (c *Client) GetURL(ctx context.Context, target string) (*Doc, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("App Store Connect 地址不对：%w", err)
	}
	var last error
	for try := 0; try < AscGetTries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.Backoff(try - 1)):
			}
		}
		status, data, err := c.RoundTrip(ctx, http.MethodGet, target, nil)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, err
			}
			last = err
		case status >= 200 && status < 300:
			return c.Decode(http.MethodGet, u.Path, data)
		case status >= 500:
			last = HTTPError(http.MethodGet, u.Path, status, data)
		default:
			return nil, HTTPError(http.MethodGet, u.Path, status, data)
		}
	}
	return nil, last
}

func (c *Client) Endpoint(path string, q url.Values) string {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawQuery = q.Encode()
	return u.String()
}

// List 是翻页读到的全部内容。Truncated：到了页数上限，后面还有。
type List struct {
	Items     []Resource
	Included  map[string]Resource // "type/id" → 资源
	Truncated bool
}

// List 顺着 links.next 翻页，最多 maxPages 页；stop 非空且返回真时读完这一页就停。next 只接受和基地址相同的 scheme、host，别处的一律报错。
func (c *Client) List(ctx context.Context, path string, q url.Values, maxPages int, stop func(page []Resource) bool) (List, error) {
	out := List{Included: map[string]Resource{}}
	target := c.Endpoint(path, q)
	for page := 0; ; page++ {
		d, err := c.GetURL(ctx, target)
		if err != nil {
			return out, err
		}
		items, err := d.Many()
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, items...)
		for _, r := range d.Included {
			out.Included[r.Type+"/"+r.ID] = r
		}
		if d.Links.Next == "" || (stop != nil && stop(items)) {
			return out, nil
		}
		if page+1 >= maxPages {
			out.Truncated = true
			return out, nil
		}
		next, err := url.Parse(d.Links.Next)
		if err != nil || next.Scheme != c.base.Scheme || next.Host != c.base.Host {
			return out, fmt.Errorf("App Store Connect 给的下一页地址不在 %s 上，不跟", c.base.Host)
		}
		target = d.Links.Next
	}
}

// Time 解析接口里的 date-time，解析不了返回零值。
func Time(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

type App struct {
	ID, Name, BundleID string
}

// MappedApp 是对应上了项目的 App。
type MappedApp struct {
	App
	Project backlog.Project
}

const ascAppPages = 10

func (c *Client) Apps(ctx context.Context) ([]App, error) {
	q := url.Values{"fields[apps]": {"name,bundleId"}, "limit": {"200"}}
	l, err := c.List(ctx, "/v1/apps", q, ascAppPages, nil)
	if err != nil {
		return nil, err
	}
	var out []App
	for _, r := range l.Items {
		var at struct {
			Name     string `json:"name"`
			BundleID string `json:"bundleId"`
		}
		if json.Unmarshal(r.Attributes, &at) != nil || r.ID == "" || at.BundleID == "" {
			continue
		}
		out = append(out, App{ID: r.ID, Name: at.Name, BundleID: at.BundleID})
	}
	return out, nil
}

var (
	bundleIDRe      = regexp.MustCompile(`PRODUCT_BUNDLE_IDENTIFIER["']?\s*[:=]\s*["']?([^"'\s;]+)`)
	bundleIDValidRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

// bundleWalkSkip：找工程文件时不进这些目录（.claude 里有 worktree，会把别的分支算进来）。
var bundleWalkSkip = map[string]bool{".git": true, ".claude": true, "node_modules": true, "Pods": true, "DerivedData": true,
	"build": true, ".build": true, "vendor": true, "Carthage": true, "fastlane": true}

// bundleIDsIn 从工程文件（*.xcodeproj/project.pbxproj、project.yml）里取 PRODUCT_BUNDLE_IDENTIFIER，只看三层以内。带变量的值（$(…)）不算。
func bundleIDsIn(dir string) []string {
	seen := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if path != dir && (bundleWalkSkip[d.Name()] || depth >= 3) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		isProj := name == "project.pbxproj" && strings.HasSuffix(filepath.Base(filepath.Dir(path)), ".xcodeproj")
		if !isProj && name != "project.yml" && name != "project.yaml" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range bundleIDRe.FindAllStringSubmatch(string(b), -1) {
			if bundleIDValidRe.MatchString(m[1]) {
				seen[m[1]] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MapApps 按 bundle id 精确匹配把 App 对应到项目。对应不上、或同一个 bundle id 出现在多个项目里（说不清是哪个）的放进 unmapped，附原因。
func MapApps(apps []App, projects []backlog.Project) (mapped []MappedApp, unmapped []string) {
	owners := map[string][]backlog.Project{}
	for _, p := range projects {
		for _, id := range bundleIDsIn(p.Dir) {
			owners[id] = append(owners[id], p)
		}
	}
	for _, app := range apps {
		switch ps := owners[app.BundleID]; len(ps) {
		case 1:
			mapped = append(mapped, MappedApp{App: app, Project: ps[0]})
		case 0:
			unmapped = append(unmapped, fmt.Sprintf("%s（projects_root 下没有工程用这个 bundle id）", app.Name))
		default:
			unmapped = append(unmapped, fmt.Sprintf("%s（%d 个项目都用这个 bundle id，说不清是哪个）", app.Name, len(ps)))
		}
	}
	return mapped, unmapped
}

func ResourceAt(r Resource) time.Time {
	var at struct {
		CreatedDate string `json:"createdDate"`
	}
	_ = json.Unmarshal(r.Attributes, &at)
	return Time(at.CreatedDate)
}
