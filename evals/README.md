# 评测集

`agentq eval` 的公开题。每题一个目录 `<题号>-<短名>/`，题目编译进二进制，评判用运行中那一代的原件。

| 文件 | 作用 |
|---|---|
| `case.json` | 领域、种类（`task`、`new`、`program`）、一句话需求、期望结果（字段见 `internal/evolve/case.go` 的 `Expect`） |
| `_project/` | 初始项目。下划线开头 go 工具不当包；`go.mod` 写成 `go.mod.tmpl`，放出来时去掉后缀 |
| `check.sh` | 题目自带的判分脚本，退出码 0 算过。任务跑完才放到磁盘上，在默认分支的干净副本上跑 |
| `fake.sh`、`fake-home/` | 假模型的做法和假环境，只有 `--fake` 用 |

旧题永久保留，新题只加不改：改题等于改分数的尺子。留出题同样的格式，放 `~/.agentq/evals/heldout/`，不进仓库。
