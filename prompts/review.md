这是 agentq 的定期复盘入口，不是另一份技能源码。
完整加载 ~/.claude/skills/nightly-review/SKILL.md 并按它执行。项目根目录与报告位置读取本机 ~/.agentq/config.json，跳过 exclude_projects 列出的项目；汇总当天 night 与 agentq 报告，写本地早报 ~/.claude/nightly/<今天 YYYY-MM-DD>.md。
只按技能授权维护教训和待办，不修改代码、已安装技能、Hook、权限、认证或调度开关，不自动 push 或发外部消息。定制应写用户/项目层，通用工作流修正写 agentq 源码 BACKLOG，经验证和审查后安全升级。
