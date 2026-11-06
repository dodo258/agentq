package service

// guardPrevTimeCmd 是保护脚本里取 agentq.prev 修改时间的命令。
const guardPrevTimeCmd = `date -r "$bin.prev" '+%Y-%m-%d %H:%M' 2>/dev/null`

func defaultServiceManager(a *Service) ServiceManager {
	return SystemdManager{a: a, ctl: RunSystemctl, loginctl: runLoginctl}
}
