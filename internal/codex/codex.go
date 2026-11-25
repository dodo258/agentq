// Package codex 让 Codex 审一次：和写代码的不是一家，看得到自家看不到的。
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Auditor 起 Codex。Path 空的是没装。
type Auditor struct {
	Path, Model, Effort string
	Timeout             time.Duration
	Env                 []string
}

// Ask 在 dir 问一次，要 schema 规定的 JSON。只能写 dir 和临时目录。
func (a Auditor) Ask(ctx context.Context, dir, prompt, schema, out string) (json.RawMessage, error) {
	if a.Path == "" {
		return nil, errors.New("没装 Codex")
	}
	if err := os.WriteFile(out+".schema.json", []byte(schema), 0o600); err != nil {
		return nil, err
	}
	log, err := os.Create(out + ".log")
	if err != nil {
		return nil, err
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.Path, "--search", "exec", "-C", dir, "-m", a.Model, "-c", "model_reasoning_effort="+a.Effort,
		"-s", "workspace-write", "-c", "sandbox_workspace_write.network_access=true", "-c", "approval_policy=never",
		"--output-schema", out+".schema.json", "-o", out+".json", prompt)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, a.Env, log, log
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("Codex 没审成：%w（%s）", err, out+".log")
	}
	b, err := os.ReadFile(out + ".json")
	if err != nil {
		return nil, err
	}
	if b = []byte(strings.TrimSpace(string(b))); !json.Valid(b) {
		return nil, errors.New("Codex 的结论读不懂")
	}
	return b, nil
}
