package notify

const LocalNotifyNote = "（没配 Telegram，走本机通知：有桌面和 notify-send 时弹出，没有 notify-send 时只写进日志；要推到手机执行 agentq notify setup）"

func localNotify(a *Service, title, body string) error { return NotifySendOrLog(a, title, body) }
