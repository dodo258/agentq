claim agent/usage-doc
cat >> "$wt/README.md" <<'MD'

## 用法

- `-l`：数行数。例子：`printf 'a b\nc\n' | sh count.sh -l`，输出 `2`。
- `-w`：数单词数。例子：`printf 'a b\nc\n' | sh count.sh -w`，输出 `3`。
MD
ready docs "README 补用法"
result "写好了。\n\n分支 agent/usage-doc 可以合并了。验收：看 README.md 的「用法」。"
