---
paths:
  - "**/*.ts"
  - "**/*.tsx"
---
# TypeScript
- 保持 `strict`。不用 `any` 和 `as` 绕过类型，外部数据先校验再当成某个类型用。
- 每个 Promise 都 `await` 或明确处理错误，不留悬空的 Promise。`fetch` 用 `AbortSignal.timeout()` 设超时。
- 定时器、监听、订阅都有清理；React 的 effect 要返回清理函数。
- 用项目已有的组件库、状态库和样式方案，不引入第二个同类依赖。
- 交付前：`tsc --noEmit`、项目的 lint 和测试；有界面改动就看实际渲染。
