package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"agentq/internal/gitx"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

const (
	// ghWait：一次 gh 调用最多等多久（tick 拿着锁）。
	ghWait = 20 * time.Second
	// verifyTries、verifyPause：发完核对公开仓，查不了（断网、5xx）时最多查几次、隔多久；GitHub 明确回没有就不再查。
	verifyTries = 3
	verifyPause = 10 * time.Second
)

// errNotFound：GitHub 明确回没有（404）。
var errNotFound = errors.New("HTTP 404")

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
			if strings.Contains(string(ee.Stderr), "(HTTP 404)") {
				return nil, fmt.Errorf("gh %s：%w", args[0], errNotFound)
			}
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

// unpublished：发版脚本退出 0 之后核对公开仓真有这个 tag 和已发布的 Release。都有返回空；没有或查不了返回原因，不能当发成。
func (s *Service) unpublished(ctx context.Context, repo, version string) string {
	pause := s.pause
	if pause == nil {
		pause = time.Sleep
	}
	var missing string
	var err error
	for i := range verifyTries {
		if i > 0 {
			pause(verifyPause)
		}
		if missing, err = s.missing(ctx, repo, version); err == nil {
			break
		}
	}
	if err != nil {
		return "查不了公开仓 " + repo + " 有没有 " + version + "：" + err.Error()
	}
	if missing != "" {
		return "公开仓 " + repo + " 没有" + missing
	}
	return ""
}

// missing：公开仓缺什么（这个 tag、这个 tag 的已发布 Release），都有是空的。
func (s *Service) missing(ctx context.Context, repo, version string) (string, error) {
	var ref struct {
		Ref string `json:"ref"`
	}
	if found, err := s.lookup(ctx, "repos/"+repo+"/git/ref/tags/"+version, &ref); err != nil {
		return "", err
	} else if !found || ref.Ref != "refs/tags/"+version {
		return " tag " + version, nil
	}
	var rel struct { // 这个接口只给已发布的 Release，草稿回 404
		TagName string `json:"tag_name"`
	}
	if found, err := s.lookup(ctx, "repos/"+repo+"/releases/tags/"+version, &rel); err != nil {
		return "", err
	} else if !found || rel.TagName != version {
		return " " + version + " 的 Release（tag 有）", nil
	}
	return "", nil
}

// lookup 用 gh api 查一样东西，读进 v；GitHub 回 404 是 false。
func (s *Service) lookup(ctx context.Context, path string, v any) (bool, error) {
	out, err := s.gh(ctx, "api", path)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return false, fmt.Errorf("gh api %s 的输出看不懂：%w", path, err)
	}
	return true, nil
}
