package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UsageStream 是可选能力：运行中能读到这次运行花了多少。没有美元硬上限的后端靠它在 tick 里补，见 Runner.CheckLiveBudget。
type UsageStream interface {
	LiveUsage(job JobRef) *JobUsage
}

// SchemaFile 是任务目录里的文件：CLI 不能强制结构化输出时，schema 存这里，读结果时校验。
const SchemaFile = "schema.json"

// SchemaPrompt 是接在提示词后面的要求。
func SchemaPrompt(schema string) string {
	return "\n\n最终回答只输出一个 JSON 对象，不加别的文字，要符合这个 JSON Schema：\n" + schema
}

// ExtractJSON 从文字里取最后一个 JSON 对象，按 schema 校验。CLI 不能强制结构化输出时的补法：取不到或校验不过返回 false，当这次没结果。
func ExtractJSON(text, schema string) (json.RawMessage, bool) {
	var sch any
	if json.Unmarshal([]byte(schema), &sch) != nil {
		return nil, false
	}
	for i := strings.LastIndexByte(text, '{'); i >= 0; i = strings.LastIndexByte(text[:i], '{') {
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			continue
		}
		rest := strings.TrimSpace(text[i+int(dec.InputOffset()):])
		if rest != "" && rest != "```" { // 后面还有内容：这是里层的对象，或者不是最终回答
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil || validate(sch, v, "") != nil {
			return nil, false
		}
		return raw, true
	}
	return nil, false
}

// validate 校验 agentq 自己写的 schema 用到的那部分 JSON Schema：type、properties、required、additionalProperties、enum、items、minItems、maxItems。
func validate(schema, v any, path string) error {
	s, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	if want, ok := s["type"].(string); ok && !typeIs(want, v) {
		return fmt.Errorf("%s 应该是 %s", path, want)
	}
	if enum, ok := s["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return e == v }) {
		return fmt.Errorf("%s 不在可选值里", path)
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, _ := r.(string); name != "" {
					if _, ok := x[name]; !ok {
						return fmt.Errorf("%s 缺 %s", path, name)
					}
				}
			}
		}
		for k, val := range x {
			p, ok := props[k]
			if !ok {
				if s["additionalProperties"] == false {
					return fmt.Errorf("%s 多了 %s", path, k)
				}
				continue
			}
			if err := validate(p, val, path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		if n, ok := s["minItems"].(float64); ok && float64(len(x)) < n {
			return fmt.Errorf("%s 太短", path)
		}
		if n, ok := s["maxItems"].(float64); ok && float64(len(x)) > n {
			return fmt.Errorf("%s 太长", path)
		}
		for i, e := range x {
			if err := validate(s["items"], e, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeIs(want string, v any) bool {
	switch want {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number", "integer":
		_, ok := v.(float64)
		return ok
	case "null":
		return v == nil
	}
	return true
}

// 事件流没有时间戳的后端用下面三个：tick 时记下输出文件的大小和时刻，事件时间取首次被 tick 看到的时刻，误差最多一个 tick。
// 不让外壳管道加时间戳：外壳一死子进程就 SIGPIPE，现在直接写文件没有这个问题。

func seenPath(dir, name string) string { return filepath.Join(dir, name+".seen") }

// NoteSeen 文件比上次记的长了就记一笔。记不下不影响任务，只是时间更粗。
func NoteSeen(dir, name string, now time.Time) {
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return
	}
	marks := readSeen(dir, name)
	if len(marks) > 0 && marks[len(marks)-1].size >= fi.Size() {
		return
	}
	f, err := os.OpenFile(seenPath(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %d\n", fi.Size(), now.UnixNano())
}

type seenMark struct {
	size int64
	at   time.Time
}

func readSeen(dir, name string) []seenMark {
	b, err := os.ReadFile(seenPath(dir, name))
	if err != nil {
		return nil
	}
	var out []seenMark
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) != 2 {
			continue
		}
		size, e1 := strconv.ParseInt(f[0], 10, 64)
		at, e2 := strconv.ParseInt(f[1], 10, 64)
		if e1 == nil && e2 == nil {
			out = append(out, seenMark{size, time.Unix(0, at)})
		}
	}
	return out
}

// SeenAt 是文件里 offset 处的内容首次被看到的时刻；没记过返回错误。
func SeenAt(dir, name string, offset int64) (time.Time, error) {
	for _, m := range readSeen(dir, name) {
		if m.size > offset {
			return m.at, nil
		}
	}
	return time.Time{}, errors.New("没记过这个位置")
}

// LastSeen 是文件最后一次变长被看到的时刻，零值表示没记过。
func LastSeen(dir, name string) time.Time {
	marks := readSeen(dir, name)
	if len(marks) == 0 {
		return time.Time{}
	}
	return marks[len(marks)-1].at
}
