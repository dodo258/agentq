package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gate"
)

// baselineHint 是没有能比的基线时告诉用户怎么建。
const baselineHint = "先 agentq eval run --fake，再 agentq eval baseline"

// cmdJudge 是评测门调的（agentq eval judge）：拿运行中这一代编译进去的题和判分，用 --fake 跑法（含程序题）评判 --bin 那个候选，
// 再和基线比。结论写进 --out，评测门读它定结果。结果不进 runs/：eval baseline 默认取最近一次，候选的分不能被当成基线。
func (s *Service) cmdJudge(args []string) error {
	var bin, out, newDir string
	var names, newIDs, domains []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--bin" || a == "--out" || a == "--new-dir" || a == "--new" || a == "--domain":
			if i+1 >= len(args) {
				return fmt.Errorf("%s 后面要写值", a)
			}
			i++
			switch a {
			case "--bin":
				bin = args[i]
			case "--out":
				out = args[i]
			case "--new-dir":
				newDir = args[i]
			case "--new":
				newIDs = append(newIDs, args[i])
			default:
				domains = append(domains, args[i])
			}
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("不认识的参数 %s", a)
		default:
			names = append(names, a)
		}
	}
	if bin == "" {
		fmt.Fprint(s.Out, evalUsage)
		return errors.New("eval judge 要写 --bin：被评判的 agentq")
	}
	bin, err := resolveBin(bin)
	if err != nil {
		return err
	}
	base, hasBase, err := s.loadBaseline() // 先读：基线坏了不白跑一遍
	if err != nil {
		return err
	}
	public, held, err := s.loadCases()
	if err != nil {
		return err
	}
	picked, err := Select(append(append([]Case{}, public...), held...), names)
	if err != nil {
		return err
	}
	fresh, err := loadNewCases(newDir, newIDs, append(public, held...))
	if err != nil {
		return err
	}
	if len(domains) > 0 && len(fresh) == 0 {
		return errors.New("--domain 要和 --new-dir、--new 一起写：长能力要带新考题")
	}
	cases, skipped := planRun(picked, ModeFake)
	for _, c := range skipped {
		fmt.Fprintf(s.Out, "%s 没有假做法，没跑\n", c.ID)
	}
	if len(cases) == 0 {
		return errors.New("没有要跑的题")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.Out, "评判 %s：%d 题（%s），题目和判分取自运行中这一代，调模型：否\n", s.Tilde(bin), len(cases), countKinds(cases))
	scores, err := s.runCases(ctx, s.newHarness(bin, ModeFake), cases)
	if err != nil {
		return err
	}
	var before, after []Score
	if len(fresh) > 0 {
		self, err := resolveBin("")
		if err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "新考题 %d 道：先在运行中这一代上跑，再在候选上跑\n", len(fresh))
		if before, err = s.runCases(ctx, s.newHarness(self, ModeFake), fresh); err != nil {
			return err
		}
		if after, err = s.runCases(ctx, s.newHarness(bin, ModeFake), fresh); err != nil {
			return err
		}
	}
	printSummary(s, scores)
	v := judgeVerdict(base.Scores, hasBase, scores)
	if len(domains) > 0 {
		capabilityVerdict(&v, domains, base.Scores, scores, before, after)
	}
	switch {
	case !v.Baseline:
		fmt.Fprintf(s.Out, "没有能比的基线，只记录：%s\n", v.Hint)
	default:
		fmt.Fprintf(s.Out, "和基线比了 %d 题\n", v.Compared)
	}
	printComparison(s, Comparison{Worse: v.Worse, Better: v.Better, Notes: v.Notes})
	if out != "" {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("写结论：%w", err)
		}
		if err := fsx.WriteFileAtomic(out, append(raw, '\n')); err != nil {
			return fmt.Errorf("写结论：%w", err)
		}
	}
	if len(v.Worse) > 0 {
		fmt.Fprintf(s.Out, "变差了 %d 处\n", len(v.Worse))
		return env.ErrLogged
	}
	fmt.Fprintln(s.Out, "没有变差")
	return nil
}

// judgeVerdict 把这次的分和基线比成评测门的结论。基线里一题都比不上（比如只有 --real 的分）和没有基线一样：只记录。留出题只写编号。
func judgeVerdict(base []Score, hasBase bool, scores []Score) gate.EvalVerdict {
	v := gate.EvalVerdict{Cases: len(scores)}
	labels := heldLabels(base, scores)
	for _, sc := range scores {
		if sc.Skipped != "" || (sc.judged() && sc.Correct) {
			continue
		}
		name := sc.Case
		if sc.Held {
			name = labels[sc.Case]
		}
		v.Failed = append(v.Failed, name)
	}
	if !hasBase {
		v.Hint = "还没有基线：" + baselineHint
		return v
	}
	cmp := Compare(base, scores)
	if cmp.Compared == 0 {
		v.Hint = "基线里没有 --fake 和程序题的分：" + baselineHint
		v.Notes = cmp.Notes
		return v
	}
	v.Baseline, v.Compared = true, cmp.Compared
	v.Worse, v.Better, v.Notes = cmp.Worse, cmp.Better, cmp.Notes
	return v
}

// loadNewCases 读候选新加的题（--new-dir 下的 --new 题号）。只认运行中这一代没有的题：已有的题用这一代的原件。
func loadNewCases(dir string, ids []string, known []Case) ([]Case, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if dir == "" {
		return nil, errors.New("--new 要和 --new-dir 一起写")
	}
	root := os.DirFS(dir)
	var out []Case
	for _, id := range ids {
		if !caseDirRe.MatchString(id) {
			return nil, fmt.Errorf("新题 %s：名字要是「题号-短名」", id)
		}
		if slices.ContainsFunc(known, func(c Case) bool { return c.ID == id }) {
			return nil, fmt.Errorf("新题 %s 运行中这一代已经有了：题只加不改", id)
		}
		sub, err := fs.Sub(root, id)
		if err != nil {
			return nil, fmt.Errorf("新题 %s：%w", id, err)
		}
		c, err := loadCase(sub, id)
		if err != nil {
			return nil, fmt.Errorf("新题 %s：%w", id, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// domainRate 是一组分数的通过题数和平均分。判不了的题算没过、0 分：新题在候选上判不了，说明不了能力长出来了。
func domainRate(scores []Score) (correct, n int, points float64) {
	for _, sc := range scores {
		if sc.Skipped != "" {
			continue
		}
		n++
		if sc.judged() {
			if sc.Correct {
				correct++
			}
			points += float64(sc.Points)
		}
	}
	if n > 0 {
		points /= float64(n)
	}
	return correct, n, points
}

// capabilityVerdict：长能力要这个领域涨分。基线一侧是基线里这个领域的老题加上新题在运行中这一代上的分，
// 候选一侧是同一批老题这次的分加上新题在候选上的分；通过率或平均分比基线高才算涨。别的领域掉没掉由 Compare 管。
func capabilityVerdict(v *gate.EvalVerdict, domains []string, base, cur, before, after []Score) {
	v.Cases += len(after)
	curBy := map[string]Score{}
	for _, c := range cur {
		curBy[c.Case+"|"+c.Mode] = c
	}
	for _, dom := range domains {
		var b, c []Score
		for _, bs := range base {
			cs, ok := curBy[bs.Case+"|"+bs.Mode]
			if bs.Held || bs.Domain != dom || !ok || !bs.judged() || cs.Skipped != "" {
				continue
			}
			b, c = append(b, bs), append(c, cs)
		}
		nNew := 0
		for i := range after {
			if after[i].Domain != dom {
				continue
			}
			nNew++
			b, c = append(b, before[i]), append(c, after[i])
			if !after[i].Correct {
				v.Failed = append(v.Failed, after[i].Case)
			}
		}
		name := DomainLabel(dom)
		if nNew == 0 {
			v.Worse = append(v.Worse, "领域 "+name+"：没有新加这个领域的考题")
			continue
		}
		bc, bn, bp := domainRate(b)
		cc, cn, cp := domainRate(c)
		line := fmt.Sprintf("领域 %s：加上 %d 道新题后通过 %d/%d → %d/%d，平均分 %.1f → %.1f", name, nNew, bc, bn, cc, cn, bp, cp)
		if cc*bn > bc*cn || cp > bp {
			v.Better = append(v.Better, line)
			continue
		}
		v.Worse = append(v.Worse, line+"，没涨：新题要在运行中这一代上判没过、在候选上判过")
	}
}
