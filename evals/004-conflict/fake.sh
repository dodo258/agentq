claim agent/mode
sed -i.fakebak '/^- \[ \] /s#（进行中#（卡住：agent/mode 两个测试互相矛盾，要用户定听哪个）（进行中#' docs/BACKLOG.md
rm -f docs/BACKLOG.md.fakebak
git add docs/BACKLOG.md
git commit -q -m "待办：卡住，等用户定"
result "两个测试互相矛盾，标了卡住。\n\n没合并，等用户定。"
