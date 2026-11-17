package evolve

import (
	"slices"
	"testing"
)

func TestPathDomain(t *testing.T) {
	for p, want := range map[string]string{
		"web/App.tsx": DomainFrontend, "src/styles/main.css": DomainFrontend, "src/components/": DomainFrontend, "app/lib/x.ts": DomainFrontend,
		"internal/evolve/": DomainBackend, "server/routes.ts": DomainBackend, "main.go": DomainBackend, "api": DomainBackend,
		"docs/design/home.md": DomainDesign, "assets/logo.svg": DomainDesign, "Views/HomeView.swift": DomainFrontend,
		"README.md": DomainDocs, "docs/": DomainDocs, "Models/Ledger.swift": "", "Makefile": "", "": "",
		"DECISIONS.md": "", "CHANGELOG.md": "", "docs/BACKLOG.md": "", "app/PROGRESS.md": "", "LESSONS.md": "",
		"lib/setup/password_dialog.dart": DomainFrontend, "lib/settings_screen.dart": DomainFrontend, "lib/widgets/x.dart": DomainFrontend,
		"lib/app_lock.dart": "", "app/src/main/LoginPage.kt": DomainFrontend,
	} {
		if got := pathDomain(p); got != want {
			t.Errorf("%q：%q，要 %q", p, got, want)
		}
	}
}

// 一次改动归到主体改的领域：顺手改的记录文件、说明文档不顶替主体，也不多记一个「文档」。
func TestFilesDomains(t *testing.T) {
	for _, c := range []struct {
		name  string
		paths []string
		want  []string
	}{
		// 11-16 一个 Flutter App 的密码页：Flutter 代码加 DECISIONS.md，以前只认得出 DECISIONS.md，记成文档返工。
		{"认不出主体时不让记录文件顶上", []string{"DECISIONS.md", "lib/app_lock.dart", "lib/l10n/app_en.arb", "test/app_lock_test.dart"}, nil},
		{"Flutter 界面", []string{"DECISIONS.md", "lib/setup/password_dialog.dart", "lib/app_lock.dart"}, []string{DomainFrontend}},
		// 11-16 一个官网的手机一栏：前端代码加 DECISIONS.md，以前多记一个文档。
		{"前端加记录文件", []string{"DECISIONS.md", "src/content.ts", "src/styles.css", "src/fonts/x.woff2"}, []string{DomainFrontend}},
		{"代码顺手改说明文档", []string{"internal/evolve/x.go", "docs/reference/commands.md", "CHANGELOG.md", "README.md"}, []string{DomainBackend}},
		{"认不出的代码也压住文档", []string{"Sources/Ledger.swift", "README.md"}, nil},
		{"只改文档", []string{"README.md", "docs/guide.md", "CHANGELOG.md"}, []string{DomainDocs}},
		{"范围写的目录", []string{"docs/"}, []string{DomainDocs}},
		{"设计稿和代码", []string{"docs/design/home.md", "web/App.tsx", "README.md"}, []string{DomainFrontend, DomainDesign}},
		{"只有记录文件", []string{"DECISIONS.md", "docs/BACKLOG.md"}, nil},
		{"不是代码的配置不压文档", []string{"README.md", "Makefile", ".github/workflows/ci.yml"}, []string{DomainDocs}},
		{"空", nil, nil},
	} {
		if got := filesDomains(c.paths); !slices.Equal(got, c.want) {
			t.Errorf("%s：%q，要 %q", c.name, got, c.want)
		}
	}
}
