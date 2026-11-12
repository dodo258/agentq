# 怎么接一个新后端

agentq 把活交给一个 CLI 去做。CLI 的差别都收在 `internal/backend.Backend` 这个接口后面，调度层只看接口和能力声明。接一个新 CLI 只新增一个包和一行注册，不用改调度层。

## 三步

1. 新建 `internal/backend/<名字>`，实现 `backend.Backend`，在 `init` 里注册：

   ```go
   package mycli

   func init() { backend.Register("mycli", New) }

   func New(o backend.Options) backend.Backend { return &Backend{o: o} }
   ```

2. 在 `internal/app/wire.go` 的 import 里加一行 `_ "agentq/internal/backend/mycli"`。`tests/arch_test.go` 的 `archAllowed` 加这个包，`AGENTS.md` 的模块表加一行。`install.sh` 的 `check_backend` 认这个名字；它在 `internal/backend/registry.go` 的 `later`（暂不支持的后端）里就删掉。

3. 写测试：假 CLI 脚本加录下来的输出样本，覆盖成功、报错、撞限、截断、丢最后事件。

用户在 `~/.agentq/config.json` 写 `"backend": "mycli"` 就切过去了。写了不认识的名字不会回退到别的后端：任务不启动，`agentq status` 和 `agentq doctor` 说明原因。按项目换用 `"project_backends": {"项目目录名": "mycli"}`。

每个后端自己的配置放 `"backends": {"mycli": {"path": "", "model": "", "effort": "", "permission": ""}}`。顶层的 `model`、`effort`、`permission_mode` 只归默认后端，别的后端看不到（`Config.ForBackend`）。默认后端不是 Claude 又没写这些键时它们是空的，不沿用 Claude 的 `opus`。空的不传，跟随 CLI 自己的设置。路径没配就按后端名在 PATH 里找。

## 接口

| 方法 | 做什么 | 注意 |
| --- | --- | --- |
| `Name` | 注册名 | 也是配置和 `Job.Backend` 的值 |
| `Caps` | 声明能力 | 做不到就写假，调度层按它降级，见下一节 |
| `EnvKeep` | 这个 CLI 要的登录和配置变量 | 只放登录位置。API Key 这类会换收费入口的不放，别家的变量也不会传给它 |
| `Probe` | `doctor` 和安装用：路径、版本、无头参数在不在、认证 | 按 `ProbeEnv.Env` 查，不用当前 shell |
| `Auth` | 启动前的登录检查 | 查不了就返回 `AuthUnknown`，照常启动。第一次因认证失败时 `Parse` 给 `StopAuth`，agentq 暂停这个后端并通知用户 |
| `Build` | 把 `Request` 翻译成参数 | `Start` 启动前已经统一查过 `Must`，直接调 `Build` 的地方（比如测试）要自己调 `backend.CheckMust`。`Model`、`Effort` 为空就不传。`Label` 放进命令行，收尾时靠它认进程；CLI 没有地方放的声明 `Caps.NoArgvLabel`，见下面。提示词走标准输入的放 `Invocation.Stdin`。要给 CLI 传文件的放 `Request.WorkDir` |
| `Parse` | 读任务目录的 `result.json`（CLI 的标准输出）和 `stderr.log` | 没读到终态时 `HaveTerminal=false`，不要猜成功。撞限只看错误通道，不扫模型正文 |
| `LastActivity` | 最后一次有动静的时刻，判断疑似卡死 | 优先读 agentq 保存的输出，不读厂商内部文件 |
| `CanResume` | 这个会话还能接上吗 | 回复链用 |
| `RecordedModel` | 从 `Job.Args` 认模型 | 统计用，认不出返回空 |

可选接口，实现了才算有：

- `QuotaReader`：不调模型读额度。没有就当未知，照常跑，只靠撞限后冷却。
- `IdentityLinker`：在别的 HOME 里运行时，指回真实家目录哪些路径就能用上本机的登录。`agentq eval --real` 用，没有就不能 `--real`。只放软链接；写的时候会被原子替换的文件不能放，替换掉链接后真实家目录那份就旧了。
- `ToolLog`：读工具调用，`Name` 要映射成规范名（Edit、Write、Bash、Agent 这些）。没有就「看进展」记未知，只留疑似卡死。
- `UsageStream`：运行中读用量。没有美元硬上限的后端靠它在 tick 里停任务，滞后最多 5 分钟。

- `Permissions`：`permission_mode` 的合法值、无人值守要用的值、同意时多说的话。没有就不查写法。
- `SkillHome`：用户级技能目录。安装引擎投影到这里，提示词里的 `~/.claude/skills/` 换成它。没有就当 Claude 的目录。
- `GlobalRules`：用户级规则目录，环境层放文件。
- `RulesBlock`：没有规则目录、只有一个用户级说明文件。环境层在里面放一段带标记的块，改前备份，关掉和卸载只删这一段，别的字节不动。
- `CLISetup`：带用户装 CLI、登录。没有就只报错，告诉用户官方命令。
- `ResumeHint`：用户想自己接着看这个会话时运行的命令，`agentq logs` 里说。没有就只写会话 id。

输出没有时间戳的，用 `backend.NoteSeen`、`SeenAt`：tick 时记下文件大小和时刻，事件时间取首次被看到的时刻。

### 认进程

收尾、stop、超时都要先确认记下的 pid 还是这个任务的，认错了不是误杀就是重复启动。

- 默认看命令行里的 `Label`，认不出就是结束了或 pid 被复用。
- `Caps.NoArgvLabel` 的后端看环境里的 `AGENTQ_JOB`。环境读得到却没有标记，是 pid 被复用了。macOS 上系统自带的程序读不到环境，这时看任务外壳：外壳在等这个 pid，pid 就不可能被复用。外壳也不在就是认不出：不杀，也不当结束了再起一个，占着名额到超时，超时后只收尾，`status` 说清楚。

### 权限没配

后端声明了 `Permissions`，权限又是空的或 `default`：能写文件的任务不起进程、不计连环失败，tick 日志同样的原因只记一次，`status`、`doctor` 一直写明要设什么（默认后端是 `permission_mode`，项目用的后端是 `backends.<名字>.permission`），`agentq run` 也拒绝。只读的追问和审查不受影响。默认后端走 `agentq start` 同意时会写好。默认后端是 Claude 时不查，老配置行为不变。

`Auth` 只看本地有没有凭据、不验证有效的，返回 `Unverified: true`。这样的「已登录」不解除认证失败的暂停。

## 能力缺了会怎样

`agentq doctor` 会打出当前后端的能力表和关掉的任务。规则：

- **`Must` 做不到，任务不启动，如实说原因。** 追问、巡逻分析、大方向商量靠 CLI 强制的只读或无工具边界保证安全，换成靠提示词约束是真的变弱，所以不补。
- **界面门的模型部分要 `Caps.ImageInput`。** 截图路径放在 `Request.Images`，也在 `AddDirs` 里。Claude 靠读文件看，Codex 在 `Build` 里转成 `--image=`（路径带逗号传不对，返回 `ErrUnsupported`）。做不到的后端界面门判不了，不放行，不起进程。
- **合并前的独立审查是硬前提，由引擎起进程。** 审查门走 `Build`（`ProfileReview`、`Schema`、`Effort=medium`、候选 worktree 作 `Dir`），不靠 CLI 有子代理，结论只记在 `gate/reviews/<项目>/<tree>.json`（tree、提交、patch-id、任务、后端）。起不了（`Caps.ReviewProfile` 为假，或 `Caps.Schema`、`Caps.SchemaByPrompt` 都为假）的后端，待做和 `new` 不接活。`SchemaByPrompt` 是 CLI 不强制但最后一条消息取得到：提示词要 JSON，校验不过重试一次，再不行判不了，不放行。
- **其余的降级，功能变弱但能跑。** 没有续跑走接手，没有美元上限靠超时，没有用量写未知。

## Codex 的实际情况

版本 codex-cli 0.166.0。官方依据写在 `internal/backend/codex/codex.go` 开头，下面标「实测」的是真实跑出来的。

| 能力 | 情况 | 缺了怎么办 |
| --- | --- | --- |
| 无头、事件流 | 有。`exec --json`，提示词走标准输入 | |
| 续跑 | 有。`exec resume <id>`。id 从第一个事件取，中途被杀也接得上（实测） | |
| 预设会话 id | 无 | 从事件流取 |
| 结构化输出 | 有。`--output-schema`。要严格 schema，不严格接口直接 400（实测），`Build` 时转 | |
| 引擎审查 | 有。`ProfileReview` 用 `workspace-write` 沙箱：能跑测试，`.git` 只读提交不了（实测），默认不联网 | 跑不了要联网的测试时模型判 hint 或 unknown，unknown 不放行 |
| 用量 | 只有 token，每次 exec 是本次的 | 金额写未知，合计注明 |
| 美元、回合上限 | 无 | 只有超时兜底，运行中也看不到用量 |
| 额度查询 | 无（`app-server` 的接口标 experimental，没接） | 只在撞限后冷却 |
| 撞限 | 只有文字（`turn.failed`、stderr），不扫模型正文 | 厂商改文案就认不出，落到普通失败和熔断 |
| 认证 | `login status` 只看本地凭据（实测令牌坏了也说已登录），`Unverified`。401 由 `Parse` 认出 | 暂停到 `limit_cooldown`，删暂停记录提前恢复 |
| 只读、无工具 | 无。`read-only` 只禁写不限读 | 大方向商量不问、按默认开工，追问回退普通对话（按原任务的后端判断，别的后端的任务照常追问），巡逻不分析 |
| 隔离 MCP | 没声明。`--ignore-user-config` 丢整份配置，不算隔离 | 同上 |
| 工具调用记录 | 有，读 agentq 保存的事件流，时间误差最多一个 tick | 子代理在同一条事件流里，「子代理失控」看不到 |
| 进程标记 | 命令行放不了：`-c` 写不认识的键每次多两条提示事件（实测）。靠 `AGENTQ_JOB`，原生程序读得到（实测） | 见「认进程」 |
| 技能 | `~/.agents/skills`（官方列的用户级位置，没有 `~/.codex/skills`） | |
| 用户级规则 | 只有 `$CODEX_HOME/AGENTS.md`，`AGENTS.override.md` 存在时不读它（官方）。块里只放 `core.md`，语言规则靠 `paths` 做不到 | `AGENTS.override.md` 存在时如实说 |

**它会改自己的配置。** 可写沙箱下 `codex exec` 把项目目录写进 `$CODEX_HOME/config.toml`（`trust_level = "trusted"`），`--ignore-user-config` 也一样（实测）。这是 Codex 自己的行为，agentq 不清，也就是每个跑过任务的项目（新项目在 `projects_root`）都会留一段。

**钩子会一直花额度。** 用户配的 Stop 钩子报错时会拦下结束，模型被反复叫回来（实测 3 分钟 50 多条消息），没有回合上限兜底，只有 `timeout`。agentq 不改用户的钩子，确认钩子在后台环境里能跑。

## pi 的实际情况

版本 pi 1.5.1（npm 包 `@earendil-works/pi-coding-agent`，MIT）。依据是包里自带的官方文档（`docs/cli.md`、`cli-integration.md`、`json.md`、`sessions.md`、`security.md`、`configuration.md`、`environment-variables.md`、`skills.md`），写在 `internal/backend/pi/pi.go` 开头；下面标「实测」的是 macOS 上真实跑出来的（模型来源是本机 pi 配的 OpenAI 兼容接口，失败样本用真实 pi 对本地假接口跑出）。

| 能力 | 情况 | 缺了怎么办 |
| --- | --- | --- |
| 无头、事件流 | 有。`pi -p --mode json`，第一行 session 头，之后一行一个事件，`agent_settled` 是终态。提示词走标准输入（实测） | |
| 失败的判断 | 失败、截断、中止只在事件里：最后一条助手消息的 `stopReason`（`error`、`length`、`aborted`）和 `agent_settled.aborted`。退出码是 0（实测 401、429、500、截断都退 0），`Parse` 不看退出码 | |
| 续跑 | 有。`--session <id>`，SIGTERM 之后接得上（实测：pi 退 143，被打断的回答记成 aborted，续跑照常） | |
| 预设会话 id | 有。`--session-id <id>` 不存在就建（实测） | |
| 结构化输出 | 无 | 审查门靠提示词要 JSON，只认最后一条消息整条是 JSON（可以包在一个代码块里），不合格重试一次，再不行判不了，不放行 |
| 引擎审查 | 有。审查进程带 `--no-approve`：候选分支上的 `.pi` 是任务写的，不在审查进程里加载 | pi 没有沙箱，审查进程能做的和 Claude 的审查进程一样 |
| 用量和金额 | 有。每条助手消息的 token 和金额加起来；金额是 pi 按模型价目表算的，价目表里没有这个模型时一直是 0，记成未知 | |
| 美元、回合上限 | 无 | 运行中读事件流里的用量（`UsageStream`），tick 超预算就停，最多滞后 5 分钟；回合上限只有超时兜底 |
| 额度查询 | 无 | 只在撞限后冷却 |
| 撞限 | 有。上游错误写成「状态码: 原文」（实测 429），只看错误通道，不看模型正文 | 别的写法认不出时落到普通失败和熔断 |
| 认证 | `pi auth check --provider <默认来源> --json`；配置的模型写成 `提供商/模型` 时改查 `pi auth check --model <模型> --json`，由 pi 按模型认来源（实测 `qwen/qwen3-coder` 认成 openrouter）。只写模型名时模糊匹配查到的未必是运行时选的那家（实测 `sonnet` 查到 amazon-bedrock），仍查默认来源。只说明本地配了凭据（API key 不验证），`Unverified`。401（实测）和没有凭据时的 `No API key found`（实测，退 1，只有 session 头）由 `Parse` 认出 | 暂停到 `limit_cooldown`，删暂停记录提前恢复 |
| 只读、无工具 | 无。`--tools read,grep,find,ls` 和 `--no-tools` 确实只给模型这些工具，但 `read` 能读工作目录外的任何文件（实测），读密钥挡不住 | 大方向商量不问、按默认开工，追问回退普通对话，巡逻不分析 |
| 隔离 MCP | 没声明。要它的任务同时要只读工具，pi 给不了，声明了也用不上；`--no-mcp` 加 `--no-extensions` 能不能完全隔离没实测 | 同上 |
| 图片输入 | 有。`@路径` 附在第一条消息上，路径带逗号也行（实测）。模型本身不收图片时看不到 | |
| 工具调用记录 | 有，读 agentq 保存的事件流（`tool_execution_start`），时间误差最多一个 tick | pi 没有子代理，「子代理失控」不适用 |
| 进程标记 | 认不出。pi 启动就把进程名改成 `pi`，macOS 上 `ps` 连命令行带环境都读不到（实测，普通 node 进程读得到）。`--name` 只当会话显示名 | 靠任务外壳认，见「认进程」 |
| 权限 | pi 没有权限确认，也没有沙箱，工具以本机用户身份直接运行（官方 `security.md`）。`permission_mode` 只有 `default`、`full`，`full` 只记用户在 `agentq start` 确认过，不变成参数 | |
| 项目信任 | 任务不传 `--approve`/`--no-approve`，跟随用户在 pi 里保存的决定（`/trust`）；没保存过就按 `defaultProjectTrust`，默认跳过项目 `.pi` 里的扩展和设置（官方，实测） | |
| 技能 | `~/.agents/skills`（官方，和 Codex 共用一份；实测加载） | |
| 用户级规则 | 配置目录里第一个存在的 `AGENTS.override.md`、`AGENTS.md`、`CLAUDE.md`（源码）。块放进用户已有的 `AGENTS.md` 或 `CLAUDE.md`，都没有才建 `AGENTS.md`；`AGENTS.override.md` 存在时如实说读不到 | |
| 模型和强度 | 跟随 pi 自己的 `defaultProvider`、`defaultModel`，`backends.pi.model` 覆盖（`--model`，可写 `提供商/模型`），强度映射到 `--thinking`，模型做不到的 pi 自己往下压 | |
| 环境变量 | 只传 `PI_CODING_AGENT_DIR`、`PI_CODING_AGENT_SESSION_DIR`、`PI_PACKAGE_DIR`。API Key 不传：凭据要存在 pi 的 `auth.json` 或 `models.json` 里，靠环境变量给 Key 的来源在后台会报没有凭据 | |
| 带你安装和登录 | 无 | 缺 CLI 或没选模型来源时报错，给官方命令 |

pi 要 `node` 在后台环境的 PATH 里（npm 装的是 `#!/usr/bin/env node` 脚本）。

## opencode

opencode 暂不支持，以后再加：删除前最后一版后端在提交 `066111f`（`git show 066111f:internal/backend/opencode/opencode.go`），当时依据的官方文档和源码写在那个文件开头。

## 测试

`testkit.RegisterFake` 注册一个能力可调的假后端，`tests/backend_fake_test.go` 用它覆盖每条降级路径。新后端不用再测这些，只测自己的参数和解析。

默认测试只用假 CLI。真实 CLI 的验收要显式打开，在新的临时 HOME 和临时 Git 项目里跑：Claude 是 `AGENTQ_REAL_E2E=1`，Codex 是 `AGENTQ_REAL_CODEX=1 go test -count=1 -v -run '^TestRealCodex' .`，用已有登录（`CODEX_HOME`），不读用户的 `config.toml` 和钩子；pi 是 `AGENTQ_REAL_PI=1 go test -count=1 -v -run '^TestRealPi' ./tests`，临时 HOME 里只软链接回 `~/.pi/agent` 的凭据和模型设置，会话、信任记录、扩展都在临时目录。CI 都不跑。
