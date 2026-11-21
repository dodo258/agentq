package notify

import (
	"slices"
	"testing"
)

// 各种依赖清单只认直接依赖；坏文件当没有依赖。
func TestDepsParsers(t *testing.T) {
	for _, c := range []struct {
		file, text string
		want       []string
	}{
		{"go.mod", "module x\n\nrequire example.com/one v1.0.0\n\nrequire (\n\texample.com/two v1.0.0\n\texample.com/ind v1.0.0 // indirect\n)\n", []string{"example.com/one", "example.com/two"}},
		{"package.json", `{"name":"x","scripts":{"build":"tsc"},"dependencies":{"react":"^19"},"devDependencies":{"vite":"^7"}}`, []string{"react", "vite"}},
		{"package.json", `{坏的`, nil},
		{"Cargo.toml", "[package]\nname = \"x\"\n\n[dependencies]\nserde = \"1\"\ntokio = { version = \"1\" }\n\n[dev-dependencies]\ninsta = \"1\"\n\n[dependencies.clap]\nversion = \"4\"\n\n[features]\ndefault = []\n", []string{"serde", "tokio", "insta", "clap"}},
		{"requirements-dev.txt", "# 注释\nRequests==2.0\n-r base.txt\nnumpy>=1\n", []string{"requests", "numpy"}},
		{"README.md", "", nil},
	} {
		parse := depsParser(c.file)
		if parse == nil {
			if c.want != nil {
				t.Errorf("%s 应认得", c.file)
			}
			continue
		}
		if got := parse(c.text); !slices.Equal(got, c.want) {
			t.Errorf("%s：%q，要 %q", c.file, got, c.want)
		}
	}
}
