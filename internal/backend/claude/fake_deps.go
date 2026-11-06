//go:build agentq_fake_deps

package claude

// 只有 install.sh 的测试带这个标记构建：这台机器上真的 CLI 不算数，测试只看临时 HOME 和 PATH。
func init() { fixedCLIPaths = nil }
