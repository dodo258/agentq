package main

import (
	"embed"

	"agentq/internal/app"
	"agentq/internal/config"
)

// 编译进二进制的资产：go:embed 不能写 ..，目录也不动，所以放根目录，由 builtinAssets 交给组装层。

//go:embed skills templates support rules dependencies/claude.lock.json
var installAssets embed.FS

// 评测题要带 .agentq、.gitignore 这些点开头的文件，所以写 all:。
//
//go:embed all:evals
var evalAssets embed.FS

//go:embed prompts/night.md
var nightPrompt string

//go:embed prompts/review.md
var reviewPrompt string

// routineText 是编译进二进制的说明文件；定时表里不放提示词文本。
var routineText = map[string]string{config.KindNight: nightPrompt, config.KindReview: reviewPrompt}

// builtinAssets 把上面这些交给组装层。
func builtinAssets() app.Assets {
	return app.Assets{RoutineText: routineText, Install: installAssets, Evals: evalAssets}
}
