claim agent/greeting
printf 'package fixture\n\nfunc Greeting() string { return "Hello, agentq." }\n' > "$wt/greeting.go"
ready feature "实现 Greeting"
result "做完了。\n\n分支可以合并了。"
