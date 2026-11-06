---
paths:
  - "**/*.sh"
  - "**/*.bash"
---
# Shell
- 脚本开头 `set -eu`（bash 再加 `-o pipefail`）。变量都加引号，变量紧跟非 ASCII 文字时写 `${变量}`。
- 临时文件用 `mktemp`，配 `trap` 清理。删除前确认路径变量非空，不写 `rm -rf "$x/"` 这种变量为空就出事的命令。
- `curl` 加 `--fail --max-time`；不把下载的内容直接管给 `sh`，除非是用户指定的官方安装命令。
- 脚本标明 `#!/bin/sh` 就只用 POSIX 语法。有 `shellcheck` 就跑一遍。
