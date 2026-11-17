package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"time"

	"agentq/internal/gitx"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// ghWait：一次 gh 调用最多等多久（tick 拿着锁）。
const ghWait = 20 * time.Second

// gh 跑一次只读的 gh 命令，返回标准输出。
func (s *Service) gh(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ghWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(gitx.GitEnv(s.Home), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = textfmt.OneLine(string(ee.Stderr), 200)
		}
		return nil, fmt.Errorf("gh %s：%s", args[0], msg)
	}
	return out, nil
}

// latestRelease：公开仓最近一次正式发布（不算草稿和预发布）的 tag 和发布时间；还没发过返回空。
func (s *Service) latestRelease(ctx context.Context, repo string) (string, time.Time, error) {
	out, err := s.gh(ctx, "release", "list", "--repo", repo, "--limit", "1", "--exclude-drafts", "--exclude-pre-releases", "--json", "tagName,publishedAt")
	if err != nil {
		return "", time.Time{}, err
	}
	var list []struct {
		TagName     string    `json:"tagName"`
		PublishedAt time.Time `json:"publishedAt"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", time.Time{}, fmt.Errorf("gh release list 的输出看不懂：%w", err)
	}
	if len(list) == 0 {
		return "", time.Time{}, nil
	}
	return list[0].TagName, list[0].PublishedAt, nil
}

// ciGreen：公开仓默认分支最新提交的检查都跑完了、都过了（跳过的算过）。没过返回原因。
func (s *Service) ciGreen(ctx context.Context, repo string) (string, error) {
	out, err := s.gh(ctx, "api", "repos/"+repo+"/commits/HEAD/check-runs")
	if err != nil {
		return "", err
	}
	var r struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("gh api check-runs 的输出看不懂：%w", err)
	}
	if r.Total == 0 || len(r.Runs) == 0 {
		return "公开仓最新提交还没有 CI 结果", nil
	}
	for _, c := range r.Runs {
		if c.Status != "completed" {
			return "公开仓最新提交的 CI 还在跑（" + c.Name + "）", nil
		}
		if !slices.Contains([]string{"success", "skipped", "neutral"}, c.Conclusion) {
			return "公开仓最新提交的 CI 没过（" + c.Name + "：" + c.Conclusion + "）", nil
		}
	}
	return "", nil
}
