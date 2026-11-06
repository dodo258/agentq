package tests

import (
	"io"

	"agentq/internal/app"
)

// 留根的用例建 App 都走这两个：产品代码的 Wire、New 要 main 传资产，用例用同一份编译进二进制的。

func wireApp(a *App) *App { return app.Wire(a, builtinAssets()) }

func newApp(home string, out io.Writer) (*App, error) {
	return app.New(home, out, builtinAssets())
}
