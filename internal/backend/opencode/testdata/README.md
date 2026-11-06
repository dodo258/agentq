# opencode 事件流夹具

**都是手写的，不是录的**：本机没有 opencode，真实的 CLI 没跑过。结构照 v1.19.5 的源码：信封和事件类型看 `packages/opencode/src/cli/cmd/run.ts`，`step_finish` 和错误看 `packages/schema/src/v1/session.ts`，撞限文字看 `packages/opencode/src/session/retry.ts`。真实跑过之后要换成录下来的。

- `success.jsonl`：一轮成功。
- `tool_steps.jsonl`：两轮，第一轮 `tool-calls`，最后一条消息是 JSON。
- `limit_429.jsonl`、`limit_text.jsonl`：429 带 retry-after；只有错误文字。
- `auth_error.jsonl`、`auth_401.jsonl`：`ProviderAuthError`；401。
- `server_500.jsonl`：先有一条写着 pass 的消息，再报 500，不能算过。
- `lost_finish.jsonl`：丢了最后的 `step_finish`（官方 issue #26855、#53207），退出码可能是 0。
- `unknown_finish.jsonl`：`reason=unknown`，opencode 的循环不算结束。
- `model_says_limit.jsonl`：模型正文说到 rate limit，不算撞限。
- `resume_after_error.jsonl`：出错后又开了一轮，看最后一轮。
- `truncated.jsonl`：最后一行只写了一半。
