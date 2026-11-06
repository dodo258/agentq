//go:build agentq_fake_deps

package setup

import "io/fs"

// 只有 install.sh 的测试带这个标记构建：不联网，用几个假的第三方技能代替锁定的 Waza。
// 发行构建（scripts/build-release.sh）不带这个标记，程序里没有这条路。
func init() {
	defaultFetch = func(fs.FS, bool) (map[string]InstallFile, error) {
		files := map[string]InstallFile{"licenses/Waza-LICENSE": {Data: []byte("test license"), Mode: 0o644}}
		for _, name := range []string{"check", "think", "hunt", "ui"} {
			files["skills/"+name+"/SKILL.md"] = InstallFile{Data: []byte("---\nname: " + name + "\ndescription: fixture\n---\n"), Mode: 0o644}
		}
		return files, nil
	}
}
