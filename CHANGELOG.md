# 更新记录

只写用得到的变化。

## 未发布

- 任务做到一半撞上游瞬时错误（5xx、422、Cloudflare 拦截页「403 Attention Required」这类网关挑战页）时，不再记失败、把条目和 worktree 里的改动晾到孤儿处理的 24 小时窗口：后端能续会话就记中断，下一次 tick 用同一会话续跑（和被 SIGTERM 打断同一条路），连续最多 2 次（和短退避重试共用上限），用完再按失败。挑战页只按结构化状态码加 API 错误正文认，不看模型正文；真正的认证失败（401、令牌失效）照旧暂停后端、不续跑。新加评测题 `028-upstream-midway-resume`。

## v0.3.0

### 像产品经理一样想下一步

- 产品经理调研（`pm_enabled`，默认开）：项目第一版合并后、之后有新任务合并且「待做」清空时，起一个只读联网的调研任务，看同类最新做法和 GitHub issues、讨论区里的用户抱怨，结合项目 README、已完成的条目、门退回和中断、`docs/NOT-DOING.md`，写 1–3 条下一步进「## 建议」，每条带出处和为什么值得做，推一条；回「做 N」移进待做，「不做 N」进不做清单。同一项目 14 天最多一次；它上次提的还没回、额度到后台上限、项目被排除、`self_project`、后端做不到只读联网（Codex、pi）时不起。不写待做、不改代码。新加评测题 `027-pm-after-merge`。
- 开工前商量大方向时先只读看一眼真实环境（条目看项目目录，`agentq new` 看 projects_root 下的项目），问题和默认答案按看到的事实定、每个带一句依据；用法（命令行、窗口界面、菜单栏）没说就问；会删改用户文件或数据的，默认写明开发测试只用临时目录造的数据、真实数据只读跑。没配 Telegram 直接用默认的 `agentq new` 也先这样看再定默认。商量进程从「无工具」改成 CLI 强制的只读（只能 Read、Grep、Glob），回合上限 15、预算 1.5 美元。

### 门和合并更稳

- 巡逻不再把「门没过、退回第 N 轮」的任务报成「任务中断」：认门记录上的退回状态，不报、不起模型分析、不留在 `patrol.json`；续跑不了又没人接的照常写接手待做。真正中断（没有门退回记录）的照旧报。新加评测题 `026-patrol-gate-return-not-anomaly`。
- 门合并时主目录在检查之后、`git merge` 之前被切到别的分支：以前快进的是那条分支却记成已合并，现在合完核对提交进没进默认分支，没进就当没合、等主目录切回来再合。
- 卡片或命令里的「放行、再审、退回、放弃」先存进 `state.json` 再删决定文件：以前先删，这几秒里 tick 被杀，用户的决定就丢了。
- 任务改了测试（删测试文件或用例、测试函数改名、加跳过）或动了 `.agentq/`、提交说明里却没写 `门说明：` 时，交了 `ready.json` 不再直接进门被测试门退回、白占一轮：引擎进门前先查，缺了用同一会话续一轮只补说明，不算退回轮次；补了还缺照常进门。
- 主分支锁：持有进程已死的旧锁，几个等锁的同时看到时，只由一个清掉（和 `with-lock.sh` 同一协议：先建 `.<名字>.reap`、在里面重看一遍）；以前会先后删掉别人刚占上的新锁，两边同时改主目录。释放也只删自己的锁。
- 安装、升级、回滚、恢复在门还在跑时也等：任务记成 done、门还在过时，以前判空闲照换安装，门跑完还会接新活。排队的门不挡。状态文件是 `{}` 这类不是状态文件的内容时，和调度器一样当损坏、不换。新加评测题 `023-setup-waits-gate`。

### 其他

- pi 后端：`backends.pi.model` 写成 `提供商/模型` 时，启动前的登录检查按这个模型查（`pi auth check --model`），不再查默认来源；登录提示也说对应来源。新加评测题 `025-pi-auth-follows-model`。
- 用量冷却和额度一律按后端存（`state.json` 的 `backend_limits`），默认后端也一样。把默认后端从 Claude 换成 Codex（常见于 Claude 撞了限）后，Codex 不再被 Claude 的冷却挡到重置，还指定 Claude 的项目也不会读到空冷却再撞一次限。老 `state.json` 顶层的 `cooldown_until`、`usage` 第一次读时迁到写它时的默认后端（取同目录 `config.json` 的 `backend`）；`config.json` 在它之后改过就迁不清楚，丢掉，最多再撞一次限。回滚到老版本时默认后端的冷却读不到，同样最多再撞一次限。新加评测题 `024-cooldown-follows-backend`。
- 每周漏洞检查没查成（没装 `govulncheck`、没网）时隔一天就再试，装上后 `agentq status` 第二天就更新，不用等一周。

## v0.2.0

### 可以用 pi 当后端

- 新装时 `install.sh --backend pi`（或 `agentq setup --backend pi`）选 pi；已有配置写 `"backend": "pi"`，或按项目 `"project_backends": {"项目": "pi"}`。技能装进 `~/.agents/skills`，和 Codex 共用。
- 模型和提供商跟随 pi 自己的 `defaultProvider`、`defaultModel`，`backends.pi.model` 可覆盖（可写 `提供商/模型`），`effort` 对应 pi 的 `--thinking`。凭据要存在 pi 的 `auth.json` 或 `models.json` 里：agentq 不把 API Key 环境变量传给后台任务。
- pi 没有权限确认，也没有沙箱，读写文件、跑命令都以你的身份直接进行。`agentq start` 会先如实说明再问你，同意后写 `permission_mode: "full"`（只表示你确认过）。
- 做不到的如实降级：只读和无工具挡不住读工作目录外的文件，所以大方向商量不问、追问回退成普通对话、巡逻不起分析；没有美元上限，tick 按运行中的用量停任务；没有强制结构化输出，审查门靠提示词要 JSON，给不出就判不了、不合并。`agentq doctor` 列出能力表。
- 审查进程不加载候选分支上项目 `.pi` 里的扩展和设置；任务跟随你在 pi 里保存的项目信任。
- 审查前换回基点版本的指令文件（`AGENTS.md`、`CLAUDE.md`、`.claude/` 等）不分大小写：pi 也读 `AGENTS.MD`，macOS 上 `agents.md` 就是 `AGENTS.md`，任务新加的这类变体以前会漏过、被审查进程读到。

### 下掉 opencode

- 下掉了从没真实跑过的 opencode 后端，其他 CLI 以后再加（接法见 `docs/backends.md`）。
- `install.sh --backend opencode`、`agentq setup --backend opencode` 直接说「opencode 暂不支持，以后再加」并退出，什么都不装；`install.sh` 在下载之前就拦下，`--backend` 写了别的名字也一样。
- 配置里 `backend` 或 `project_backends` 写了 opencode 的：不接活，不换成别的后端，`agentq doctor` 报「opencode 暂不支持，以后再加」，改成 `claude`、`codex` 或 `pi` 就好。

### 发现自己不会什么，长出成套能力（自我进化 E7）

- 返工、门 `error` 档没过（按门点名的文件）、超时、卡在门上都按领域记账（前端、后端、设计、文档……，中英文一张表）。某领域近 30 天问题 3 次以上、问题率 25% 以上且至少是其他领域的 2 倍，写一条带证据的「能力缺口：<领域>」，走自我派活，同一领域只写一次；这一条合并后这个领域清零。
- 能力清单 `internal/gate/capabilities/capabilities.json` 登记每项能力（名字、领域、知识文件、检查、考题、启用条件、日期）。新的门 `capability`（默认 `warn`）按启用条件跑能力的检查，记下有没有帮上忙。
- `ready.json` 的 `type` 可以写 `capability`：评测门要求登记了新能力、新加了同领域的考题，新题在运行中这一代和候选上各跑一次，这个领域涨分、别的领域不掉才合并。
- 一项能力 60 天没帮上忙，或检查报的问题一半以上被放过，写一条「撤掉能力：<名字>」，不自动删文件。
- `agentq evolve signals` 多出按领域的账、缺口和能力清单。

### 发行版可以自己升级（`auto_update`，默认关）

- 在 `~/.agentq/config.json` 写 `"auto_update": true` 打开。之后每天最多看一次发布地址，有更新的版本就在没有任务在跑、在过门时照 `agentq update` 装上，安装时用过的 `--proxy`、`--mirror` 照样用；装上后 3 个任务比之前差就回滚到上一代、推通知，这个版本以后不再自动装。下载或校验不过不装，同一个版本只推一次。关着时不联网，和以前一样。源码构建、配了 `self_project` 的机器不管。详见 `docs/reference/commands.md` 的「自动更新」。
- `agentq status` 多一行「自动更新」（关着时收进「没开」）。
- `agentq update --check` 只看发布上的最新版本（对过校验）；发布里多一个 `VERSION` 文件，也写进 `SHA256SUMS`。
- `agentq update` 运行安装脚本时不再有 30 分钟的总超时（下载仍有超时），免得安装做到一半被杀。

### 卡住了先自己想办法，再找你

- 任务技能：标卡住之前要先查（报错原文、官方文档、上游 issue，来源写进 `DECISIONS.md` 或交付说明），按查到的再换一种做法；只有授权、付费、账号、产品方向这类必须你定的才直接交给你，写成 `（卡住：等用户：…）`。
- 任务自己标了卡住、又不属于等你定的：不再推「卡住了」，引擎去掉卡住标记，下次 tick 用新会话从上次的分支再接一次，合同带上上次的原因和说明，要求先查资料、换思路。再接的那次还卡住，才推一条「再接一次还是卡住了」，写两次各怎么卡住的、分支在哪；不会接第三次。
- 门退回两轮还不过时，决定卡片除了门的结论，还带任务最后一轮自己的说明（对审查意见的分析）。

### 自己长大：自己的问题自己修、修过的变考题、修好自己用上

配了 `self_project`（agentq 源码所在的项目）时默认打开，`self_grow_enabled=false` 关掉就和以前一样。详见 `docs/reference/commands.md` 的「自己长大」。

- agentq 自己反复出的问题（门判不了、任务超时、巡逻发现等）直接写进 `self_project` 的「待做」并自动起任务，不再等你回「做 N」；`self_project` 在 `exclude_projects` 里也做。同一类修过两次还出现，条目写成重写这一块。
- 巡逻每周用 `govulncheck` 查一次正在运行的 agentq 编进去的 Go 版本和依赖，有已知漏洞就写进待做；没装就跳过，`agentq status` 里说明怎么装。照提示 `go install` 装在 `~/go/bin` 的也找得到（后台服务的 PATH 里没有这个目录）。
- 评测门：agentq 自己的修复（`type` 是 `fix`、改了 Go 代码）必须新加一道能复现它的评测题，不然退回；只新加整道题不再算改尺子，照常自动合并。新加评测题 `022-self-grow-todo`。
- 合并过了评测门的修复，在没有任务在跑、在过门时用同一安装引擎升级自己（有要升的就先不接新活，最多挡 2 小时）；第一次升级前先在真实服务上演练一次整代回滚，演练完原来的回滚点还在。升级后 3 个任务的失败、超时、卡在门上的个数和过门轮数比升级前差就回滚（还在过门的任务等门出结论再算，不当成功）到上一代、推通知说原因、写一条待做去修；带着回滚掉那次改动的版本都不再升，直到修它的那一条合并。
- 每次安装后只留现在、上一代和最近 3 代程序，旧的代自动删掉（以前一直堆着）。
- `agentq evolve upgrades` 看每次演练、升级、留下、回滚的记录和观察期；`agentq evolve apply` 现在就照规则做一次。

### 评测题补到 21 道

- `agentq eval list` 从 8 题加到 21 题，领域多了 engine、routine、docs，每个领域至少 2 题。新题把 11-09～11-11 真实使用里修过的 9 类问题各做成一道能复现的题（暂停时门退回接不上、停掉的条目静默等、开关当天打开巡逻误报、旧远程让无事跳过失效、被排除项目亲手派的活不续跑、首版设计被否返工、任务汇报语言跟条目、`agentq add` 不在桌面/文档/下载本身建仓库、`retention_days` 配 0 不删记录），在修复前的那一代上都会判成没过；另补前端、文档、安装各一道。
- 评测门（`agentq eval judge`）因此能拦下这 9 类问题的回归。

### 做减法：删掉用不上的功能、配置项和命令

按 30 天的真实使用（任务记录、日志、推送记录、配置和信号）删减：一次都没运行过的功能删掉，从没改过默认值的配置项写死，做同一件事的两套留一套。

- 删掉的功能和命令：主动感知（`agentq sense`，读 App Store 评价、TestFlight 反馈、GitHub issue）、发版助手（`agentq release`，Telegram 回「发」提交审核）、Router（Telegram 新消息现在一律交给对话任务，快捷命令照旧）、闲时找活（`agentq suggest`）、任务箱（`agentq inbox`、`~/.agentq/outbox/`）。「## 建议」一节、回「做 N」「不做 N」和 `docs/NOT-DOING.md` 还在，自我进化的提案写在这里。
- `agentq quality` 并进 `agentq stats`：stats 最后带上最近 7 天的一次满意率、返工率和每条的依据。
- 删掉的配置键：`suggest_enabled` `suggest_model` `suggest_budget_usd` `sense_enabled` `sense_model` `sense_budget_usd` `release_assist_enabled` `release_budget_usd` `router_enabled` `router_model` `verify_after_merge` `model_policy`。
- 写死、不再读的配置键（值和原来的默认一样）：`tick_after_job`（开）、`breaker_streak` 3、`breaker_quick` 3m、`breaker_cooldown` 30m、`breaker_max` 4h、`stall_after` 45m、`night_timeout` 6h、`review_timeout` 4h、`night_budget_usd` 10、`review_budget_usd` 15、`patrol_budget_usd` 2、`job_budget_usd` 150、`progress_autostop`（开）、`verify_timeout` 30m、`discuss_wait` 2h、`evolve_signals_enabled`（开）、`learned_constraints_enabled`（开）；按活分配的 `discuss_model` `review_model` `review_effort` `patrol_model` `followup_model` `heavy_effort` `low_risk_model` `low_risk_effort` `light_review_model` 写死成原来的默认分配（`model` 跟随 CLI 或别家后端时照旧全部跟随主模型）。
- `patrol_analyze` 并进 `patrol_enabled`：开着巡逻，有新异常时就起一次只读分析（每天最多 3 次）。
- 配置项从 66 个减到 26 个，命令从 39 个减到 34 个。

老用户要改什么：

- 不用改配置文件：上面这些键写着也不报错，只是不起作用；agentq 下次整份保存配置时会去掉。改过其中某个值的，现在按上面写死的值走。
- 开过 `sense_enabled`、`release_assist_enabled`、`router_enabled`、`suggest_enabled` 的，这些功能不再运行；`~/.agentq/` 里的 `sense/`、`release.json`、`router/`、`inbox.md`、`outbox/` 不再读写，确认不要后可以自己删。
- 只开巡逻、关着 `patrol_analyze` 的，升级后巡逻有新异常会起分析（花少量额度）；不要的话关掉 `patrol_enabled`。
- 脚本里调 `agentq quality` 的改成 `agentq stats`；调 `agentq inbox`、`suggest`、`sense`、`release` 的会报「不认识的命令」。

### 装好就能用：装完即运行，开口说

- 一键安装（`install.sh`、`scripts/bootstrap.sh`）装完直接是运行状态：服务加载、不暂停、权限放开，不再按回车同意，不用 `agentq start`。安装本身就是同意，结束时如实说一句后台任务能读写本机文件、运行命令、用你自己的模型额度，并给暂停、卸载命令；`consent.json` 记方式 `install`。想先不跑加 `--paused`（`AGENTQ_PAUSED=1`）。旧的 `--yes` 还认，没有作用。
- 升级保留你原来的开关：暂停着的不打开；装之前就有、写着暂停的老配置也不打开。
- 没有终端时 CLI 没装或没登录：不装不登录，给命令；弄好后在聊天里说需求，后台自己开起来。
- 派活技能也装到 Codex（本机用着 Codex 时装到 `~/.agents/skills`）。在 Claude Code 或 Codex 里说开发需求不用说「派活」：加功能、修 bug、改界面，哪怕很小，都写成待做交后台；只是问问、改错字改文案这类零碎改动、说「直接改/我看着」的当场做；看进度、验收、停也在聊天里说。
- 没接入的目录自己接入：`agentq add` 在不是 Git 仓库的目录里先 `git init`，现有文件做初始提交（自动写的 `.gitignore` 挡住 `.env`、密钥和依赖目录）；家目录、`projects_root`，以及桌面、文档、下载、临时目录本身不建（里面的子目录照常）。家目录、桌面这类不像项目的地方，技能先问一句项目名和位置。
- 新命令 `agentq start --check`（只读）：后台能不能接活。服务没加载时技能自己 `agentq start` 拉起。

### 任务汇报用条目的语言

- 后台任务结束时的汇报和条目原文用同一种语言，中文条目不会再收到英文汇报。

### 自我进化：收得到真实使用里的问题

- `agentq evolve signals` 多五类信号，都只读本机已有的数据、不调模型：你亲手 `agentq stop` 的任务（`user-stop`）；门卡片或 `gate` 命令上你退回、放弃的任务（`user-decide`，读门的审计日志）；BACKLOG 里出现「验收 <标题> 的意见」（`rework`，按条目范围、原条目范围和合并提交改了的文件归到前端、后端、设计、文档，看不出时按意见的文字，再看不出记「未分类」）；巡逻报过、当天又自己消失的问题（`patrol-gone`，疑似误报）；还在「待做」的条目被 retry_after 挡住满 6 小时（`retry-wait`，暂停时不算）。
- 这几类都算 agentq 自己的问题，同类两次以上和原有信号一样写提案。没有这几类数据时信号文件不变。

### 自我进化：评测门

- agentq 改自己源码的任务过门时多一道评测门：用合并态构建候选，由运行中这一代用自己的题跑 `agentq eval judge`（`--fake` 加程序题，不调模型），和基线比，任一题或任一领域变差就退回，理由写清哪题哪领域和耗时。候选改评测题不影响结果。没有基线只记录不挡，提示先 `agentq eval run --fake` 再 `agentq eval baseline`。别的项目不跑这道门。
- 候选改了评测题、判分、门的判定或 `.agentq/` 的，门过了也不自动合并，等你拍板。
- 新命令 `agentq eval judge --bin <路径>`。

### 推送和 Telegram 回复

- 消息分块：每段段名加粗，内容放进带竖条的块里，项目名加粗；早报标题下先写一句结论（早报第一行），昨晚完成按项目一行。
- 额度改成进度条放在最后：`会话 ▰▱▱▱▱▱▱▱▱▱   1%  今天 19:00 重置`，到后台上限的行末加 ⚠️，只列会限制任务的窗口。早报、Telegram「状态」「额度」都这样显示，`agentq status` 照旧是文字。
- 对话任务的回答也按这套分块版式写。

### 按活分配模型

- 每类调用按活的性质配模型和强度（依据官方对各档模型的定位）：Telegram 路由用 haiku；开工前问大方向、复盘、巡逻分析、回复链里的只读答问用 sonnet（复盘 high，其余 medium）；带 `（低风险）` 的待做任务用 sonnet、medium；标题提到重写、重构、迁移，或标了 `（难撤回）` 的待做任务模型不变、强度 xhigh（重写、重构这类即使标了低风险也不降模型）；完整流程的代码审查和界面审查遇到影响面广、难撤回的改动用 high。普通待做任务、夜班、要动手的对话照旧用 `model`、`effort`。
- 新键 `discuss_model`、`review_model`、`review_effort`、`patrol_model`、`followup_model`、`heavy_effort`、`low_risk_effort`；`router_model` 缺键默认从 sonnet 改成 haiku，`low_risk_model` 从空改成 sonnet。都写别名，新模型出来自动跟上；新配置、默认后端不是 Claude、配置明写 `"model": ""` 跟随 CLI 时都是空的，照 `model`、`effort`（上一版的 `light_review_model` 也照这个规则，跟随 CLI 的配置不再被塞 sonnet）。
- `agentq doctor` 发现配置里写了带版本号的模型 ID 会提醒改成别名。

### 用量显示

- `agentq status`、当天汇总、`agentq stats`、推送里的用量只写 token，不再写按 API 标价折算的「约 $x」：用订阅时这个数不是实际花的钱。汇总小结改叫「当天用量」，按 token 排；`stats` 最后一列是 token 占比。美元预算上限照旧按金额算。

### 调度

- 暂停时亲手派的活能做完：用 `agentq run` 或 Telegram「做」启动的条目被门退回或中断了，暂停期间 tick 也会用同一会话接着做，不用先 `resume`；其余条目照旧不动。`agentq run` 点名的条目中断了、还没人续跑时（包括 `exclude_projects` 的项目），直接接着做，不再报「没有可以做的条目」。
- `exclude_projects` 里的项目，用 `agentq run`、Telegram「做」亲手启动的条目被门退回或中断了，tick 也会用同一会话接着做（同暂停时）；原来直接放掉，要再敲一次 run。自动流程照旧不碰这些项目的其他条目。
- 夜班、复盘当天才打开：定点时刻开关还关着的那天，巡逻不再报「08:00 后还没有当天的早报/夜班报告」，tick 也不补跑；第二天起照常。原来一打开就报两条缺报告，还会立刻补跑一次复盘。
- 项目里留着指向已删除 GitHub 仓库的远程时，夜班、复盘的「无事跳过」照常能判断，不再因为 `gh` 找不到仓库天天照跑。
- 停掉的条目写明多久后再接：`agentq stop`（和 Telegram「停」）的回复、`agentq status` 的待做列表会写出这一条要等 `retry_after`（默认 12 小时）到几点才自动再接，以及现在就做用的 `agentq run` 命令。原来不说，看着像没人接。
- 只限某个模型的周额度（如只限 Sonnet 的周窗口）按每个任务实际用的模型算：到后台上限时，用这个模型的任务（低风险条目、复盘、巡逻分析、答问……）不启动，用别的模型的照常；`agentq status` 的额度行写明挡的是哪类任务。原来只看主模型 `model`，用 sonnet 的任务照样会启动。
- 后台给你留额度：5 小时会话窗口用到 70% 就不领新活（原来是 90%），周窗口仍是 90%，等窗口重置自动恢复。`usage_stop_session_pct`、`usage_stop_week_pct` 可改。`agentq status` 的额度行写出各窗口的后台上限。
- 同一项目里没有依赖的条目可以并行：条目写 `（范围：a/, b.go）` 标明改动范围，同项目在跑的条目都写了范围且互不重叠时一起起，一次 tick 可以连着起几条；写 `（依赖：<另一条标题里的一段字>）` 的等那条进了「已完成」才起。依赖哪里都找不到时 `agentq status` 说一句。
- `per_project` 默认从 1 改成 2（新配置和没写这个键的旧配置）。没写范围的条目照旧一次一条，所以不用新标记时没有变化。
- 之前手动设了 `per_project` ≥2 的：没写范围的条目从直接并行改成串行，要并行就给条目写上范围。`agentq run`、中断续跑、接手孤儿条目、回复续跑和发版准备任务守同样的规则（发版准备没有条目，当没写范围）。

### 质量门

- 新命令 `agentq gate return <任务id> --reason "意见"`：门停住等你看时，带意见退回，任务用同一会话接着改，意见原文进下一轮提示词。以前只有 Telegram 卡片的「退回」，不带意见，任务只能看到门的报告（门都过了时它不知道要改什么）。
- 门停住等你看时，在分支上补了提交（带着交付单）再 `agentq gate rerun`：命令回复和 `agentq gate report` 马上写新提交，再审次数按新提交重新算。以前要等门跑完才改成新提交，补了提交后再审还可能被旧提交的次数上限拒掉。
- 小改动走轻量流程：改错的代价是「局部」、不算测试和记录文件最多 3 个文件 60 行、不是 rewrite、没改指令文件和门脚本时，审查和界面审查换便宜一档的 `light_review_model`（默认别名 sonnet，新模型出来自动跟上；新配置为空即照 `model`；不能设成 haiku），强度照旧 medium；测试、补丁、烟测这些确定性的门照跑。`agentq gate report` 多一行「流程：轻量/完整（理由）」。
- 审查和界面审查读到条目依据的方案：条目写了 `.md` 路径就读那份，只写「按方案第几节」的读 `docs/specs/` 下最新三份；照方案做的设计不再被提意见。界面审查也带上开工前定的大方向。
- 按改错了多难撤回放行。动了迁移、`*.sql`、部署目录，新增行有 `DROP TABLE`、`DELETE FROM` 这类语句，或条目标了 `（难撤回）`：门照跑，过了也不自动合并，卡片「等你拍板」，`agentq gate pass` 才合。改了依赖清单、接口定义、删了文件、改动很大：补丁、测试、烟测门这次 `warn` 当 `error`，测试门关着就等你定。项目可在 `.agentq/risk.json` 加规则（读默认分支上的；写坏了修好之前一律等你拍板）。`agentq gate report` 多一行「改错的代价」。
- 退回时限定范围：门退回的提示多一行「范围：只改这些文件：…」，是没过的门点名的文件（没点名就是这一轮改的文件）加同目录的测试文件。下一轮新门 `scope`（默认 `error`，`.agentq/gates.json` 可调）查这一轮任务自己的提交有没有改范围外的文件，合默认分支带来的不算，`CHANGELOG.md`、`DECISIONS.md` 这类记录文件不算越界。退回 2 轮还没过的，卡片、卡住标记和报告写「多半是计划（条目拆法）的问题：建议重新拆或改验收」。

### 自我进化

- tick 每小时从门结果、任务记录、巡逻收信号，同类归并，不调模型（`evolve_signals_enabled`，默认开）。`agentq evolve signals` 看。
- 学到的约束：门拦下、退回后同一条合并了或被放弃的拦截，记成项目约束，新任务合同末尾带上出处（`learned_constraints_enabled`，默认开）。`gate pass` 放行的不算。每个项目最多 10 条，30 天没再出现删掉。`agentq evolve constraints <项目>` 看，派活技能写条目前也看。
- agentq 自己的问题同一类出现两次写一条带证据的提案，只提一次：配了 `self_project` 写进那个项目的「## 建议」（回「做 N」「不做 N」），没配写 `~/.agentq/evolve/proposals.md`。

## v0.1.0

### 评测

- `agentq eval`：评测集（`evals/`）和基线。`run` 默认只跑不调模型的程序题，`--fake` 用假 CLI 走完全部题，`--real` 才调模型；`compare` 任一题或任一领域变差就报。留出题放 `~/.agentq/evals/heldout/`。

### 后端

- Codex CLI 后端（`{"backend":"codex"}` 或 `project_backends`）：提示词走标准输入，会话从事件流取，能续跑；审查用 `workspace-write` 沙箱；技能在 `~/.agents/skills`，规则是 `AGENTS.md` 里带标记的一段。能力表和没有的东西见 `docs/backends.md`。
- `backends.<名字>`：每个后端自己的路径、模型、强度、权限。模型和权限不再串给别的后端。

### 质量门

- 任务只做到分支可合并，写 `.agentq/ready.json`；引擎在合并态上跑测试门和审查门，过了才合并，并写 BACKLOG、PROGRESS。合并后核验删除，`verify_after_merge` 不再起作用，`verify_timeout` 改作门的超时。新增 `agentq gate report|pass`，项目用 `.agentq/gates.json` 调档位。独立审查由引擎起进程，后端不再要求子代理（`doctor` 的能力表改成「引擎审查」）。

- 三道新门，默认只记录，`.agentq/gates.json` 可以调成挡合并。补丁门：同一模块 30 天内第三次修复记下来，调成 `error` 就退回，合同改成重写。界面门：改到界面文件才跑，脚本截图，查不是空白，再交给审查进程，只有渲染坏、溢出、看不清、占位内容能阻塞。成品门：`agentq new` 的第一版和标了 `（成品）` 的条目才跑，烟测过了才有交付单。
- `agentq health [项目]`：每个模块最近 30 天的改动、修复次数、行数，两周返工率。
- `agentq new` 的第一条不再自己合并，和别的条目一样过门。结尾第四行从「合并提交」换成「任务分支」。第一版没合并时这个项目后面的条目不开工，`agentq status` 写明卡在哪和解法。
- 界面门的审查走任务的后端：Claude 读文件看截图，Codex 用 `--image` 附上；后端看不到图片算判不了，不放行。
- `agentq gate abandon <任务>`：放弃卡在门上的任务或新项目的第一版，分支留着。
- 任务动 `docs/design/approved/`（用户确认过的界面基线）过不了测试门。

- 大方向商量：标题带 `（大方向）` 的条目和 `agentq new` 开工前问一次，最多 3 个问题，每个带默认答案。Telegram 卡片等 `discuss_wait`（默认 2 小时）没回按默认，别的条目不等；没配 Telegram 直接按默认，不调模型。`agentq discuss` 看和答。方向写进条目，审查门看得到。
- `agentq stats` 按门统计通过、失败、判不了、warn 档会挡下多少、退回和放行。早报多一节：放行、放弃、warn 没过的、关着的门。
- 门调模型撞上游 5xx：退避 15 分钟、1 小时重跑，3 次都不行才判不了。之前重试一次就卡住。
- `agentq gate rerun` 和卡片的「再审一次」：不改代码重新跑门，不算退回轮数，同一提交最多 2 次。
- `gate/reviews/` 随 `retention_days` 清，在用的和卡住的不清。门上卡住的任务记录不再 30 天过期。

### 上手

- 默认分支自动认：`origin/HEAD` 指的分支，没有就看本地 `main`、`master`，不再要求叫 `main`。认不出的项目不起任务，`agentq status` 和日志写明原因。
- 做完告诉你：任务合并后推一条，带合并提交和验收方法；失败、中断、卡住也说。一键脚本里第一次同意启动时打开本机通知，配了 Telegram 发 Telegram；已有配置里写的 `notify_enabled=false` 不动。
- 一条命令安装和升级：`install.sh` 检查 Git 和 Claude CLI，下载程序并校验 SHA256，配置和任务数据保留。发第一个 Release 后可以用 curl 直接运行，在此之前 clone 后 `sh install.sh`。
- 新安装的最后一步讲清楚任务能做什么，按回车同意并启动，输入 n 只安装。没有终端或在 CI 里只安装不启动；`--yes` 是明确同意。同意记在 `~/.agentq/consent.json`。
- 安装脚本加 `--proxy`、`--mirror`（校验文件不走镜像）、`--uninstall`、`--version`，拒绝 root，终端里转圈，遵守 `NO_COLOR`。
- 通用工程规则随安装带：第一次同意时放到 `~/.claude/rules/agentq/`，前台会话和后台任务都读得到。已有全局规则的不放；`agentq env status|on|off` 管理；改过的文件升级和卸载都不动。
- 第一次 `agentq add` 自动接入项目，不用先 `init`；项目不用放到 `~/projects` 下，在外面就登记到 `extra_projects`。没有 `~/projects` 时 tick 不再报错。
- 接手老项目时按 `go.mod`、`package.json`、`Cargo.toml`、`pyproject.toml`、`Package.swift` 生成 `.agentq/verify`，判断不了就不写。核验脚本退出 125 表示这次跑不了，不算失败。
- 新配置的模型和强度留空，跟随你在 CLI 里选的；已有配置照旧。可选 `model_policy=auto` 按任务类型选，重写、审查类不降级。
- 只有桌面端或什么都没有也能走完：同意后运行官方命令装 Claude CLI、登录，没登录不开后台。官方脚本装不上就停下，贴原文，给 Homebrew 和 npm。
- `~/.local/bin` 不在 PATH 时，同意后往 zsh、bash 的配置文件加一行带标记的 export，改前备份，卸载时只删这一行。
- `agentq update` 升级：取发布地址上的 `install.sh`，校验后运行。
- `agentq init` 把项目接入，`agentq add` 加一条待做，`agentq start` 一步打开后台。
- `agentq start` 讲清完全权限的风险再问你。不同意就什么都不改。
- `agentq version` 查版本。
- 没有 Python 也能装，只有可选第三方技能的辅助脚本要用。

### 配置和后台

- 后端接口：`backend` 选 CLI 后端（默认 `claude`），写错不回退，任务不启动；`project_backends` 按项目换。目前只有 Claude 一个实现，`agentq doctor` 打出能力表，接新后端见 `docs/backends.md`。
- `exclude_projects`：列出的项目 tick、闲时找活、主动感知都不碰，`agentq run` 明确指定时仍可跑。
- 磁盘清理：`launchd.log` 超过 10MB 只留尾部；任务记录超过 `retention_days`（默认 90 天，`0` 不删）的 tick 每天清一次。你的产出 `outbox/` 不动。
- Telegram：`agentq notify setup` 配好凭据后自动打开推送；`agentq start` 把常驻监听一起装上，也可以 `setup --services --listen`。
- 配置文件不存在时跑普通命令，新写出的配置和安装器一致：暂停、权限 `default`、后台开关全关。已有配置一个字节都不动。
- 权限是 `default` 时，`agentq run` 和 `agentq doctor` 会提醒任务写不了文件。
- `agentq status` 说出谁拿着 `tick.lock`、拿了多久：tick 卡住时不再只写「调度：正常」。
- `state.json` 和任务的 `meta.json` 同时坏时，tick 重建状态会杀掉这个任务还活着的进程并写进当天汇总，不再让它没人管，也不会同一条起两个。
