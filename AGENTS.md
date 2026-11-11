# 项目地图与 Agent 维护合同

## 项目是什么
agentq：一句话交代项目任务，由后台调度器把 `projects_root/*/docs/BACKLOG.md`（加 `extra_projects` 登记的项目）「待做」交给独立 Claude CLI，按归仓 agent-task 技能做到分支可合并，引擎过门（测试、审查）后合并并写交接。CLI 自然语言会话与 agentq 管理命令分开，Telegram 可选。

## 技术栈与目录
Go 标准库，单二进制，分模块见下一节。平台差异只在 `*_darwin.go`、`*_linux.go`（服务管理在 `internal/service/`，systemd 那部分不带标签）；macOS 走 launchd，Linux 走 `systemd --user`。`skills/` 自有技能唯一源，`rules/` 通用工程规则（`core.md` 常驻，不超过 4KB，语言文件带 `paths`），`evals/` 评测题（编译进二进制，只加不改），`templates/` 五模板和 `gates/` 门脚本示例，`support/` 支持文件，`dependencies/claude.lock.json` 第三方来源/commit/许可证/路径声明。运行数据在 `~/.agentq/`，每日汇总 `~/.claude/nightly/<日期>-agentq.md`。`prompts/` 定时入口编译进二进制（根目录 `assets.go`），review 委托唯一技能，night 保持现有调度开关，不从桌面任务反向覆盖源码。

## 模块
一个 Go module，`main` 只有入口（`main.go`）和编译进二进制的资产（`assets.go`），其余都在 `internal/<模块>`。依赖不往上层走，同层依赖也要表里写明。`TestArchImports` 按 `tests/arch_test.go` 的 `archAllowed` 查（产品代码和测试都查），根目录也查，`tests/` 由 `TestArchTestsDir` 查。加模块、加依赖先改这张表。

| 层 | 模块 | 负责 |
|---|---|---|
| 0 | `textfmt` `fsx` `proc` `backend` | 给人看的短文字；原子写和文件锁；子进程环境白名单和进程组；后端接口、注册表、共用类型（用量、额度档位、进展信号） |
| 1 | `config` `env` `backend/claude` `backend/codex` `backend/pi` `gitx` | config.json；运行环境（路径、时钟、日志、取后端）；Claude Code 后端（参数、结果解析、额度、会话记录、登录）；Codex 后端（参数、事件流解析、登录）；pi 后端（参数、事件流解析、登录）；跑 git |
| 2 | `state` `backlog` `telegram` | state.json 和 Job；BACKLOG 文本和提交；Telegram 客户端 |
| 3 | `quality` `ledger` `service` `envlayer` `notify` | 满意率；meta.json 和当天汇总；launchd、systemd；通用工程规则（`rules/`）放到后端的规则目录；通知队列、早报 |
| 4 | `gate` `runner` | 合并前的质量门和引擎合并（门队列、`_gate` 进程、收尾，加门走 `gate.Register`；`agentq health`）；任务从启动到收尾（任务外壳、续跑、超时、熔断、预算、额度门，功能走 `runner.Hooks`） |
| 5 | `orphan` `discuss` `newproj` `suggest` `routine` `patrol` `evolve` | 孤儿条目；开工前商量大方向；`agentq new`（用 `discuss`）；「## 建议」的编号、回「做 N」「不做 N」和不做清单；夜班和复盘；巡逻；评测集和判分（`agentq eval`，tick 不调用），自我进化的信号、学到的约束、提案和自己长大（自身问题进 `self_project` 待做、空闲时升级自己、观察期变差回滚；`agentq evolve`，tick 每小时收，不调模型；用 `suggest` 的建议编号和不做清单，只读 `gate` 的结果类型；装和回滚只经子进程调安装引擎） |
| 6 | `chat` `stats` | 用户消息（长轮询、Fast Path、对话任务、回复链）；任务统计 |
| 7 | `sched` `setup` `onboard` | 一次 tick 的固定流程和磁盘清理；安装引擎；`init`、`add`、`start`、`env` 和接手时判断检查命令 |
| 8 | `app` | 建每个模块的唯一实例、接钩子；`status`、`run`、`stop`、`logs`、`notify`、`backlog archive` |

- 只给测试 import：`testkit`（`GitT`、`MkProject` 这类夹具）、`testkit/stack`（建到 `runner` 为止的一套真实例，不接钩子）。
- 接新后端见 `docs/backends.md`：新增一个包，`app` 加一行 import。后端实现只有 `app` 能 import（注册），其余都走 `backend.Backend`；各家 CLI 的参数、结果格式、会话文件位置只能写在自己的包里（`TestArchBackendImpls`、`TestArchClaudeFlagsOnlyInBackend`，Codex、pi 也查）。
- 每个模块的实例类型都叫 `Service`，匿名嵌入 `*env.Env`，依赖是具名字段；只有 `internal/app` 能建（`TestArchSingletons`）。`app.Wire` 最后四行的顺序固定，`Hooks` 最后（`TestArchWireOrder`）。
- 单独测一块：`go test -race ./internal/<模块>`。跑真二进制的行为用例在 `tests/`（`go test -race ./tests`，约 5 分钟，`TestMain` 把工作目录切到仓库根；黄金文件在 `tests/testdata/`）；要走到钩子或多个功能的用例放 `tests/` 走导出 API。
- 反复出问题的模块重写时接口尽量不变，要变就写进提交说明。`Job`、`NotifyState` 仍是共享数据，改字段要看所有读写方，落盘格式由 `tests/testdata/` 的黄金文件固定。

## 常用命令
- 检查：`test -z "$(git ls-files '*.go' | xargs gofmt -l)" && go vet ./... && go test -race ./...`
- 一键安装/升级：`sh install.sh`，已装的用 `agentq update`（Release 附件的那份和程序都由 `scripts/build-release.sh <版本> <输出目录> <owner/repo>` 填入仓库名）。源码安装：`sh scripts/bootstrap.sh --yes`；受管安装中 `scripts/update.sh` 复用同一引擎。候选临时 HOME 自检、比较冲突、事务备份和完整代回滚见 `docs/install.zh-CN.md` / `docs/install.en.md`。新安装默认暂停，保留已有配置/开关。
- 服务明确选择：`--services` / `--listen` / `--backup` / `--watchdog`；`agentq start` 只在 Telegram 配好时带上 listen。卸载 `agentq remove --yes`，恢复 `sh scripts/bootstrap.sh restore --yes`。旧 binary-only 操作仅兼容，见 `docs/legacy-install.md`。
- 网络入口验收：`AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...`。
- 真实 systemd 验收：只在 CI 的 Linux runner 上，见 `.github/workflows/ci.yml` 的 `TestSystemdAcceptance`；本机不跑（动真实用户服务）。
- 评测：`agentq eval run` 只跑程序题，`--fake` 用假 CLI 走完全部题，`--real` 才调模型（周额度 ≥85% 拒绝），见 `docs/reference/commands.md`。
- 真实 pi 验收：`AGENTQ_REAL_PI=1 go test -count=1 -v -run '^TestRealPi' ./tests`，花本机 pi 配的模型额度。
- 真实 CLI 验收：`AGENTQ_REAL_E2E=1 go test -count=1 -v -run '^TestRealClaudeInstallationE2E$' ./...`；会消耗模型额度，连接型身份最小投影需另显式 `AGENTQ_REAL_E2E_CONNECTION=1`。

## 约定
- 走直路：先找最简单能达到目的的做法。能删的不加，现有机制够用就不新建模块、配置项、命令；新加一个就说明为什么现有的不够。交付说明写一句「有没有更简单的做法、为什么没用」。
- 清洁源边界见 `config/clean-source.json` 和 `docs/privacy.md`。通用源不放个人绑定；本机规则放仓库外的 `~/.agentq/private/`。提交前扫描完整索引，测试内含门禁。公开候选用 `scripts/export-public.sh <输出目录> <owner/repo>` 导出。
- 默认测试只用临时 HOME、临时 projects_root、假 claude/launchctl/systemctl，不读写真实的 `~/.agentq`、`~/projects`、`~/.claude`。真实 CLI 仅显式 opt-in，在新临时 HOME/临时 Git 项目内；身份由授权调用环境提供，不复制旧配置/缓存，不把真实产品当测试夹具。
- 状态文件读不了当未知：不收尾不启动，只记日志。内容损坏时 tick 备份坏文件、按 jobs/*/meta.json 重建并收尾，那次不启动新任务。
- 传给 CLI 的环境变量：公共部分在 `internal/proc`，每个后端在 `EnvKeep` 声明自己的，别家的不传（`proc.BackendEnv`）；新加变量说明必要性，不隐式切换收费入口。

## Agent 维护合同
- 可在任务源码 worktree 修改 Go/skills/support/templates/prompts，亲自验证、自检、构建候选，按项目流程审查合并；源码是唯一维护来源，已安装 generation 不是手改源。
- 完整实现与自验后开一个 medium 同步独立 check。审查代理亲自看 diff、覆盖各维度并验证，不派生 Agent/Workflow；最多第二轮只复验确认的实质修正。只处理真实影响功能/安全/数据/隐私或违反项目规则的问题，没有就通过。
- 经覆盖实际目标的授权后使用同一安装引擎升级。不就地覆盖运行中程序，不丢状态/身份/用户配置，不借维护绕过权限、认证、发布或远程授权。活跃任务、未知状态、冲突时停并保留材料。
- 用户/项目定制与厂商资产分层；第三方锁定声明和许可证随安装保留，不复制 vendor/cache/身份。未知同名技能不能替用户删重；第三方 Hook/状态栏/settings 保留，受管入口或 generation 被改时报告冲突。
- `agentq doctor` 检查，`agentq rollback --yes` 恢复上一完整代，`restore` 撤回中断事务或恢复卸载；任务/秘密/用户配置不随代码回滚。没有上代、用户修改或未知状态时如实报告，不能宣称已恢复。

## 进度与决定
用户能感知的变化写 `CHANGELOG.md`，取舍和验证结果写提交说明。
