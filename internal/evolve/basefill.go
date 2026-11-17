package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 基线跟着新题补上：新一代编译进来的题（修 bug 时新加的那种）基线里没有，评测门就只写「基线里没有这题，没比」，起不到防回归的作用。
// 自己长大或自动更新开着、已经有基线、这一代过了观察期（或没有在观察的升级）、没有任务在跑或在过门时，
// tick 起一个脱离自己的 agentq eval baseline --fill：用运行中这一代按 --fake 跑法（含程序题）只跑基线里还没有的题，
// 通过了的才并进基线；已有题的分不动，没过、判不了的不进。不调模型。每一代只试一次（fill.json 记着），没过的等下一代再试。

func (s *Service) fillLockPath() string  { return filepath.Join(s.evalDir(), "fill.lock") }
func (s *Service) fillLogPath() string   { return filepath.Join(s.evalDir(), "fill.log") }
func (s *Service) fillStatePath() string { return filepath.Join(s.evalDir(), "fill.json") }

// fillState 记哪一代试过哪些题：同一代没过的不再每小时重跑。
type fillState struct {
	Version string   `json:"version"`
	Tried   []string `json:"tried"`
}

// scoreKey 是一题在基线里的键：程序题的跑法记成 program，模型题按这次的跑法。
func scoreKey(c Case, mode string) string {
	if c.Kind == KindProgram {
		mode = ModeProgram
	}
	return c.ID + "|" + mode
}

// missingCases：--fake 跑得了、基线里还没有的题。
func missingCases(cases []Case, base []Score) []Case {
	have := map[string]bool{}
	for _, b := range base {
		have[b.Case+"|"+b.Mode] = true
	}
	run, _ := planRun(cases, ModeFake)
	var out []Case
	for _, c := range run {
		if !have[scoreKey(c, ModeFake)] {
			out = append(out, c)
		}
	}
	return out
}

// FillBaseline 把这次通过了、基线里还没有的题并进基线，返回新基线和补进的题。基线里已有的（同一题同一种跑法）一分不动，没过、判不了的不进。
func FillBaseline(base, cur []Score) ([]Score, []string) {
	have := map[string]bool{}
	for _, b := range base {
		have[b.Case+"|"+b.Mode] = true
	}
	var add []Score
	var added []string
	for _, sc := range cur {
		key := sc.Case + "|" + sc.Mode
		if have[key] || !sc.judged() || !sc.Correct {
			continue
		}
		have[key] = true
		add, added = append(add, sc), append(added, sc.Case)
	}
	if len(add) == 0 {
		return base, nil
	}
	return MergeBaseline(base, add), added
}

// fillDue：tick 该不该起补基线。返回空是该起；否则是不起的原因（给测试看，不进日志：每个 tick 都会走一遍）。
func (s *Service) fillDue(st *state.State) string {
	if !s.SelfGrowOn() && !s.AutoUpdateOn() {
		return "新一代不是自动装的"
	}
	base, ok, err := s.loadBaseline()
	switch {
	case err != nil:
		return err.Error()
	case !ok:
		return "还没有基线"
	}
	public, held, err := s.loadCases()
	if err != nil {
		return err.Error()
	}
	missing := missingCases(append(public, held...), base.Scores)
	if f, err := s.loadFill(); err == nil && f.Version == s.Version {
		missing = slices.DeleteFunc(missing, func(c Case) bool { return slices.Contains(f.Tried, c.ID) })
	}
	if len(missing) == 0 {
		return "基线里没有缺的题"
	}
	evs, err := s.LoadUpgrades()
	if err != nil {
		return err.Error()
	}
	if openUpgrade(evs, s.Version) != nil {
		return "这一代还在观察期"
	}
	if st.ActiveJobs() > 0 {
		return "有任务在跑或在过门"
	}
	if _, _, held := fsx.LockHolder(s.fillLockPath()); held {
		return "正在补"
	}
	return ""
}

// FillBaselineIfDue：tick 收自我进化时调用，该补就起 agentq eval baseline --fill。出错只记日志，不影响 tick。
func (s *Service) FillBaselineIfDue() {
	st, err := state.Load(s.StatePath())
	if err != nil {
		return // 状态读不了当未知：不起
	}
	if s.fillDue(st) != "" {
		return
	}
	if err := s.spawnFill(); err != nil {
		s.Logf("评测基线：起不来 agentq eval baseline --fill：%v", err)
	}
}

// spawnFill 起脱离 tick 的 agentq eval baseline --fill，输出写 fill.log。
func (s *Service) spawnFill() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到自己：%w", err)
	}
	if err := os.MkdirAll(s.evalDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(s.fillLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "eval", "baseline", "--fill")
	cmd.Dir = s.evalDir()
	cmd.Env = proc.ChildEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // tick 还活着时不留僵尸，退出后交给 launchd 回收
	s.Logf("评测基线：起了 agentq eval baseline --fill（pid %d），补基线里还没有的题", cmd.Process.Pid)
	return nil
}

func (s *Service) loadFill() (fillState, error) {
	var f fillState
	b, err := os.ReadFile(s.fillStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// cmdFill 是 agentq eval baseline --fill：用运行中这一代按 --fake 跑法跑基线里还没有的题，通过的并进基线。
func (s *Service) cmdFill() error {
	if err := os.MkdirAll(s.evalDir(), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(s.fillLockPath(), 0)
	if err != nil {
		return fmt.Errorf("拿 %s：%w", s.fillLockPath(), err)
	}
	if lock == nil {
		fmt.Fprintln(s.Out, "已经有一个在补基线")
		return nil
	}
	defer lock.Close()
	base, ok, err := s.loadBaseline()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("还没有基线：" + baselineHint + "；--fill 只往已有的基线里补")
	}
	public, held, err := s.loadCases()
	if err != nil {
		return err
	}
	missing := missingCases(append(public, held...), base.Scores)
	if len(missing) == 0 {
		fmt.Fprintln(s.Out, "基线里没有缺的题")
		return nil
	}
	self, err := resolveBin("")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.Out, "基线里缺 %d 题（%s），用运行中这一代 %s 跑（--fake），调模型：否\n", len(missing), countKinds(missing), s.Version)
	h := s.newHarness(self, ModeFake)
	scores, err := s.runCases(ctx, h, missing)
	if err != nil {
		return err
	}
	if scores, err = s.retryEnv(ctx, h, missing, scores); err != nil {
		return err
	}
	// 跑的这几分钟里有人手动 eval baseline 过：以磁盘上最新的为准再补
	if base, _, err = s.loadBaseline(); err != nil {
		return err
	}
	var added []string
	base.Scores, added = FillBaseline(base.Scores, scores)
	if len(added) > 0 {
		base.Updated = s.Now()
		raw, err := json.MarshalIndent(base, "", "  ")
		if err != nil {
			return fmt.Errorf("写基线：%w", err)
		}
		if err := fsx.WriteFileAtomic(s.baselinePath(), append(raw, '\n')); err != nil {
			return fmt.Errorf("写基线：%w", err)
		}
	}
	tried := make([]string, 0, len(missing))
	for _, c := range missing {
		tried = append(tried, c.ID)
	}
	if raw, err := json.Marshal(fillState{Version: s.Version, Tried: tried}); err == nil {
		if err := fsx.WriteFileAtomic(s.fillStatePath(), append(raw, '\n')); err != nil {
			s.Logf("评测基线：%s 没写上，下次 tick 会再跑一遍：%v", s.Tilde(s.fillStatePath()), err)
		}
	}
	labels := heldIndex(missing)
	var left []string
	for _, sc := range scores {
		if !slices.Contains(added, sc.Case) {
			name := sc.Case
			if l, ok := labels[sc.Case]; ok {
				name = l
			}
			left = append(left, name)
		}
	}
	line := fmt.Sprintf("评测基线：补进 %d 题，现在共 %d 题", len(added), len(base.Scores))
	if len(left) > 0 {
		line += fmt.Sprintf("；%d 题没过或判不了，没进（%s），这一代不再试", len(left), textfmt.OneLine(strings.Join(left, "、"), 300))
	}
	fmt.Fprintln(s.Out, line)
	s.Logf("%s", line)
	return nil
}
