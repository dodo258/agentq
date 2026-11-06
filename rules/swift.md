---
paths:
  - "**/*.swift"
---
# Swift
- 不用 `!` 强制解包和 `try!`，用 `guard let`、`if let`、`do/catch`；错误要让用户或日志看得到。
- 新代码用 `async/await`。界面更新放 `@MainActor`，长任务支持取消，不阻塞主线程。
- 闭包里捕获 `self` 想清楚生命周期，会形成循环引用就用 `[weak self]`。
- SwiftUI 视图保持小而纯；状态放在最小够用的范围，文字走本地化，适配动态字体和深色模式。
- 交付前：`swift build`、`swift test`，或项目规定的 `xcodebuild test`。
