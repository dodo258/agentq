package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/fsx"
)

type backupTarget struct {
	Project string `json:"project"`
	Remote  string `json:"remote"`
	URL     string `json:"url"`
}

func maintenanceCommand(home, dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = InstallEnv(home)
	return cmd.Output()
}

func backupProjects(home string, out io.Writer) error {
	// 仅显式列出的目标才允许远程写入；服务安装本身不授权任何仓库。
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "backup.json"))
	if errors.Is(e, os.ErrNotExist) {
		fmt.Fprintln(out, "Backup inactive: no explicit targets in ~/.agentq/backup.json")
		return nil
	}
	if e != nil {
		return e
	}
	var cfg struct {
		Targets []backupTarget `json:"targets"`
	}
	if e = json.Unmarshal(b, &cfg); e != nil {
		return e
	}
	for _, t := range cfg.Targets {
		if t.Remote == "" || strings.HasPrefix(t.Remote, "-") || t.URL == "" || !filepath.IsAbs(t.Project) {
			return errors.New("invalid explicit backup target")
		}
		url, e := maintenanceCommand(home, t.Project, "git", "remote", "get-url", "--push", t.Remote)
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(url)) != t.URL {
			return errors.New("backup remote changed; re-authorization required")
		}
		meta, e := maintenanceCommand(home, t.Project, "gh", "repo", "view", t.URL, "--json", "isPrivate")
		if e != nil {
			return errors.New("cannot verify backup target visibility; refusing push")
		}
		var repo struct {
			IsPrivate bool `json:"isPrivate"`
		}
		if json.Unmarshal(meta, &repo) != nil || !repo.IsPrivate {
			return errors.New("backup target is not verified private; refusing push")
		}
		if _, e = maintenanceCommand(home, t.Project, "git", "push", t.Remote, "--all"); e != nil {
			return fmt.Errorf("backup failed: %w", e)
		}
		if _, e = maintenanceCommand(home, t.Project, "git", "push", t.Remote, "--tags"); e != nil {
			return fmt.Errorf("backup tags failed: %w", e)
		}
		fmt.Fprintln(out, "Backup OK:", filepath.Base(t.Project))
	}
	return nil
}

func RunMaintenance(args []string, out, errout io.Writer) int {
	if len(args) != 1 || (args[0] != "backup" && args[0] != "watchdog") {
		fmt.Fprintln(errout, "usage: agentq maintenance backup|watchdog")
		return 2
	}
	home, e := os.UserHomeDir()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if args[0] == "backup" {
		e = backupProjects(home, out)
	} else {
		i := &Installer{Home: home, Out: out}
		e = i.Doctor()
	}
	// 看门狗只写本地报告，不自动接活、不修权限、不重复推送。
	text := fmt.Sprintf("%s %s OK\n", time.Now().Format(time.RFC3339), args[0])
	if e != nil {
		text = fmt.Sprintf("%s %s ERROR: %v\n", time.Now().Format(time.RFC3339), args[0], e)
	}
	if w := fsx.WriteFileAtomic(filepath.Join(home, ".agentq", args[0]+"-report.txt"), []byte(text)); w != nil && e == nil {
		e = w
	}
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	fmt.Fprint(out, text)
	return 0
}
