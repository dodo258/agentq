claim agent/fix-usage
sed -i.fakebak 's/--lines/-l/; s/--words/-w/' "$wt/docs/usage.md" && rm -f "$wt/docs/usage.md.fakebak"
ready docs "文档的参数照 count.sh 改对"
result "改好了。\n\n分支 agent/fix-usage 可以合并了。验收：看 docs/usage.md。"
