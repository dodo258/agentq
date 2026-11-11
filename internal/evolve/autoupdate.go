package evolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 自动更新（auto_update，默认关）：普通用户的发行版自己取新版本。和自己长大共用空闲判断、hold、观察期、回滚和 upgrades.jsonl（upgrade.go、apply.go）。
//   - 只管发行版（版本不是 dev 开头）、安装引擎装的（有 receipt.json）、没配 self_project 的机器；源码构建和维护者的机器由自己长大管。
//   - 关着时不联网。开着时 tick 每天最多看一次：跑 agentq update --check --saved-net，取发布里的 VERSION 对过 SHA256SUMS（和 agentq update 同一条路）。
//   - 比现在新（按版本号比，认不出的不比）、没回滚过、6 小时内没装失败过，就和自己长大一样等空闲、交给 agentq evolve apply：
//     它先等 tick 放开锁，再跑 agentq update --saved-net --version <版本>（安装脚本对过校验才运行，安装那一步不设超时），新的一代装在新目录。
//   - 下载或校验不过不装：记一行、推一条，同一个版本只推一次。装上后观察期变差就 rollback 回上一代、推通知，这个版本以后不再自动装。
//   - 代理和镜像沿用一键安装时用过的（internal/setup 记在 ~/.agentq/update-net.json，--saved-net 读它）。
const (
	checkEvery = 24 * time.Hour
	// checkWait：看一次新版本最多等这么久（两次下载各有 2 分钟超时）。
	checkWait = 5 * time.Minute
	// alertedKeep：记着推过的版本最多这么多个。
	alertedKeep = 20
)

// autoState 是 ~/.agentq/evolve/autoupdate.json：上次看的时间和结果、推过没装成的版本。
type autoState struct {
	CheckedAt time.Time `json:"checked_at"`
	// Latest：发布上对过校验的最新版本（上次看成功时的）。
	Latest string `json:"latest,omitempty"`
	// Result：上次看的结果，status 那一行用。
	Result  string   `json:"result,omitempty"`
	Alerted []string `json:"alerted,omitempty"`
}

func (s *Service) autoPath() string { return filepath.Join(s.evolveDir(), "autoupdate.json") }

// isRelease：发行构建的版本号（scripts/build-release.sh 写进去的），不是源码构建的 dev、dev-<提交>。
func isRelease(v string) bool { return v != "" && !strings.HasPrefix(v, "dev") }

// autoSkip：开着自动更新但这台机器不归它管的原因；空的就是归它管。
func (s *Service) autoSkip() string {
	switch {
	case s.Cfg.SelfProject != "":
		return "配了 self_project，由自己长大升级"
	case !isRelease(s.Version):
		return "这一代是源码构建（" + s.Version + "），用 git pull 再 sh install.sh 升级"
	}
	if _, err := os.Stat(s.receiptPath()); err != nil {
		return "不是安装引擎装的（没有 " + s.Tilde(s.receiptPath()) + "），没有整代可换，用一键安装命令重装一次"
	}
	return ""
}

// AutoUpdateOn：打开了自动更新，这台机器也归它管。
func (s *Service) AutoUpdateOn() bool { return s.Cfg.AutoUpdate && s.autoSkip() == "" }

func (s *Service) loadAuto() (autoState, error) {
	var a autoState
	b, err := os.ReadFile(s.autoPath())
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, fmt.Errorf("读 %s：%w", s.autoPath(), err)
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return autoState{}, fmt.Errorf("%s 格式不对：%w", s.autoPath(), err)
	}
	return a, nil
}

// withAuto 拿着 evolve.lock 改 autoupdate.json（tick 看新版本和 apply 记推过的版本可能同时写）。文件坏了从空的重写。
func (s *Service) withAuto(fn func(a *autoState)) error {
	if err := os.MkdirAll(s.evolveDir(), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(s.evolveLockPath(), evolveLockWait)
	if err != nil || lock == nil {
		return fmt.Errorf("拿不到 %s：%v", s.evolveLockPath(), err)
	}
	defer lock.Close()
	a, err := s.loadAuto()
	if err != nil {
		s.Logf("自动更新：%v，重写", err)
	}
	fn(&a)
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(s.autoPath(), append(b, '\n'))
}

// checkRelease：tick 里最多一天看一次发布上的版本。看不成只记下原因，明天再看。
func (s *Service) checkRelease(ctx context.Context, now time.Time) {
	if a, err := s.loadAuto(); err == nil && !a.CheckedAt.IsZero() && !a.CheckedAt.After(now) && now.Sub(a.CheckedAt) < checkEvery {
		return
	}
	v, cerr := s.releaseLatest(ctx)
	err := s.withAuto(func(a *autoState) {
		a.CheckedAt = now
		if cerr != nil {
			a.Result = "没看成：" + textfmt.OneLine(cerr.Error(), 200)
			return
		}
		a.Latest, a.Result = v, "发布上最新是 "+v
	})
	switch {
	case cerr != nil:
		s.Logf("自动更新：看新版本没成，明天再看：%v", cerr)
	default:
		s.Logf("自动更新：发布上最新是 %s，现在是 %s", v, s.Version)
	}
	if err != nil {
		s.Logf("自动更新：%s 没写上：%v", s.autoPath(), err)
	}
}

// releaseLatest 跑 agentq update --check --saved-net：下载、对校验都是 agentq update 那一条路，仓库名也在那边。
func (s *Service) releaseLatest(ctx context.Context) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("找不到自己：%w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, checkWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "update", "--check", "--saved-net")
	cmd.Env = proc.ChildEnv()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("agentq update --check：%w：%s", err, textfmt.OneLine(errOut.String(), 300))
	}
	return strings.TrimSpace(out.String()), nil
}

// releasePlan：自动更新这次要不要装新版本、回滚装上的版本。只读本机文件，不联网。
func (s *Service) releasePlan(st *state.State, now time.Time) upgradePlan {
	evs, err := s.LoadUpgrades()
	if err != nil {
		return upgradePlan{Note: "升级记录读不了，先不更新：" + err.Error()}
	}
	pl, evs, done := s.observing(evs, st, now)
	if done {
		return pl
	}
	a, err := s.loadAuto()
	if err != nil {
		return upgradePlan{Note: "自动更新：" + err.Error()}
	}
	v := a.Latest
	newer, ok := compareVersions(v, s.Version)
	switch {
	case v == "" || !ok || newer <= 0:
		return upgradePlan{}
	case rolledBackRelease(evs, v):
		return upgradePlan{Note: "自动更新：" + v + " 装上后变差回滚过，不再自动装，等更新的版本"}
	case recentFail(evs, "", v, now):
		return upgradePlan{Note: "自动更新：装 " + v + " 没做成，" + upgradeRetry.String() + " 内不再试"}
	}
	return upgradePlan{Action: EvUpgrade, Release: v}
}

// installRelease：照 agentq update 装这个版本（--saved-net 沿用安装时的代理和镜像），装完核对版本。没装成记一行，同一个版本只推一次。
func (s *Service) installRelease(self string, pl upgradePlan) error {
	err := s.installer(self, "update", "--saved-net", "--version", pl.Release)
	if errors.Is(err, errNotIdle) {
		return err
	}
	if err == nil {
		if to, verr := versionOf(s.BinPath()); verr != nil || to != pl.Release {
			err = fmt.Errorf("装完报的版本是 %q，不是 %s：%v", to, pl.Release, verr)
		}
	}
	if err != nil {
		s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvFail, From: s.Version, To: pl.Release, Release: pl.Release, Why: textfmt.OneLine(err.Error(), 300)})
		s.alertRelease(pl.Release, "自动更新：装 "+pl.Release+" 没成（下载或校验不过的不装），还是 "+s.Version+"；这个版本不再推，"+upgradeRetry.String()+" 后再试："+textfmt.OneLine(err.Error(), 200))
		return err
	}
	s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvUpgrade, From: s.Version, To: pl.Release, Release: pl.Release})
	s.Notify.Notify(notify.Notice{Cat: "done", Key: "autoupdate:" + pl.Release,
		Text: fmt.Sprintf("自动更新：从 %s 升到 %s，接下来 %d 个任务比之前差就自动回滚", s.Version, pl.Release, observeTasks)})
	return nil
}

// alertRelease：同一个版本没装成只推一次。记不下推过就不推（宁可少推，不反复推）。
func (s *Service) alertRelease(version, text string) {
	first := false
	if err := s.withAuto(func(a *autoState) {
		if slices.Contains(a.Alerted, version) {
			return
		}
		first = true
		a.Alerted = append(a.Alerted, version)
		if len(a.Alerted) > alertedKeep {
			a.Alerted = a.Alerted[len(a.Alerted)-alertedKeep:]
		}
	}); err != nil {
		s.Logf("自动更新：%v，没推", err)
		return
	}
	if first {
		s.alert("autoupdate:"+version, text)
	}
}

// AutoUpdateApplies：这台机器归自动更新管（发行版、安装引擎装的、没配 self_project），不管开没开。
func (s *Service) AutoUpdateApplies() bool { return s.autoSkip() == "" }

// AutoUpdateLine 是 agentq status 的那一行：开着时说上次看的时间和结果、或为什么不管这台。关着返回空（status 把它收进「没开」，不归它管的机器不提）。
func (s *Service) AutoUpdateLine() string {
	if !s.Cfg.AutoUpdate {
		return ""
	}
	if why := s.autoSkip(); why != "" {
		return "开着但不管这台：" + why
	}
	if s.Cfg.Paused {
		return "开着，暂停时不看也不装"
	}
	a, err := s.loadAuto()
	switch {
	case err != nil:
		return "开着；" + err.Error()
	case a.CheckedAt.IsZero():
		return "开着，还没看过（下一次 tick 看）"
	}
	line := "开着，上次看 " + a.CheckedAt.Format("01-02 15:04") + "：" + a.Result
	if a.Latest != "" {
		if c, ok := compareVersions(a.Latest, s.Version); ok && c <= 0 {
			line += "，现在 " + s.Version + " 已是最新"
		} else if ok {
			if evs, err := s.LoadUpgrades(); err == nil && rolledBackRelease(evs, a.Latest) {
				line += "，现在是 " + s.Version + "；" + a.Latest + " 装上后变差回滚过，不再自动装，等更新的版本"
			} else {
				line += "，现在是 " + s.Version + "，空闲时装"
			}
		}
	}
	return line
}

// rolledBackRelease：这个发行版本装上后观察期变差回滚过，不再自动装。
func rolledBackRelease(evs []UpgradeEvent, v string) bool {
	return slices.ContainsFunc(evs, func(ev UpgradeEvent) bool { return ev.Kind == EvRollback && ev.Release == v })
}

// compareVersions 比两个发行版本号（v1.2.3、1.2.3-rc.1 这样的）：a 新返回 1，一样 0，旧 -1。认不出的 ok 为假，不比。
func compareVersions(a, b string) (int, bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range 3 {
		if pa.n[i] != pb.n[i] {
			if pa.n[i] > pb.n[i] {
				return 1, true
			}
			return -1, true
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0, true
	case pa.pre == "": // 正式版比同号的预发布新
		return 1, true
	case pb.pre == "":
		return -1, true
	case pa.pre > pb.pre:
		return 1, true
	}
	return -1, true
}

type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	var v version
	s = strings.TrimPrefix(s, "v")
	s, _, _ = strings.Cut(s, "+") // 构建信息不比
	s, v.pre, _ = strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.n[i] = n
	}
	return v, true
}
