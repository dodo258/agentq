package service

// guardPrevTimeCmd 是保护脚本里取 agentq.prev 修改时间的命令。
const guardPrevTimeCmd = `stat -f '%Sm' -t '%Y-%m-%d %H:%M' "$bin.prev" 2>/dev/null`

func defaultServiceManager(a *Service) ServiceManager {
	return LaunchdManager{a: a, ctl: a.launchctl}
}
