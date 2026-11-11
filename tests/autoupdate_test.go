package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 自动更新（auto_update）的验收用例：install.sh 从假发布服务器（AGENTQ_RELEASE_BASE）装好发行版 v1.0.0，
// 之后发布上出了 v2.0.0（VERSION、程序包、install.sh 都写进 SHA256SUMS，和 scripts/build-release.sh 一样）。
// 自动更新走的就是 agentq update 那一条路：真 install.sh、真安装引擎，只是服务器是假的。

// serveAuto 放出新版本：根目录（latest）有 VERSION 和校验文件，<版本>/ 下有程序包、install.sh、校验文件。badSum 时程序包的校验和是错的。
func (k *installKit) serveAuto(version string, badSum bool) {
	k.t.Helper()
	data := tarGz(k.t, "agentq", installBinary(k.t, version))
	script := testkit.ReadBytes(k.t, k.script)
	ver := []byte(version + "\n")
	sum := sha256Hex(data)
	if badSum {
		sum = strings.Repeat("0", 64)
	}
	sums := []byte(sum + "  " + k.platformFile() + "\n" + sha256Hex(ver) + "  VERSION\n" + sha256Hex(script) + "  install.sh\n")
	for _, dir := range []string{"", "/" + version} {
		k.setFile(dir+"/"+k.platformFile(), data)
		k.setFile(dir+"/install.sh", script)
		k.setFile(dir+"/VERSION", ver)
		k.setFile(dir+"/SHA256SUMS", sums)
	}
}

func (k *installKit) setConfig(kv map[string]any) {
	k.t.Helper()
	cfg := map[string]any{}
	if err := json.Unmarshal(testkit.ReadBytes(k.t, k.configPath()), &cfg); err != nil {
		k.t.Fatal(err)
	}
	for key, v := range kv {
		cfg[key] = v
	}
	b, _ := json.Marshal(cfg)
	testkit.WriteInstall(k.t, k.configPath(), string(b), 0o600)
}

func (k *installKit) mustAgentq(args ...string) string {
	k.t.Helper()
	out, code := k.agentq(k.home, args...)
	if code != 0 {
		k.t.Fatalf("agentq %v 退出码 %d：\n%s", args, code, out)
	}
	return out
}

func (k *installKit) upgradesLog() string {
	b, _ := os.ReadFile(filepath.Join(k.home, ".agentq", "evolve", "upgrades.jsonl"))
	return string(b)
}

// waitApply 等 agentq evolve apply 在升级记录里写出第 n 行 kind，并且退出（放开 upgrade.lock）。
func (k *installKit) waitApply(kind string, n int) {
	k.t.Helper()
	lock := filepath.Join(k.home, ".agentq", "evolve", "upgrade.lock")
	deadline := time.Now().Add(4 * time.Minute)
	for {
		_, _, held := fsx.LockHolder(lock)
		if strings.Count(k.upgradesLog(), `"kind":"`+kind+`"`) >= n && !held {
			return
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(k.home, ".agentq", "evolve", "upgrade.log"))
			k.t.Fatalf("等不到第 %d 行 %s：\n%s\n日志：\n%s", n, kind, k.upgradesLog(), log)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (k *installKit) version() string {
	k.t.Helper()
	return strings.TrimSpace(k.mustAgentq("version"))
}

// noticesText 是要推和推出去的消息（没配 Telegram 时推到本机通知，测试里写进日志）。
func (k *installKit) noticesText() string {
	q, _ := os.ReadFile(filepath.Join(k.home, ".agentq", "notify.json"))
	l, _ := os.ReadFile(filepath.Join(k.log, "macnotify.log"))
	return string(q) + string(l)
}

// fakeProxy 是只转发 http 的代理，记下经过它的路径。
func fakeProxy(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(s.Close)
	return s, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// 关着：整个 tick 不发一个网络请求，和没有这个功能一样；status 把它收进「没开」。暂停时开着也不看不装。
func TestAutoUpdateOffAndPaused(t *testing.T) {
	t.Parallel()
	t.Run("关着", func(t *testing.T) {
		t.Parallel()
		k := newWelcomeKit(t)
		k.mustInstall()
		k.serveAuto("v2.0.0", false)
		before := len(k.requests())
		k.mustAgentq("tick")
		k.mustAgentq("tick")
		if got := k.requests(); len(got) != before {
			t.Fatalf("关着不该联网：%v", got[before:])
		}
		if exists(filepath.Join(k.home, ".agentq", "evolve", "autoupdate.json")) || k.upgradesLog() != "" {
			t.Fatal("关着不该留下自动更新的记录")
		}
		if out := k.mustAgentq("status"); !strings.Contains(out, "没开：") || !strings.Contains(out, "自动更新") || strings.Contains(out, "自动更新：") {
			t.Fatalf("status 要把它收进没开：\n%s", out)
		}
		if v := k.version(); !strings.Contains(v, "v1.0.0") {
			t.Fatalf("不该装：%s", v)
		}
	})
	t.Run("暂停", func(t *testing.T) {
		t.Parallel()
		k := newWelcomeKit(t)
		k.mustInstall()
		k.serveAuto("v2.0.0", false)
		k.setConfig(map[string]any{"auto_update": true, "paused": true})
		before := len(k.requests())
		k.mustAgentq("tick")
		if got := k.requests(); len(got) != before {
			t.Fatalf("暂停时不该联网：%v", got[before:])
		}
		if out := k.mustAgentq("status"); !strings.Contains(out, "自动更新：开着，暂停时不看也不装") {
			t.Fatalf("status：\n%s", out)
		}
		if v := k.version(); !strings.Contains(v, "v1.0.0") {
			t.Fatalf("暂停时不该装：%s", v)
		}
	})
}

// 开着、空闲、有新版本：沿用安装时的代理装上新的一代，记一行；装上后任务变差就回滚到上一代、推通知，这个版本不再自动装。
func TestAutoUpdateInstallAndRollback(t *testing.T) {
	t.Parallel()
	k := newWelcomeKit(t)
	proxy, seen := fakeProxy(t)
	k.mustInstall("--proxy", proxy.URL)
	var net struct{ Proxy string }
	json.Unmarshal(testkit.ReadBytes(t, filepath.Join(k.home, ".agentq", "update-net.json")), &net)
	if net.Proxy != proxy.URL {
		t.Fatalf("安装时用的代理要记下：%+v", net)
	}
	genA := generation(t, k.home)
	k.serveAuto("v2.0.0", false)
	k.setConfig(map[string]any{"auto_update": true})
	before := len(seen())

	k.mustAgentq("tick") // 看新版本，空闲：起 agentq evolve apply
	k.waitApply("upgrade", 1)
	if v := k.version(); !strings.Contains(v, "v2.0.0") {
		t.Fatalf("要装上 v2.0.0：%s\n%s", v, k.upgradesLog())
	}
	if genB := generation(t, k.home); genB == genA || !exists(genA) {
		t.Fatalf("新的一代装在新目录、旧的一代留着：%s %s", genA, genB)
	}
	via := strings.Join(seen()[before:], " ")
	for _, want := range []string{"/VERSION", "/v2.0.0/install.sh", "/v2.0.0/" + k.platformFile()} {
		if !strings.Contains(via, want) {
			t.Fatalf("自动更新要沿用安装时的代理，%s 没走代理：%s", want, via)
		}
	}
	if ups := k.upgradesLog(); !strings.Contains(ups, `"release":"v2.0.0"`) || !strings.Contains(ups, `"from":"v1.0.0"`) {
		t.Fatalf("要记一行：\n%s", ups)
	}
	if q := k.noticesText(); !strings.Contains(q, "从 v1.0.0 升到 v2.0.0") {
		t.Fatalf("装上要推一条：\n%s", q)
	}
	if out := k.mustAgentq("status"); !strings.Contains(out, "自动更新：开着，上次看") || !strings.Contains(out, "已是最新") {
		t.Fatalf("status 要说上次看的时间和结果：\n%s", out)
	}

	// 装上后的 3 个任务都失败或超时：变差，回滚到上一代
	var at time.Time
	for _, line := range strings.Split(strings.TrimSpace(k.upgradesLog()), "\n") {
		var ev struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
		}
		json.Unmarshal([]byte(line), &ev)
		if ev.Kind == "upgrade" {
			at = ev.At
		}
	}
	statePath := filepath.Join(k.home, ".agentq", "state.json")
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{state.StatusFailed, state.StatusFailed, state.StatusTimeout} {
		start := at.Add(time.Duration(i+1) * time.Minute)
		end := start.Add(time.Minute)
		st.Jobs = append(st.Jobs, &state.Job{ID: "q" + string(rune('1'+i)), Project: "web", Title: "x", Status: s, StartedAt: start, EndedAt: &end})
	}
	if err := state.Save(statePath, st); err != nil {
		t.Fatal(err)
	}
	k.mustAgentq("tick")
	k.waitApply("rollback", 1)
	if v := k.version(); !strings.Contains(v, "v1.0.0") || generation(t, k.home) != genA {
		t.Fatalf("要回滚到上一代：%s %s", v, generation(t, k.home))
	}
	if q := k.noticesText(); !strings.Contains(q, "已回滚到") || !strings.Contains(q, "不再自动装") {
		t.Fatalf("回滚要推通知说原因：\n%s", q)
	}
	// 第二天又看一次，发布上还是 v2.0.0：不再自动装
	k.env = append(k.env, "AGENTQ_TEST_NOW="+time.Now().Add(25*time.Hour).Format(time.RFC3339))
	k.mustAgentq("tick")
	time.Sleep(2 * time.Second)
	if _, _, held := fsx.LockHolder(filepath.Join(k.home, ".agentq", "evolve", "upgrade.lock")); held || strings.Count(k.upgradesLog(), `"kind":"upgrade"`) != 1 {
		t.Fatalf("回滚过的版本不再自动装：\n%s", k.upgradesLog())
	}
	if v := k.version(); !strings.Contains(v, "v1.0.0") {
		t.Fatalf("还是上一代：%s", v)
	}

	// 关掉：和没有这个功能一样，不再联网
	k.setConfig(map[string]any{"auto_update": false})
	before = len(k.requests())
	k.mustAgentq("tick")
	if got := k.requests(); len(got) != before {
		t.Fatalf("关掉后不该联网：%v", got[before:])
	}
}

// 校验和对不上：不装，记一行、推一条；第二天再试还是不对，同一个版本不再推。
func TestAutoUpdateBadChecksum(t *testing.T) {
	t.Parallel()
	k := newWelcomeKit(t)
	k.mustInstall()
	k.serveAuto("v2.0.0", true)
	k.setConfig(map[string]any{"auto_update": true})
	k.mustAgentq("tick")
	k.waitApply("fail", 1)
	if v := k.version(); !strings.Contains(v, "v1.0.0") {
		t.Fatalf("校验不过不该装：%s", v)
	}
	if ups := k.upgradesLog(); !strings.Contains(ups, `"release":"v2.0.0"`) {
		t.Fatalf("要记一行：\n%s", ups)
	}
	if q := k.noticesText(); strings.Count(q, "装 v2.0.0 没成") != 1 {
		t.Fatalf("要推一条：\n%s", q)
	}
	os.Remove(filepath.Join(k.home, ".agentq", "notify.json")) // 推出去了
	logBefore, _ := os.ReadFile(filepath.Join(k.log, "macnotify.log"))

	k.env = append(k.env, "AGENTQ_TEST_NOW="+time.Now().Add(25*time.Hour).Format(time.RFC3339))
	k.mustAgentq("tick")
	k.waitApply("fail", 2)
	// 测试钟每个进程从 AGENTQ_TEST_NOW 起走：apply 记的失败时间比紧接着的 tick 的「现在」晚零点几秒，
	// 会被当成将来的记录不算，再起一个 apply 在测试结束后还写 HOME。往后拨一分钟，像真实时钟一样在它之后。
	k.env = append(k.env, "AGENTQ_TEST_NOW="+time.Now().Add(25*time.Hour+time.Minute).Format(time.RFC3339))
	k.mustAgentq("tick") // 把可能要推的推出去
	q, _ := os.ReadFile(filepath.Join(k.home, ".agentq", "notify.json"))
	logAfter, _ := os.ReadFile(filepath.Join(k.log, "macnotify.log"))
	if strings.Contains(string(q), "装 v2.0.0 没成") || strings.Count(string(logAfter), "装 v2.0.0 没成") != strings.Count(string(logBefore), "装 v2.0.0 没成") {
		t.Fatalf("同一个版本不再推：\n%s\n%s", q, logAfter)
	}
	if v := k.version(); !strings.Contains(v, "v1.0.0") {
		t.Fatalf("还是不该装：%s", v)
	}
}
