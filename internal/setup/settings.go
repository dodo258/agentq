package setup

// 校验宿主常用结构，不丢未知键；未来版本字段仍由宿主验证。
func validSettingsShape(v map[string]any) bool {
	for _, key := range []string{"env", "permissions", "hooks", "statusLine"} {
		raw, ok := v[key]
		if !ok {
			continue
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		switch key {
		case "env":
			for _, x := range obj {
				if _, ok := x.(string); !ok {
					return false
				}
			}
		case "permissions":
			for _, list := range []string{"allow", "deny", "ask", "additionalDirectories"} {
				if x, ok := obj[list]; ok {
					values, ok := x.([]any)
					if !ok {
						return false
					}
					for _, value := range values {
						if _, ok := value.(string); !ok {
							return false
						}
					}
				}
			}
			if mode, ok := obj["defaultMode"]; ok {
				switch mode {
				case "default", "manual", "plan", "acceptEdits", "auto", "dontAsk", "bypassPermissions":
				default:
					return false
				}
			}
		case "statusLine":
			if obj["type"] != "command" {
				return false
			}
			if _, ok := obj["command"].(string); !ok {
				return false
			}
		case "hooks":
			for _, raw := range obj {
				entries, ok := raw.([]any)
				if !ok {
					return false
				}
				for _, entry := range entries {
					item, ok := entry.(map[string]any)
					if !ok {
						return false
					}
					hooks, ok := item["hooks"].([]any)
					if !ok {
						return false
					}
					for _, hook := range hooks {
						h, ok := hook.(map[string]any)
						if !ok {
							return false
						}
						kind, ok := h["type"].(string)
						if !ok || kind == "" {
							return false
						}
						if kind == "command" {
							if _, ok := h["command"].(string); !ok {
								return false
							}
						}
					}
				}
			}
		}
	}
	return true
}
