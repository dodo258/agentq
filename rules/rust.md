---
paths:
  - "**/*.rs"
---
# Rust
- 产品代码不用 `unwrap()`、`expect()`、`panic!` 处理可能发生的错误，用 `?` 往上抛并带上下文；只有确实不可能失败时才 `expect` 并写明原因。
- 公开函数优先收 `&str`、`&[T]` 这类借用；`clone` 要有理由，不用它躲借用检查。
- 异步代码里不直接做阻塞 IO 或长计算，放进 `spawn_blocking`；外部调用设超时。
- `unsafe` 只在必须时用，紧贴着写清为什么安全。
- 交付前：`cargo fmt --check`、`cargo clippy --all-targets -- -D warnings`、`cargo test`。
