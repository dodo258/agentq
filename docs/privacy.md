# 清洁源与本机隐私门禁

这份文档说明公开源码的边界，以及提交前和发布前怎么查隐私。维护者的协作记录不在公开源码里。

## 范围

`config/clean-source.json` 是可审计边界，不是“忽略所有难处理文件”：
- 默认纳入所有受控源码、prompts、测试、技能、脚本、模板、规则和通用文档。
- `private_paths` 列出维护者的协作记录和研究原稿（`PROGRESS.md`、`DECISIONS.md`、`LESSONS.md`、`docs/BACKLOG.md`、`docs/NOT-DOING.md` 等），它们不进入公开源码。普通项目里生成的同名文件是用户自己的，和这里无关。
- `.claude/` 是本机 worktree 和设置，不分发。分发的技能在 `skills/`，同样要扫描。
- `approved_files` 里的图片已看过内容和元数据，固定 hash 只批准这个版本。
- 媒体、图片、归档、大文件与二进制未知即阻断；审批必须先看内容/元数据/权属，再记录hash。文本不能用hash绕过检查。

## 运行

个人patterns仅存于 `~/.agentq/private/privacy-patterns.json`，权限600，父目录700；不把真实模式写进测试或仓库。新机器必须在仓库外建立自己的规则，缺失或损坏时失败关闭。格式：`version: 1`，`patterns`非空数组，每项含`type`、`literal`和可选`case_sensitive`；type用通用类别，literal用本机真实值。

```sh
python3 -I scripts/privacy_scan.py --manifest config/clean-source.json
python3 -I scripts/privacy_scan.py --staged --manifest config/clean-source.json
python3 -I scripts/privacy_scan.py --root /path/to/clean-candidate --manifest /path/to/clean-candidate/config/clean-source.json
```

默认扫tracked工作树；staged扫完整索引blob（含索引中的manifest）；root扫所有文件（含ignored/untracked/生成物），只扣除明确私有资产和`.git`。公开候选用 `scripts/export-public.sh <输出目录> <owner/repo>` 导出：从 HEAD 取文件、去掉 `private_paths`、填仓库名，再扫一遍，有命中就退出非 0。

成功无发现输出；发现只报`path:line:type`，文件名本身敏感时改成编号。0为干净、1为发现、2为无法可靠扫描。扫描不证明不存在所有编码/语义秘密，发布前还需独立人工检查。

## 接入提交前与测试

仓库自带Hook `scripts/pre-commit`。clone后可执行 `git config core.hooksPath scripts`，有既有Hook时应先比较并串联。不能用跳过Hook的参数提交。

`go test ./...` 内含隐私源门禁与真实临时Git提交拒绝测试；测试使用临时HOME/临时patterns，不读作者机器。测试只用通用规则和生成的哨兵。

