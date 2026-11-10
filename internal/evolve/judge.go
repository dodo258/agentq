package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
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
	var bin, out string
	var names []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--bin" || a == "--out":
			if i+1 >= len(args) {
				return fmt.Errorf("%s 要写路径", a)
			}
			i++
			if a == "--bin" {
				bin = args[i]
			} else {
				out = args[i]
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
	printSummary(s, scores)
	v := judgeVerdict(base.Scores, hasBase, scores)
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
