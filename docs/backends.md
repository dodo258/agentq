# 怎么接一个新后端

agentq 把活交给一个 CLI 去做。CLI 的差别都收在 `internal/backend.Backend` 这个接口后面，调度层只看接口和能力声明。接一个新 CLI 只新增一个包和一行注册，不用改调度层。

## 三步

1. 新建 `internal/backend/<名字>`，实现 `backend.Backend`，在 `init` 里注册：

   ```go
   package mycli

   func init() { backend.Register("mycli", New) }

   func New(o backend.Options) backend.Backend { return &Backend{o: o} }
   ```

2. 在 `internal/app/wire.go` 的 import 里加一行 `_ "agentq/internal/backend/mycli"`。`tests/arch_test.go` 的 `archAllowed` 加这个包，`AGENTS.md` 的模块表加一行。

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

## opencode 的实际情况

**实验。真实的 opencode 没跑过**：下面都是 v1.19.5（2026-10-28）的官方文档和源码写的，官方依据在 `internal/backend/opencode/opencode.go` 开头，事件样本是手写的（`testdata/README.md`）。真实跑过之后才能改成「支持」，同时把样本换成录下来的。

| 能力 | 情况 | 缺了怎么办 |
| --- | --- | --- |
| 无头、事件流 | 有。`run --format json`，提示词走标准输入（没有终端时读 stdin） | |
| 续跑 | 有。`--session=<id>`。每行事件都带 `sessionID`，中途被杀也接得上 | 没等到第一个事件就被杀走接手 |
| 预设会话 id | 无 | 从事件流取 |
| 结构化输出 | 无，`run` 没有这个参数 | 审查门靠提示词要 JSON（`SchemaByPrompt`），不合格重试一次，再不行判不了，不放行。要 `MustSchema` 的任务不启动 |
| 引擎审查 | 有。`ProfileReview` 是 `--auto` 跑在一次性 worktree 里，没有 Codex 那样的操作系统沙箱，能力和 Claude 的审查进程一样 | |
| 用量 | 有，每个 `step_finish` 的 tokens、cost，运行中就有（`UsageStream`） | 金额按它的价目表算，订阅和本地模型是 0：算未知。丢最后一个事件时少最后一步 |
| 美元、回合上限 | 无（agent 的 `steps` 没有 run 参数） | tick 读运行中的用量，超了才停，最多滞后一个 tick |
| 额度查询 | 无（多模型来源） | 只在撞限后冷却 |
| 撞限 | error 事件的 `statusCode=429` 算结构化，`retry-after` 头当重置时间；文字（rate limit、quota 这些）算文字。它自己会先重试，这段时间事件流没动静 | 模型来源多，认不出就落到普通失败和熔断 |
| 认证 | 查不了：`auth list` 只列存下的凭据，配置里的和本地模型看不到，空的不等于没登录。`ProviderAuthError`、401 由 `Parse` 认出 | 照常启动，第一次失败暂停到 `limit_cooldown` |
| 终态 | 没有单独的事件：最后的 `step_finish` 的 `reason` 不是 `tool-calls`、`unknown`（它自己的循环也这么判），或者 `error` 事件 | 官方记过丢最后的 `step_finish` 还退 0（#26855、#53207，后者 2026-10-30 关）：当没有终态，不算成功，条目还进行中就走中断续跑 |
| 只读、无工具、隔离 MCP | 没声明。agent 的 `permission` 是 CLI 强制的（`--agent`），`--auto` 下明确的 deny 还生效（官方），但没真实跑过，这些是安全边界，不靠文档开 | 大方向商量不问，追问回退普通对话，巡逻不分析 |
| 工具调用记录 | 有，事件自带毫秒时间戳。`run` 把子代理的事件滤掉了 | 「子代理失控」看不到 |
| 进程标记 | 没放进命令行：npm 版是 node 外壳再起原生程序，`--title` 放得下，但原生程序改不改进程名没验证。靠 `AGENTQ_JOB` | 见「认进程」 |
| 技能 | `~/.agents/skills`，和 Codex 共用一份（官方也读 `~/.config/opencode/skills`、`~/.claude/skills`） | |
| 用户级规则 | `~/.config/opencode/AGENTS.md`，块方式。这个文件存在时它不再读 `~/.claude/CLAUDE.md`（官方：每一类第一个算） | |
| 权限 | `permission_mode` 只有 `default`、`auto`。不加 `--auto` 时要批准的动作（工作目录外、`.env`）一律被拒，读不到技能，所以要 `auto` | 没配不起能写文件的任务 |
| API Key | 各模型来源的环境变量不传（`envKeep` 只有 `OPENCODE_CONFIG`、`OPENCODE_CONFIG_DIR`、`OPENCODE_DB`），用 `opencode auth login` 存的凭据。`backends.<名字>.env` 还没做 | 只靠环境变量配 Key 的用法跑不起来 |
| 目录 | `XDG_*` 不传：外壳和 git 走同一份白名单，放了 git 就改读 `$XDG_CONFIG_HOME/git/config`，老用户行为会变 | 改过 `XDG_DATA_HOME`、`XDG_CONFIG_HOME` 的用户，后台任务里 opencode 用默认目录，凭据要在 `~/.local/share/opencode` |

`--variant` 对没有这档的模型是什么结果没查到：审查固定传 `medium`，真实验收时要看。

## 测试

`testkit.RegisterFake` 注册一个能力可调的假后端，`tests/backend_fake_test.go` 用它覆盖每条降级路径。新后端不用再测这些，只测自己的参数和解析。

默认测试只用假 CLI。真实 CLI 的验收要显式打开，在新的临时 HOME 和临时 Git 项目里跑：Claude 是 `AGENTQ_REAL_E2E=1`，Codex 是 `AGENTQ_REAL_CODEX=1 go test -count=1 -v -run '^TestRealCodex' .`，用已有登录（`CODEX_HOME`），不读用户的 `config.toml` 和钩子，CI 不跑。
