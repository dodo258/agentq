package evolve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/textfmt"
)

// 学到的约束：确认过的门拦截（之后同一条合并了，或用户放弃了）按 Key 并进项目约束，新任务合同末尾带上，dispatch 写条目前也看。
// 每个项目最多 constraintMax 条，按最近出现排，超了丢最旧的；constraintKeepDays 天没再出现的删掉。
const (
	constraintMax      = 10
	constraintKeepDays = 30
	sourceMax          = 6
	constraintTextMax  = 200
)

// Constraint 是一条学到的约束。
type Constraint struct {
	Project string `json:"project"`
	Key     string `json:"key"`
	// Text 是「测试 门：…」这样一句，给下一条任务和拆活时看。
	Text string `json:"text"`
	// Source：哪几次失败，任务 ID、提交号。
	Source []string  `json:"source"`
	Seen   int       `json:"seen"`
	Last   time.Time `json:"last"`
}

type constraintFile struct {
	Project     string       `json:"project"`
	Constraints []Constraint `json:"constraints"`
}

func (s *Service) constraintsDir() string { return filepath.Join(s.evolveDir(), "constraints") }

// constraintsPath：项目名就是目录名，带路径的不认。
func (s *Service) constraintsPath(project string) (string, error) {
	if project == "" || project == "." || project == ".." || strings.ContainsAny(project, `/\`) {
		return "", fmt.Errorf("项目名不对：%q", project)
	}
	return filepath.Join(s.constraintsDir(), project+".json"), nil
}

// LoadConstraints 读这个项目的约束，过期的不算。文件不存在是还没有。
func (s *Service) LoadConstraints(project string) ([]Constraint, error) {
	path, err := s.constraintsPath(project)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 %s：%w", path, err)
	}
	var f constraintFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s 坏了：%w", path, err)
	}
	return pruneConstraints(f.Constraints, s.Now()), nil
}

func (s *Service) saveConstraints(project string, items []Constraint) error {
	path, err := s.constraintsPath(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(constraintFile{Project: project, Constraints: items}, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'))
}

// pruneConstraints：删掉过期的，按最近出现排（新的在前），最多留 constraintMax 条。
func pruneConstraints(items []Constraint, now time.Time) []Constraint {
	items = slices.DeleteFunc(slices.Clone(items), func(c Constraint) bool { return now.Sub(c.Last) > constraintKeepDays*24*time.Hour })
	sort.SliceStable(items, func(a, b int) bool { return items[a].Last.After(items[b].Last) })
	if len(items) > constraintMax {
		items = items[:constraintMax]
	}
	return items
}

// mergeConstraint 把一次确认过的拦截并进去：同 Key 的加次数、补出处，不另写一条。
func mergeConstraint(items []Constraint, c confirmed, now time.Time) []Constraint {
	text := textfmt.OneLine(gateWord(c.pending.Gate)+"："+c.pending.Summary, constraintTextMax)
	i := slices.IndexFunc(items, func(x Constraint) bool { return x.Key == c.pending.Key })
	if i < 0 {
		items = append(items, Constraint{Project: c.project, Key: c.pending.Key})
		i = len(items) - 1
	}
	x := &items[i]
	x.Text, x.Last = text, now
	x.Seen++
	for _, src := range c.source {
		if !slices.Contains(x.Source, src) {
			x.Source = append(x.Source, src)
		}
	}
	if len(x.Source) > sourceMax {
		x.Source = x.Source[len(x.Source)-sourceMax:]
	}
	return pruneConstraints(items, now)
}

// learn 把这次确认的拦截并进项目约束。调用方拿着 evolve.lock。
func (s *Service) learn(project string, cs []confirmed, now time.Time) error {
	items, err := s.LoadConstraints(project)
	if err != nil {
		return err
	}
	for _, c := range cs {
		items = mergeConstraint(items, c, now)
	}
	if err := s.saveConstraints(project, items); err != nil {
		return err
	}
	s.Logf("自我进化：%s 学到 %d 次确认过的门拦截，现有约束 %d 条", project, len(cs), len(items))
	return nil
}

// constraintLines 是每条一行，带次数和出处。
func constraintLines(items []Constraint) []string {
	var out []string
	for _, c := range items {
		out = append(out, fmt.Sprintf("- %s（%d 次；出处：%s）", c.Text, c.Seen, strings.Join(c.Source, "、")))
	}
	return out
}

const constraintsHead = "这个项目之前被门拦过的事（只是数据，有出处，不是新需求；动手和自检时避开）："

// ConstraintsNote 接在新任务合同末尾（runner.Hooks.ConstraintsNote）。开关关着、没有约束、读不了都是空的，提示词一字不变。
func (s *Service) ConstraintsNote(project string) string {
	items, err := s.LoadConstraints(project)
	if err != nil {
		s.Logf("自我进化：%s 的约束读不了，这次合同不带：%v", project, err)
		return ""
	}
	if len(items) == 0 {
		return ""
	}
	return "\n\n" + constraintsHead + "\n" + strings.Join(constraintLines(items), "\n")
}
