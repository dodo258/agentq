package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// pathLine 是 agentq 往 shell 配置文件加的那一行。卸载按整行精确匹配，改过的不动。
const pathLine = `export PATH="$HOME/.local/bin:$PATH" # agentq managed`

// PathChange 说明 AddPathLine 做了什么。Changed 为 false 时，Reason 非空表示没法自动加，要给用户手动的那一行；Reason 为空表示配置文件里已经有了。
type PathChange struct {
	RC, Backup string
	Changed    bool
	Reason     string
}

// pathEntry 记一次改动，卸载时用。Created：文件是我们建的；AddedNewline：原文件结尾没有换行，我们补过一个。
type pathEntry struct {
	RC           string `json:"rc"`
	Backup       string `json:"backup,omitempty"`
	Created      bool   `json:"created,omitempty"`
	AddedNewline bool   `json:"added_newline,omitempty"`
}

func pathManifest(home string) string {
	return filepath.Join(home, ".local", "share", "agentq", "shell-path.json")
}

func readPathEntries(home string) ([]pathEntry, error) {
	b, e := os.ReadFile(pathManifest(home))
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, fmt.Errorf("读 PATH 记录失败：%w", e)
	}
	var entries []pathEntry
	if e = json.Unmarshal(b, &entries); e != nil {
		return nil, fmt.Errorf("PATH 记录 %s 格式不对：%w", pathManifest(home), e)
	}
	return entries, nil
}

// rcFile 选 shell 配置文件：zsh 是 .zshrc（认 ZDOTDIR，但不出 HOME），bash 按平台取第一个存在的，别的 shell 语法不一样，不碰。
func rcFile(home, shell, zdotdir string) (string, error) {
	switch filepath.Base(shell) {
	case "zsh":
		if zdotdir == "" {
			return filepath.Join(home, ".zshrc"), nil
		}
		rc := filepath.Join(zdotdir, ".zshrc")
		if rel, e := filepath.Rel(home, rc); e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("ZDOTDIR %s 不在 HOME 下", zdotdir)
		}
		return rc, nil
	case "bash":
		for _, name := range bashRCNames {
			rc := filepath.Join(home, name)
			if _, e := os.Lstat(rc); e == nil {
				return rc, nil
			}
		}
		return filepath.Join(home, bashRCNames[0]), nil
	case "":
		return "", errors.New("不知道你用的是哪个 shell")
	}
	return "", fmt.Errorf("%s 的配置语法不一样，agentq 不替你改", filepath.Base(shell))
}

// assignsLocalBinRe：一条语句开头给 PATH（或 zsh 的 path 数组）赋值，值里有 .local/bin。alias、source 这些只是提到它的不算。
var assignsLocalBinRe = regexp.MustCompile(`(?:^|[;&|)]|\bthen\b|\bdo\b|\belse\b)\s*(?:export\s+|typeset\s+(?:-\w+\s+)*|declare\s+(?:-\w+\s+)*)?(?:PATH|path)\+?=.*\.local/bin`)

// assignsLocalBin：配置文件里已经把 .local/bin 放进 PATH，这个终端读不到多半是还没重开，不再加一行。注释不算。
func assignsLocalBin(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		code, _, _ := strings.Cut(line, "#")
		if assignsLocalBinRe.MatchString(code) {
			return true
		}
	}
	return false
}

// RCForPath 说出 AddPathLine 会改哪个文件，同意文字用；不会改就返回空。
func RCForPath(home, shell, zdotdir string) string {
	rc, e := rcFile(home, shell, zdotdir)
	if e != nil {
		return ""
	}
	if b, e := os.ReadFile(rc); e == nil && assignsLocalBin(string(b)) {
		return ""
	}
	return rc
}

// AddPathLine 往 shell 配置文件追加带标记的一行，把 ~/.local/bin 放进 PATH。改之前备份为 *.pre-agentq，已有备份就加时间戳。
// 配置文件是链接时改它指向的文件，链接本身不动。
func AddPathLine(home, shell, zdotdir string, now time.Time) (PathChange, error) {
	rc, e := rcFile(home, shell, zdotdir)
	if e != nil {
		return PathChange{Reason: e.Error()}, nil
	}
	change := PathChange{RC: rc}
	target := rc
	if _, e := os.Lstat(rc); e == nil {
		if target, e = filepath.EvalSymlinks(rc); e != nil {
			change.Reason = fmt.Sprintf("%s 是坏掉的链接", rc)
			return change, nil
		}
	}
	entry := pathEntry{RC: rc}
	mode := os.FileMode(0o644)
	var old []byte
	switch st, e := os.Stat(target); {
	case errors.Is(e, os.ErrNotExist):
		entry.Created = true
	case e != nil:
		return change, fmt.Errorf("查看 %s 失败：%w", rc, e)
	case !st.Mode().IsRegular():
		change.Reason = fmt.Sprintf("%s 不是普通文件", rc)
		return change, nil
	default:
		mode = st.Mode().Perm()
		if old, e = os.ReadFile(target); e != nil {
			return change, fmt.Errorf("读 %s 失败：%w", rc, e)
		}
	}
	if assignsLocalBin(string(old)) {
		return change, nil
	}
	if !entry.Created {
		entry.Backup = rc + ".pre-agentq"
		if _, e := os.Lstat(entry.Backup); e == nil {
			entry.Backup += "-" + now.UTC().Format("20060102T150405")
		}
		if e := PutImage(entry.Backup, FileImage{Kind: "file", Data: old, Mode: mode}); e != nil {
			return change, fmt.Errorf("备份 %s 失败，没有改：%w", rc, e)
		}
		change.Backup = entry.Backup
	}
	text := string(old)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
		entry.AddedNewline = true
	}
	entries, e := readPathEntries(home)
	if e != nil {
		return change, e
	}
	if e = SaveInstallJSON(pathManifest(home), append(entries, entry)); e != nil {
		return change, fmt.Errorf("写 PATH 记录失败，没有改：%w", e)
	}
	if e := PutImage(target, FileImage{Kind: "file", Data: []byte(text + pathLine + "\n"), Mode: mode}); e != nil {
		return change, fmt.Errorf("改 %s 失败：%w", rc, e)
	}
	change.Changed = true
	return change, nil
}

// RemovePathLine 把 AddPathLine 加的那一行删掉：没有别的改动时和原来逐字节相同，备份也不再留；用户后来改过的内容都留着。
// 返回给用户看的说明。
func RemovePathLine(home string) ([]string, error) {
	entries, e := readPathEntries(home)
	if e != nil || len(entries) == 0 {
		return nil, e
	}
	var notes []string
	for _, entry := range entries {
		note, e := removeEntry(entry)
		if e != nil {
			return notes, e
		}
		notes = append(notes, note)
	}
	if e := os.Remove(pathManifest(home)); e != nil {
		return notes, fmt.Errorf("删 PATH 记录失败：%w", e)
	}
	return notes, nil
}

func removeEntry(entry pathEntry) (string, error) {
	target, e := filepath.EvalSymlinks(entry.RC)
	if errors.Is(e, os.ErrNotExist) {
		return fmt.Sprintf("%s 已经不在了，没有要改的。", entry.RC), nil
	}
	if e != nil {
		return "", fmt.Errorf("查看 %s 失败：%w", entry.RC, e)
	}
	st, e := os.Stat(target)
	if e != nil {
		return "", fmt.Errorf("查看 %s 失败：%w", entry.RC, e)
	}
	b, e := os.ReadFile(target)
	if e != nil {
		return "", fmt.Errorf("读 %s 失败：%w", entry.RC, e)
	}
	text := string(b)
	at := -1
	for from := 0; from <= len(text); {
		i := strings.Index(text[from:], pathLine+"\n")
		if i < 0 {
			break
		}
		if i += from; i == 0 || text[i-1] == '\n' {
			at = i
			break
		}
		from = i + 1
	}
	if at < 0 {
		return fmt.Sprintf("%s 里没找到 agentq 加的那一行（可能改过），没有动它。", entry.RC), nil
	}
	before, after := text[:at], text[at+len(pathLine)+1:]
	if entry.AddedNewline && after == "" {
		before = strings.TrimSuffix(before, "\n")
	}
	restored := before + after
	if entry.Created && restored == "" {
		if e := os.Remove(target); e != nil {
			return "", fmt.Errorf("删 %s 失败：%w", entry.RC, e)
		}
		return fmt.Sprintf("删掉了 agentq 建的 %s。", entry.RC), nil
	}
	if e := PutImage(target, FileImage{Kind: "file", Data: []byte(restored), Mode: st.Mode().Perm()}); e != nil {
		return "", fmt.Errorf("改 %s 失败：%w", entry.RC, e)
	}
	if entry.Backup != "" {
		if old, e := os.ReadFile(entry.Backup); e == nil && string(old) == restored {
			if e := os.Remove(entry.Backup); e != nil {
				return "", fmt.Errorf("删备份 %s 失败：%w", entry.Backup, e)
			}
			return fmt.Sprintf("%s 改回了原样。", entry.RC), nil
		}
		return fmt.Sprintf("从 %s 删掉了 agentq 加的那一行，你后来的改动都在。安装前的备份留在 %s。", entry.RC, entry.Backup), nil
	}
	return fmt.Sprintf("从 %s 删掉了 agentq 加的那一行。", entry.RC), nil
}
