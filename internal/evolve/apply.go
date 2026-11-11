package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 动手的那一半：agentq evolve apply 照 plan（upgrade.go）演练、升级或回滚，都经子进程调安装引擎（setup --yes、rollback --yes）。

// spawnApply 起脱离 tick 的 agentq evolve apply：自己的进程组，服务卸载重装、tick 退出都不带走它。
func (s *Service) spawnApply() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到自己：%w", err)
	}
	if err := os.MkdirAll(s.evolveDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(s.upgradeLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "evolve", "apply")
	cmd.Dir = s.evolveDir()
	cmd.Env = proc.ChildEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // tick 还活着时不留僵尸，退出后交给 launchd 回收
	s.Logf("自己长大：起了 agentq evolve apply（pid %d）", cmd.Process.Pid)
	return nil
}

// errNotIdle：这会儿有任务在跑、在过门，或 tick 一直不放锁。不算没做成，下次空闲再做。
var errNotIdle = errors.New("不空闲")

// Apply 是 agentq evolve apply：照 plan 做一次升级（第一次先演练）或回滚。同一时间只有一个；不空闲就什么都不做。
func (s *Service) Apply() error {
	if err := os.MkdirAll(s.evolveDir(), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(s.upgradeLockPath(), 0)
	if err != nil {
		return fmt.Errorf("拿 %s：%w", s.upgradeLockPath(), err)
	}
	if lock == nil {
		fmt.Fprintln(s.Out, "已经有一个在升级或回滚")
		return nil
	}
	defer lock.Close()
	st, err := state.Load(s.StatePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出，当作未知，不升级：%w", err)
	}
	pl := s.plan(context.Background(), st, s.Now())
	if pl.Action == "" {
		fmt.Fprintln(s.Out, "没有要做的升级或回滚 "+pl.Note)
		return nil
	}
	self, err := os.Executable()
	if err == nil {
		switch {
		case pl.Action == EvRollback:
			err = s.rollback(self, pl)
		case pl.Release != "":
			err = s.installRelease(self, pl)
		case pl.Drill:
			var why string
			if why, err = s.drill(self); err == nil {
				s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvDrill, To: s.Version, Why: why})
				err = s.upgrade(pl)
			} else if !errors.Is(err, errNotIdle) {
				s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvFail, To: s.Version, Why: "整代回滚演练没过：" + textfmt.OneLine(err.Error(), 300)})
				s.alert("drill", "自己长大：整代回滚演练没过，先不自动升级："+textfmt.OneLine(err.Error(), 200))
			}
		default:
			err = s.upgrade(pl)
		}
	}
	if errors.Is(err, errNotIdle) {
		fmt.Fprintln(s.Out, "有任务在跑、在过门，或 tick 一直没放锁，下次空闲再做")
		return nil
	}
	return err
}

// installer 跑一次安装引擎（bin setup --yes / rollback --yes），输出进升级日志。
// 先等到空闲、tick 放开锁（安装引擎自己也拿 tick.lock，只等 90 秒）。不设超时、不杀：安装事务第一步就卸载服务，
// 中途被杀会留下没做完的事务、服务全停；安装引擎里联网、探测这些步骤自己有超时。
func (s *Service) installer(bin string, args ...string) error {
	if st, err := state.Load(s.StatePath()); err != nil || activeJobs(st) > 0 {
		return errNotIdle
	}
	lock, err := fsx.AcquireLock(s.LockPath(), tickWait)
	if err != nil || lock == nil {
		return errNotIdle
	}
	lock.Close() // 安装引擎自己要拿这把锁：这里只是等前一个 tick（巡逻、推送）跑完
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = s.evolveDir(), proc.ChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	s.Out.Write(out)
	if err == nil {
		return nil
	}
	if _, _, held := fsx.LockHolder(s.LockPath()); held || s.busy() {
		return errNotIdle // 交出锁的那一下又起了 tick 或任务：安装引擎没动，下次再做
	}
	return fmt.Errorf("agentq %s：%w：%s", strings.Join(args, " "), err, textfmt.OneLine(lastBytes(out, 2048), 300))
}

func lastBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// busy：状态文件里有在跑、在过门的任务。读不了当有（不动安装）。
func (s *Service) busy() bool {
	st, err := state.Load(s.StatePath())
	return err != nil || activeJobs(st) > 0
}

// readGeneration 读安装记录里现在是哪一代。
func (s *Service) readGeneration() (string, error) {
	b, err := os.ReadFile(s.receiptPath())
	if err != nil {
		return "", err
	}
	var r struct {
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Generation == "" {
		return "", fmt.Errorf("安装记录 %s 读不出是哪一代", s.receiptPath())
	}
	return r.Generation, nil
}

// versionOf 是这个程序报的版本（「agentq dev-…」去掉前缀）。
func versionOf(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", fmt.Errorf("%s 报不出版本：%w", bin, err)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "agentq")), nil
}

// drill：用这一代自己再装一代，再 rollback 回原来那一代。两代代码一样，演练中途跑 tick 也无妨。
// rollback 会删掉 previous.json：演练前的回滚点先存着，演练过了放回去（那一代还在才放）。
func (s *Service) drill(self string) (string, error) {
	gen, err := s.readGeneration()
	if err != nil {
		return "", err
	}
	prevPath := filepath.Join(filepath.Dir(s.receiptPath()), "previous.json")
	prev, _ := os.ReadFile(prevPath) // 没有就是没有回滚点，演练完也没有
	if err := s.installer(self, "setup", "--yes"); err != nil {
		return "", err
	}
	if g, err := s.readGeneration(); err != nil || g == gen {
		return "", fmt.Errorf("演练装一代后还是原来那一代（%s）：%v", g, err)
	}
	if err := s.installer(self, "rollback", "--yes"); err != nil {
		return "", err
	}
	if g, err := s.readGeneration(); err != nil || g != gen {
		return "", fmt.Errorf("演练回滚后不是原来那一代（现在 %s，原来 %s）：%v", g, gen, err)
	}
	if v, err := versionOf(s.BinPath()); err != nil || v != s.Version {
		return "", fmt.Errorf("演练回滚后装着的版本是 %q，应该是 %s：%v", v, s.Version, err)
	}
	why := "用这一代再装一代、rollback 回来，服务和版本都对"
	var r struct {
		Generation string `json:"generation"`
	}
	if json.Unmarshal(prev, &r) != nil || r.Generation == "" {
		return why + "；演练前没有回滚点", nil
	}
	if _, err := os.Stat(r.Generation); err != nil {
		return why + "；演练前回滚点的那一代已经不在，没放回", nil
	}
	if err := fsx.WriteFileAtomic(prevPath, prev); err != nil {
		return "", fmt.Errorf("演练前的回滚点没放回去：%w", err)
	}
	return why + "；演练前的回滚点已放回", nil
}

// buildTags 是这一代构建时的 -tags：新的一代照样构建。
func buildTags() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, kv := range info.Settings {
		if kv.Key == "-tags" {
			return kv.Value
		}
	}
	return ""
}

// upgrade：临时克隆检出目标提交、构建、用新程序 setup --yes。临时目录用完就删，主目录不动。
func (s *Service) upgrade(pl upgradePlan) error {
	bin, to, err := s.build(pl.Commit)
	if bin != "" {
		defer os.RemoveAll(filepath.Dir(bin))
	}
	if err == nil {
		err = s.installer(bin, "setup", "--yes")
	}
	if errors.Is(err, errNotIdle) {
		return err
	}
	if err != nil {
		s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvFail, From: s.Version, Commit: pl.Commit, Job: pl.Job, Why: textfmt.OneLine(err.Error(), 300)})
		s.alert("upgrade:"+short(pl.Commit), "自己长大：升级到 "+short(pl.Commit)+" 没做成，"+upgradeRetry.String()+" 后再试："+textfmt.OneLine(err.Error(), 200))
		return err
	}
	s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvUpgrade, From: s.Version, To: to, Commit: pl.Commit, Job: pl.Job})
	s.Notify.Notify(notify.Notice{Cat: "done", Key: "selfgrow-upgrade:" + short(pl.Commit),
		Text: fmt.Sprintf("自己长大：升级到 %s（任务 %s 合并的、过了评测门），接下来 %d 个任务比升级前差就自动回滚", to, pl.Job, observeTasks)})
	return nil
}

// build 在临时克隆里构建这个提交，返回程序和它报的版本。克隆、构建各有超时（这一步还没动安装）。
func (s *Service) build(commit string) (bin, version string, err error) {
	p, ok := s.selfProjectAny()
	if !ok {
		return "", "", errors.New("找不到 self_project")
	}
	tmp, err := os.MkdirTemp("", "agentq-selfgrow-")
	if err != nil {
		return "", "", err
	}
	bin, src := filepath.Join(tmp, "agentq"), filepath.Join(tmp, "src")
	ctx := context.Background()
	// 共享对象的独立克隆，不是 worktree：go 只认 .git 目录，worktree 里构建记不下提交号（版本成了 dev），也就认不出装的是哪一代
	if _, err := runGit(ctx, tmp, "clone", "-q", "--shared", "--no-checkout", p.Dir, src); err != nil {
		return bin, "", fmt.Errorf("克隆 %s：%w", p.Dir, err)
	}
	if _, err := runGit(ctx, src, "checkout", "-q", "--detach", commit); err != nil {
		return bin, "", fmt.Errorf("检出 %s：%w", short(commit), err)
	}
	args := []string{"build", "-o", bin}
	if tags := buildTags(); tags != "" {
		args = append(args, "-tags", tags)
	}
	bctx, cancel := context.WithTimeout(ctx, buildWait)
	defer cancel()
	cmd := exec.CommandContext(bctx, "go", append(args, ".")...)
	cmd.Dir, cmd.Env = src, proc.ChildEnv()
	proc.BoundCmd(cmd)
	out, err := cmd.CombinedOutput()
	proc.ClearLeftovers(cmd)
	if err != nil {
		return bin, "", fmt.Errorf("go build：%w：%s", err, textfmt.OneLine(lastBytes(out, 2048), 300))
	}
	version, err = versionOf(bin)
	if vm := devVersionRe.FindStringSubmatch(version); err != nil || vm == nil || vm[2] != "" || !strings.HasPrefix(commit, vm[1]) {
		return bin, "", fmt.Errorf("新构建报的版本是 %q，和提交 %s 对不上：%v", version, short(commit), err)
	}
	return bin, version, nil
}

// rollback：回到上一代，拉黑这个提交，推通知说原因，再写一条待做去找、修让任务变差的改动（selfgrow.go）。
// 自动更新装上的发行版本：回到上一代，这个版本以后不再自动装（autoupdate.go），不写待做（普通用户没有源码可修）。
func (s *Service) rollback(self string, pl upgradePlan) error {
	who, key := "自己长大", "rollback:"+short(pl.Commit)
	if pl.Release != "" {
		who, key = "自动更新", "autoupdate-rollback:"+pl.Release
	}
	if err := s.installer(self, "rollback", "--yes"); err != nil {
		if !errors.Is(err, errNotIdle) {
			s.appendUpgrade(UpgradeEvent{At: s.Now(), Kind: EvFail, From: s.Version, Commit: pl.Commit, Job: pl.Job, Release: pl.Release, Why: "回滚没做成：" + textfmt.OneLine(err.Error(), 300)})
			s.alert(key, who+"：升级到 "+s.Version+" 后变差（"+pl.Why+"），回滚没做成，要你看一下："+textfmt.OneLine(err.Error(), 200))
		}
		return err
	}
	to, err := versionOf(s.BinPath())
	if err != nil {
		to = "上一代"
	}
	now := s.Now()
	if pl.Release != "" {
		s.appendUpgrade(UpgradeEvent{At: now, Kind: EvRollback, From: s.Version, To: to, Release: pl.Release, Why: pl.Why + "；这个版本以后不再自动装，有更新的版本再说"})
		s.alert(key, "自动更新：装上 "+s.Version+" 后任务变差，已回滚到 "+to+"，这个版本以后不再自动装（有更新的版本再说）。"+pl.Why)
		return nil
	}
	s.appendUpgrade(UpgradeEvent{At: now, Kind: EvRollback, From: s.Version, To: to, Commit: pl.Commit, Job: pl.Job, Why: pl.Why + "；带着这个提交的版本不再升，等修它的那一条合并"})
	s.alert(key, "自己长大：升级到 "+s.Version+" 后任务变差，已回滚到 "+to+"，带着这次改动的版本不再升，已写待做去修。"+pl.Why)
	s.reportRegression(pl.Commit, s.Version, pl.Why, now)
	return nil
}

func (s *Service) alert(key, text string) {
	s.Notify.Notify(notify.Notice{Cat: "alert", Key: "selfgrow:" + key, Text: text})
}
