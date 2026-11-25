# 发版（给维护者）

私有仓库开发，公开仓库 `dodo258/agentq` 只放干净的快照。下面的步骤要人来执行，工作流不会替你推。

## 公开快照

1. 在私有仓库的 main 上确认 `.agentq/verify` 通过、`git status` 干净。
2. 取快照：`git archive --format=tar HEAD | tar -x -C <空目录>`。`.gitattributes` 把 `docs/specs/`（开发记录，含需求原话）挡在外面。
3. 在公开仓库现有 main 的检出里，把工作区换成快照内容（删掉快照里没有的文件），做一次**普通提交**。不强推，不带私有历史。
4. 提交邮箱用 GitHub 的 noreply 地址（`<id>+<用户名>@users.noreply.github.com`），提交前 `git config user.email` 看一眼。
5. 推送前 `git diff --stat origin/main` 看一遍，确认没有 `docs/specs/`、本机路径、个人信息。

## 发布

推 tag：`git tag v1.0.0 && git push origin v1.0.0`（版本形如 `v1.2.3`，可带后缀；旧版占用了 v0.1.0–v0.4.0，从 v1.0.0 起）。`release.yml` 会：

1. 在 macOS 上跑 `.agentq/verify`，不过就停。
2. `scripts/release.sh "$GITHUB_REF_NAME" dist` 编 darwin arm64、amd64 两个包，连同 `install.sh` 写出 `SHA256SUMS`。
3. `gh release create … --verify-tag --generate-notes` 建 Release，附上这四个文件。

Release 里**不放 `VERSION` 文件**：旧版（v0.1–v0.3）的自动更新看它，没有它旧版就不会被升级成这个完全不同的程序。旧版用户手动跑 `agentq update` 会取到新的 `install.sh`，它认得出旧版并拒绝，说明怎么换。

## 核对

- Release 页面有 `agentq_darwin_arm64.tar.gz`、`agentq_darwin_amd64.tar.gz`、`install.sh`、`SHA256SUMS` 四个文件，没有别的。
- `shasum -a 256 -c SHA256SUMS`（在下载目录里）全部 OK。
- 临时 HOME 里装一次：`HOME=$(mktemp -d) sh -c 'curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh'`，再 `$HOME/.local/bin/agentq version` 应是刚推的 tag。
- 本地不推 tag 也能试打包：`scripts/release.sh v0.0.0-test /tmp/agentq-release`。

没做 Apple 公证；amd64 包是交叉编译，没在 Intel 机器上跑过。
