# Codex 事件流夹具

来自 codex-cli 0.166.0 的真实输出，临时 Git 项目、极小提示词，脱敏过（cf-ray、request id、路径）。

- `success.jsonl`：最小成功。
- `resume.jsonl`：`exec resume` 的成功，thread_id 和原会话一样，用量只算这一次。
- `auth_401.jsonl`：空的 `CODEX_HOME`，重连后 `turn.failed`。
- `schema_400.jsonl`：schema 不是严格格式，接口拒绝。
- `schema_ok.jsonl`：严格 schema，最后一条消息是 JSON。
- `killed_midway.jsonl`：真实任务中途被 SIGTERM（截到第 10 行），没有 `turn.completed`，开头有 type=error 的提示 item。

撞限没有真实样本（不能为了夹具把额度用完）：`limit_*.jsonl` 是按 `codex-rs/protocol/src/error.rs` 的文字拼的，事件结构和真实的 `turn.failed` 一样。
