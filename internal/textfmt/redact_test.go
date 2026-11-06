package textfmt

import "testing"

func TestRedactPaths(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/My Projects/app 认不出默认分支（没有）":   "<本机路径>（没有）",
		"查 /tmp/x/y 的默认分支：git 失败":            "查 <本机路径>：git 失败",
		"门没过：~/projects/app，任务 0315-1":       "门没过：<本机路径>，任务 0315-1",
		"worktree（/private/var/t/wt）删不掉":     "worktree（<本机路径>）删不掉",
		"git -C '/srv/work/a' switch main":   "git -C '<本机路径>' switch main",
		"两个：/a/b，/c/d/e.txt":                 "两个：<本机路径>，<本机路径>",
		"分支 agent/1105-x 没过":                 "分支 agent/1105-x 没过",
		"看 /settings 页面":                     "看 /settings 页面",
		"origin/HEAD、本地 main、master 都没有":     "origin/HEAD、本地 main、master 都没有",
		"文件 docs/BACKLOG.md 和 .agentq/ready": "文件 docs/BACKLOG.md 和 .agentq/ready",
		"1/2 轮":                     "1/2 轮",
		"/opt/My Projects/app":      "<本机路径>",
		"https://example.com/a/b 看": "https://example.com/a/b 看",
		"":                          "",
	} {
		if got := RedactPaths(in); got != want {
			t.Errorf("%q：得到 %q，想要 %q", in, got, want)
		}
	}
}
