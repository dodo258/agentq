claim agent/hello
printf 'Hello, agentq.\n' > "$wt/hello.txt"
ready feature "Add hello.txt"
case "$prompt" in # 合同说了用条目原文的语言才照条目写；没说就跟着中文合同写中文
  *用条目原文的语言*) result "Done.\n\nThe branch agent/hello is ready to merge; nothing is left undone. To accept it, open hello.txt on main or run .agentq/verify." ;;
  *) result "做完了。\n\n分支 agent/hello 可以合并了，没有没做完的。验收：在 main 上看 hello.txt，或者跑 .agentq/verify。" ;;
esac
