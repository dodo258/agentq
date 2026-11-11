package patrol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// govulncheck -format json 的样子：一条只到模块（没用到函数，不算），两条真用到了 stdlib 里有漏洞的函数，一条是依赖的。
const vulnJSON = `{"config":{"protocol_version":"v1.0.0"}}
{"osv":{"id":"GO-2026-0001","summary":"net/http 头部解析会卡死"}}
{"osv":{"id":"GO-2026-0002","summary":"crypto/tls 握手泄漏内存"}}
{"osv":{"id":"GO-2026-0003","summary":"x/net 的 html 解析越界"}}
{"osv":{"id":"GO-2026-0009","summary":"没用到的"}}
{"finding":{"osv":"GO-2026-0009","fixed_version":"v1.26.9","trace":[{"module":"stdlib","version":"v1.26.3"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v1.26.4","trace":[{"module":"stdlib","version":"v1.26.3","package":"net/http","function":"ReadRequest"},{"module":"agentq","function":"main"}]}}
{"finding":{"osv":"GO-2026-0002","fixed_version":"v1.26.5","trace":[{"module":"stdlib","version":"v1.26.3","package":"crypto/tls","function":"Dial"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v1.26.4","trace":[{"module":"stdlib","version":"v1.26.3","package":"net/http","function":"Serve"}]}}
{"finding":{"osv":"GO-2026-0003","fixed_version":"v0.40.0","trace":[{"module":"golang.org/x/net","version":"v0.39.0","package":"golang.org/x/net/html","function":"Parse"}]}}
`

func TestParseVulns(t *testing.T) {
	got, err := parseVulns(strings.NewReader(vulnJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Module != "golang.org/x/net" || got[1].Module != "stdlib" {
		t.Fatalf("按模块归两条：%+v", got)
	}
	std := got[1]
	if strings.Join(std.IDs, ",") != "GO-2026-0001,GO-2026-0002" || std.Version != "v1.26.3" || std.Fixed != "v1.26.5" || len(std.Summary) != 2 {
		t.Fatalf("没用到函数的不算，修好的版本取最高：%+v", std)
	}
	if _, err := parseVulns(strings.NewReader("{坏的")); err == nil {
		t.Fatal("读不懂要报错")
	}
}

// fakeVulncheck 把一个假 govulncheck 放进 PATH，调用次数记在 calls 里。
func fakeVulncheck(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "govulncheck"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return calls
}

func countCalls(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

// 每周一次：查正在运行的程序（二进制模式），有漏洞按模块报成问题，中间每小时照存下的结果报；满一周再查，修好了就算恢复。
func TestPatrolVulnWeekly(t *testing.T) {
	e := newPatrolFixture(t)
	out := filepath.Join(t.TempDir(), "out.json")
	os.WriteFile(out, []byte(vulnJSON), 0o644)
	calls := fakeVulncheck(t, "cat '"+out+"'\n")
	r := e.patrol()
	if countCalls(calls) != 1 || !strings.Contains(readFile(t, calls), "-mode=binary -format json /") {
		t.Fatalf("要用二进制模式查自己：%s", readFile(t, calls))
	}
	var titles []string
	for _, is := range r.New {
		if is.Check == "vuln" {
			titles = append(titles, is.Title)
		}
	}
	if len(titles) != 2 || !strings.Contains(strings.Join(titles, "\n"), "agentq 用的 Go v1.26.3 有已知漏洞 GO-2026-0001、GO-2026-0002，v1.26.5 修了") {
		t.Fatalf("按模块报：%v", titles)
	}
	e.now = e.now.Add(2 * time.Hour)
	r = e.patrol()
	if countCalls(calls) != 1 || len(r.Old) < 2 {
		t.Fatalf("一周内不再查，照存下的报：调用 %d 次，旧问题 %d", countCalls(calls), len(r.Old))
	}
	os.WriteFile(out, []byte(`{"config":{}}`+"\n"), 0o644)
	e.now = e.now.Add(7 * 24 * time.Hour)
	r = e.patrol()
	gone := 0
	for _, is := range r.Gone {
		if is.Check == "vuln" {
			gone++
		}
	}
	if countCalls(calls) != 2 || gone != 2 {
		t.Fatalf("满一周再查，修好了算恢复：调用 %d 次，恢复 %d 个", countCalls(calls), gone)
	}
}

// 没装 govulncheck：跳过、说明怎么装，status 带上，一周只说一次；跑不成时上次的结果照旧算。
func TestPatrolVulnSkipAndFail(t *testing.T) {
	e := newPatrolFixture(t)
	path := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	e.patrol()
	ps, _ := e.a.LoadPatrol()
	if ps.Vuln == nil || !strings.Contains(ps.Vuln.Note, "没装 govulncheck") || !strings.Contains(e.a.PatrolStatus(), "go install golang.org/x/vuln/cmd/govulncheck@latest") {
		t.Fatalf("没装要说明：%+v %s", ps.Vuln, e.a.PatrolStatus())
	}
	e.now = e.now.Add(time.Hour)
	e.patrol()
	if n := strings.Count(e.out.String(), "没装 govulncheck"); n != 1 {
		t.Fatalf("一周只说一次：%d\n%s", n, e.out)
	}

	t.Setenv("PATH", path)
	e = newPatrolFixture(t)
	out := filepath.Join(t.TempDir(), "out.json")
	os.WriteFile(out, []byte(vulnJSON), 0o644)
	fakeVulncheck(t, "[ -f '"+out+".fail' ] && { echo 没网 >&2; exit 1; }\ncat '"+out+"'\n")
	e.patrol()
	os.WriteFile(out+".fail", nil, 0o644)
	e.now = e.now.Add(8 * 24 * time.Hour)
	r := e.patrol()
	n := 0
	for _, is := range r.Old {
		if is.Check == "vuln" {
			n++
		}
	}
	ps, _ = e.a.LoadPatrol()
	if n != 2 || !strings.Contains(ps.Vuln.Note, "没网") {
		t.Fatalf("跑不成时上次的结果照旧：%d %+v", n, ps.Vuln)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
