// Package flow 是从一句话到合进主分支的那条直线。每一步做完存盘，被打断了从存下的那一步接着做。
package flow

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"agentq/internal/claude"
	"agentq/internal/codex"
	"agentq/internal/lock"
	"agentq/internal/order"
	"agentq/internal/repo"
	"agentq/internal/todo"
)

const (
	redTries      = 3
	conflictTries = 2
	landTries     = 5
	checkTimeout  = 30 * time.Minute // 跑一次测试的上限
	landWait      = time.Minute      // 主目录挡着时隔多久再试
)

// Flow 做一条需求。
type Flow struct {
	Claude claude.Runner
	// Lead 想方案、修难的、合到一起；Code 写普通模块。
	Lead, Code string
	// Last 只在 Lead 修了 redTries 次还红时出手一次。
	Last  string
	Codex codex.Auditor
	Say   func(string)
	Log   func(string, ...any)
	// Source 是 agentq 自己的源码目录；在那里合进主分支后调 Upgrade 换上新程序。
	Source  string
	Upgrade func(*order.Order)
	// Next 接着做这个项目待办里能做的。
	Next func(*order.Order)
	// Locks 放各仓库合主分支用的锁。
	Locks string

	mergeMu sync.Mutex
}

// Run 从现在这一步做到底。
func (f *Flow) Run(ctx context.Context, o *order.Order) error {
	err := f.line(ctx, o)
	if err == nil || ctx.Err() != nil { // 被停或机器关机：留在这一步，下次接着
		return err
	}
	if why := err.Error(); !strings.HasPrefix(why, "要你定") && !strings.Contains(why, "出手一次还没过") && !strings.HasPrefix(why, "审计的 bug 没修好") {
		f.self(o, "卡住："+why) // 要你定、测试修不好是项目的事，其余是 agentq 自己没跑顺
	}
	if serr := o.Update(func() { o.State, o.Why, o.Ended = order.Stuck, err.Error(), time.Now() }); serr != nil {
		return errors.Join(err, serr)
	}
	f.Say(fmt.Sprintf("🔴 卡住了 · %s\n\n%s　%s\n%s\n接着做：agentq retry %s", day(), o.Project, Title(o), claude.Short(err.Error(), 200), o.ID))
	return err
}

func (f *Flow) line(ctx context.Context, o *order.Order) error {
	if err := f.plan(ctx, o); err != nil {
		return err
	}
	all := f.tree(o, "")
	if err := repo.Worktree(o.Dir, all, branch(o, ""), o.Base); err != nil {
		return err
	}
	if err := repo.Keep(all); err != nil { // 上次打断时留下的、人在现场改的，都留着
		return err
	}
	if err := f.file(o); err != nil {
		return err
	}
	for _, step := range []func(context.Context, *order.Order) error{f.build, f.green, f.audit, f.land} {
		if err := step(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// plan：产品经理出方案。
func (f *Flow) plan(ctx context.Context, o *order.Order) error {
	if o.Plan != nil {
		return nil
	}
	base, err := repo.Git(o.Dir, "rev-parse", "refs/heads/"+o.Main)
	if err != nil {
		return err
	}
	f.step(o, order.Planning, "")
	dir := f.tree(o, "plan")
	drop, err := repo.Aside(o.Dir, dir, base) // 只许看：另开一份，不在用户的目录里跑
	if err != nil {
		return err
	}
	defer drop()
	rep, err := f.ask(ctx, o, "plan", claude.Req{Dir: dir, Prompt: planPrompt(o), Schema: planSchema, Effort: "high"})
	if err != nil {
		return err
	}
	p, err := parsePlan(rep.JSON)
	if err != nil {
		return err
	}
	if err := o.Update(func() { o.Plan, o.Base = p, base }); err != nil {
		return err
	}
	if p.Stop != "" {
		return errors.New("要你定：" + p.Stop)
	}
	f.hand(o, "产品经理", "交出方案，划成 %d 块：%s", len(p.Modules), p.Summary)
	if !p.Small {
		f.Say(fmt.Sprintf("📋 开工 · %s\n\n%s　%s\n分 %d 块：%s", day(), o.Project, p.Title, len(p.Modules), claude.Short(p.Summary, 120)))
	}
	return nil
}

// file 把大活的方案和后面几期写进项目：以后做的人看文件，不靠谁记得。
func (f *Flow) file(o *order.Order) error {
	if o.Plan.Small || o.Plan.Filed {
		return nil
	}
	all := f.tree(o, "")
	dir := filepath.Join(all, "docs", "specs")
	spec, text := filepath.Join(dir, o.Started.Format("2006-01-02")+"-"+o.ID+".md"), "# "+o.Plan.Title+"\n\n> 需求原话："+oneLine(o.Text)+"\n\n"+o.Plan.Spec+"\n"
	if err := errors.Join(os.MkdirAll(dir, 0o755), os.WriteFile(spec, []byte(text), 0o644)); err != nil {
		return err
	}
	if len(o.Plan.Later) > 0 {
		if err := todo.Add(all, o.Plan.Later); err != nil {
			return err
		}
	}
	if err := repo.Commit(all, "方案："+o.Plan.Title); err != nil {
		return err
	}
	return o.Update(func() { o.Plan.Filed = true })
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// parsePlan 读方案，模块名要能当分支名，依赖要指得到、不打转。
func parsePlan(raw json.RawMessage) (*order.Plan, error) {
	var p order.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("产品经理没给出合格的方案：%w", err)
	}
	if p.Stop != "" {
		return &p, nil
	}
	if len(p.Modules) == 0 {
		return nil, errors.New("产品经理的方案里没有模块")
	}
	names := map[string]bool{}
	for i := range p.Modules {
		m := &p.Modules[i]
		m.Name = slug(m.Name)
		if !nameRe.MatchString(m.Name) || names[m.Name] {
			return nil, fmt.Errorf("方案里的模块名不能用：%q", m.Name)
		}
		names[m.Name] = true
		for j, n := range m.Needs {
			m.Needs[j] = slug(n)
		}
	}
	for i, m := range p.Modules {
		p.Modules[i].Needs = slices.DeleteFunc(m.Needs, func(n string) bool { return !names[n] || n == m.Name })
	}
	seen := map[string]bool{}
	for range p.Modules {
		for _, m := range p.Modules {
			if ready(m, seen) {
				seen[m.Name] = true
			}
		}
	}
	if len(seen) != len(p.Modules) {
		return nil, errors.New("方案里的模块互相等，谁都开不了工")
	}
	return &p, nil
}

// slug 把模块名写成能当分支名的样子。
func slug(name string) string {
	return strings.ToLower(strings.NewReplacer("_", "-", " ", "-", "/", "-").Replace(strings.TrimSpace(name)))
}

func ready(m order.Module, done map[string]bool) bool {
	return !slices.ContainsFunc(m.Needs, func(n string) bool { return !done[n] })
}

// build：依赖都合好的模块同时开工，做完就合到一起。一块失败，别的照样做完，最后一起报。
func (f *Flow) build(ctx context.Context, o *order.Order) error {
	f.step(o, order.Building, "")
	type result struct {
		name string
		err  error
	}
	ch := make(chan result)
	running, merged, tasks := 0, map[string]bool{}, map[string]order.Task{}
	for name, t := range o.Tasks {
		merged[name], tasks[name] = t.Merged, *t
	}
	var failed []error
	started := map[string]bool{}
	for {
		for _, m := range o.Plan.Modules {
			if started[m.Name] || merged[m.Name] || !ready(m, merged) {
				continue
			}
			started[m.Name] = true
			running++
			go func() { ch <- result{m.Name, f.module(ctx, o, m, tasks[m.Name])} }()
		}
		if running == 0 {
			break
		}
		r := <-ch
		running--
		if merged[r.name] = r.err == nil; r.err != nil {
			failed = append(failed, fmt.Errorf("%s：%w", r.name, r.err))
			f.Log("模块 %s 没做成：%v", r.name, r.err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, m := range o.Plan.Modules {
		if !started[m.Name] && !merged[m.Name] {
			failed = append(failed, fmt.Errorf("%s：等的那块没做成，没开工", m.Name))
		}
	}
	return errors.Join(failed...)
}

// module：一个模块从做到合进这条需求的分支。t 是上次做到哪了。
func (f *Flow) module(ctx context.Context, o *order.Order, m order.Module, t order.Task) error {
	dir, br, all := f.tree(o, m.Name), branch(o, m.Name), f.tree(o, "")
	from, err := repo.Git(all, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err := repo.Worktree(o.Dir, dir, br, from); err != nil {
		return err
	}
	q := claude.Req{Dir: dir, Prompt: buildPrompt(o, m), Resume: t.Session != "", Effort: "high", Model: f.writer(m)}
	if q.Resume {
		q.Prompt = "刚才被打断了，接着把这一块做完并提交。先看一眼现在的进度，别从头来。"
	}
	t.Session, t.From = cmp.Or(t.Session, claude.NewSession()), cmp.Or(t.From, from)
	q.Session = t.Session
	if err := f.saveTask(o, m.Name, t); err != nil {
		return err
	}
	f.Log("开工：%s", m.Name)
	f.hand(o, m.Name, "接到：%s（只动 %s）", m.Goal, strings.Join(m.Paths, "、"))
	rep, err := f.ask(ctx, o, m.Name, q)
	if err != nil && q.Resume { // 会话没存下来：在现有进度上开新的
		q.Session, q.Resume = claude.NewSession(), false
		q.Prompt = buildPrompt(o, m) + "\n这个分支上可能已经做了一部分，先看进度，接着做。"
		rep, err = f.ask(ctx, o, m.Name, q)
	}
	if err != nil {
		return err
	}
	t.Session = rep.Session
	if err := f.saveTask(o, m.Name, t); err != nil {
		return err
	}
	if err := f.tidy(ctx, o, m.Name, dir, f.writer(m), &t); err != nil {
		return err
	}
	if !changed(dir, t.From) {
		return errors.New("会话结束了但什么都没提交：" + oneLine(rep.Text))
	}
	for n := 0; ; n++ {
		err := f.join(all, br, m.Name)
		if err == nil {
			break
		}
		if !errors.Is(err, repo.ErrConflict) {
			return err
		}
		if n >= conflictTries {
			return errors.New("合到一起有冲突，解了几次没解开")
		}
		f.hand(o, m.Name, "打回：合到一起有冲突，让它自己解决")
		if _, err := f.ask(ctx, o, m.Name+"-conflict", claude.Req{Dir: dir, Prompt: conflictPrompt(branch(o, "")), Session: t.Session, Resume: true, Effort: "high", Model: f.writer(m)}); err != nil {
			return err
		}
		if err := f.tidy(ctx, o, m.Name, dir, f.writer(m), &t); err != nil {
			return err
		}
	}
	t.Merged = true
	f.hand(o, m.Name, "交回：%s", lastPara(rep.Text))
	if o.Plan.Small || len(o.Plan.Modules) == 1 {
		if err := o.Update(func() { o.How = lastPara(rep.Text) }); err != nil {
			return err
		}
	}
	return f.saveTask(o, m.Name, t)
}

// join 合进这条需求的分支。一次一个；有冲突退回干净的样子。
func (f *Flow) join(all, br, name string) error {
	f.mergeMu.Lock()
	defer f.mergeMu.Unlock()
	err := repo.Merge(all, br, "合并模块 "+name)
	if errors.Is(err, repo.ErrConflict) {
		if _, aerr := repo.Git(all, "merge", "--abort"); aerr != nil {
			return aerr
		}
	}
	return err
}

// tidy：会话结束了还有没提交的，让它收一次尾。
func (f *Flow) tidy(ctx context.Context, o *order.Order, name, dir, model string, t *order.Task) error {
	if repo.Dirty(dir) == "" && !repo.InMerge(dir) {
		return nil
	}
	_, err := f.ask(ctx, o, name+"-tidy", claude.Req{Dir: dir, Prompt: "工作目录里还有没提交的东西（git status）。该提交的提交，不要的清掉，做到工作目录干净。", Session: t.Session, Resume: true, Effort: "medium", Model: model})
	if err != nil {
		return err
	}
	if d := repo.Dirty(dir); d != "" || repo.InMerge(dir) {
		return errors.New("会话结束了还有没提交的改动：" + oneLine(d))
	}
	return nil
}

// green：在合到一起的代码上跑测试，红了修，最多 redTries 次。
func (f *Flow) green(ctx context.Context, o *order.Order) error {
	for n := 0; ; n++ {
		f.step(o, order.Testing, "")
		log := filepath.Join(o.WorkDir(), fmt.Sprintf("verify-%s.log", time.Now().Format("150405")))
		ok, note, err := repo.Check(ctx, f.tree(o, ""), log, claude.Env(), checkTimeout)
		if err = errors.Join(err, ctx.Err()); err != nil {
			return err
		}
		if ok {
			f.hand(o, "测试", "过了 %s", note)
			return nil
		}
		f.hand(o, "测试", "打回：%s，日志 %s", note, log)
		who, name := f.Lead, "red"
		switch {
		case n == redTries:
			who, name = f.Last, "last"
			f.hand(o, "测试", "%s 修了 %d 次还没过，请 %s 出手一次", f.Lead, redTries, f.Last)
		case n > redTries:
			return fmt.Errorf("%s，%s 修了 %d 次、%s 出手一次还没过，日志 %s", note, f.Lead, redTries, f.Last, log)
		}
		f.step(o, order.Fixing, note)
		if _, err := f.fixIn(ctx, o, name, who, redPrompt(log, note), ""); err != nil {
			return err
		}
	}
}

// fixIn 在合到一起的代码上起一个会话修东西，要它提交干净。
func (f *Flow) fixIn(ctx context.Context, o *order.Order, name, model, prompt, schema string) (claude.Reply, error) {
	all := f.tree(o, "")
	effort := "high"
	if model == f.Last { // 只出手一次，想到底
		effort = "xhigh"
	}
	rep, err := f.ask(ctx, o, name, claude.Req{Dir: all, Prompt: prompt, Schema: schema, Effort: effort, Model: model})
	if err != nil {
		return rep, err
	}
	return rep, f.tidy(ctx, o, name, all, model, &order.Task{Session: rep.Session})
}

// audit：全面审一次，大 bug 一次修掉，再跑测试。不再审。小改动不审。
func (f *Flow) audit(ctx context.Context, o *order.Order) error {
	if o.Plan.Small {
		return nil
	}
	if !o.Audited {
		f.step(o, order.Auditing, "")
		dir := f.tree(o, "audit")
		drop, err := repo.Aside(o.Dir, dir, branch(o, "")) // 另开一份，审的时候写了什么、提交了什么都不算数
		if err != nil {
			return err
		}
		defer drop()
		raw, err := f.Codex.Ask(ctx, dir, auditPrompt(o), auditSchema, filepath.Join(o.WorkDir(), "codex-audit"))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil { // 没装、没登录、没额度：换 Claude 审，不为它停下
			f.hand(o, "审计", "%v，换 Claude 审", err)
			f.self(o, "Codex 审计没跑成："+err.Error())
			rep, err := f.ask(ctx, o, "audit", claude.Req{Dir: dir, Prompt: auditPrompt(o), Schema: auditSchema, Effort: "xhigh"})
			if err != nil {
				return err
			}
			raw = rep.JSON
		}
		var got struct {
			Summary string      `json:"summary"`
			Bugs    []order.Bug `json:"bugs"`
			Risks   []string    `json:"risks"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			return fmt.Errorf("审计没给出合格的结论：%w", err)
		}
		f.hand(o, "审计", "%d 个大 bug。%s", len(got.Bugs), got.Summary)
		for _, b := range got.Bugs {
			f.hand(o, "审计", "打回 %s：%s %s", b.Module, b.Where, b.What)
		}
		if err := o.Update(func() { o.Audited, o.Bugs, o.Risks = true, got.Bugs, got.Risks }); err != nil {
			return err
		}
	}
	if len(o.Bugs) == 0 || o.Fixed {
		return nil
	}
	f.step(o, order.Fixing, fmt.Sprintf("审计找到 %d 个大 bug", len(o.Bugs)))
	all := f.tree(o, "")
	before, err := repo.Git(all, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	rep, err := f.fixIn(ctx, o, "fix", f.Lead, fixPrompt(o), fixSchema)
	if err != nil {
		return err
	}
	var got struct {
		Results []struct {
			Result string `json:"result"`
			Note   string `json:"note"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rep.JSON, &got); err != nil || len(got.Results) != len(o.Bugs) {
		return errors.New("修审计的 bug，会话没逐条说清结果")
	}
	invalid, left := 0, ""
	for i, r := range got.Results {
		f.hand(o, "修", "%s：%s（%s）", o.Bugs[i].Where, r.Note, r.Result)
		switch r.Result {
		case "invalid":
			invalid++
		case "failed":
			left += "\n" + o.Bugs[i].Where + "：" + r.Note
		}
	}
	if left != "" {
		return errors.New("审计的 bug 没修好：" + left)
	}
	if invalid < len(o.Bugs) && !changed(all, before) {
		return errors.New("会话说修了审计的 bug，但什么都没提交")
	}
	if err := f.green(ctx, o); err != nil {
		return err
	}
	return o.Update(func() { o.Fixed, o.Invalid = true, invalid })
}

// land：合进主分支。主目录有改动挡着就等，等的时候不占着别人的路。
func (f *Flow) land(ctx context.Context, o *order.Order) error {
	for told := false; ; {
		blocked, err := f.tryLand(ctx, o)
		if err != nil {
			return err
		}
		if blocked == "" {
			return f.landed(o)
		}
		f.step(o, order.Landing, "主目录有改动挡着："+blocked)
		if !told {
			told = true
			f.Say(fmt.Sprintf("🟠 等合并 · %s\n\n%s　%s\n做好了，%s 所在的目录有没提交的改动挡着，提交或挪开后自动合", day(), o.Project, Title(o), o.Main))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(landWait):
		}
	}
}

// tryLand 试一次。主分支动过就先合进来，待办打勾，动过内容就再跑一次测试。挡着的返回原因。
func (f *Flow) tryLand(ctx context.Context, o *order.Order) (string, error) {
	all, br := f.tree(o, ""), branch(o, "")
	f.step(o, order.Landing, "")
	if err := os.MkdirAll(f.Locks, 0o700); err != nil {
		return "", err
	}
	held, err := lock.Flock(filepath.Join(f.Locks, strings.ReplaceAll(strings.Trim(o.Dir, "/"), "/", "-")), true)
	if err != nil {
		return "", err
	}
	defer held.Close()
	for n := 0; ; {
		moved := !repo.Has(all, "refs/heads/"+o.Main)
		if moved {
			if n++; n > landTries {
				return "", errors.New(o.Main + " 一直在动，合了几次都被抢先")
			}
			if err := repo.Merge(all, o.Main, "合入最新 "+o.Main); errors.Is(err, repo.ErrConflict) {
				if _, err := f.fixIn(ctx, o, "land-conflict", f.Lead, "当前目录正在合入最新的 "+o.Main+"，有冲突（git status）。解决冲突，两边的功能都要在，跑有关的测试，提交这次合并。不借这次改别的。"+ground, ""); err != nil {
					return "", err
				}
			} else if err != nil {
				return "", err
			}
		}
		ticked, err := f.tick(o)
		if err != nil {
			return "", err
		}
		if moved || ticked { // 合进去的要和测过的一样
			if err := f.green(ctx, o); err != nil {
				return "", err
			}
			f.step(o, order.Landing, "")
		}
		err = repo.Land(o.Dir, o.Main, br)
		if err == nil {
			return "", nil
		}
		if repo.Has(all, "refs/heads/"+o.Main) { // 不是刚好又有人合了
			return oneLine(err.Error()), nil
		}
	}
}

// landed：合进去了，告诉用户，清现场。
func (f *Flow) landed(o *order.Order) error {
	commit, err := repo.Git(f.tree(o, ""), "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	if err := o.Update(func() { o.State, o.Why, o.Commit, o.Ended = order.Done, "", commit, time.Now() }); err != nil {
		return err
	}
	f.hand(o, "合并", "进了 %s，%s", o.Main, commit)
	f.Say(report(o))
	if o.Chain {
		f.Next(o)
	}
	if f.Upgrade != nil && o.Dir == f.Source {
		f.Upgrade(o)
	}
	for _, m := range append(o.Plan.Modules, order.Module{}) { // 最后那个没名字的是合到一起的
		if err := repo.Remove(o.Dir, f.tree(o, m.Name), branch(o, m.Name), o.Main); err != nil {
			f.Log("%v", err)
		}
	}
	return nil
}

// self 记一条 agentq 自己的毛病，同一条只记一次。
func (f *Flow) self(o *order.Order, what string) {
	what = claude.Short(what, 200)
	if err := o.Update(func() {
		if !slices.Contains(o.Self, what) {
			o.Self = append(o.Self, what)
		}
	}); err != nil {
		f.Log("状态没存下：%v", err)
	}
}

// writer 是写这一块的模型。
func (f *Flow) writer(m order.Module) string {
	if m.Hard {
		return f.Lead
	}
	return f.Code
}

// tick 给出自待办的那条打勾，返回有没有提交东西。
func (f *Flow) tick(o *order.Order) (bool, error) {
	if o.Todo == "" {
		return false, nil
	}
	all := f.tree(o, "")
	if ok, err := todo.Tick(all, o.Todo, oneLine(how(o))); err != nil || !ok {
		f.hand(o, "待办", "没找到「%s」，没打勾", o.Todo)
		f.self(o, "待办没打上勾：「"+o.Todo+"」")
		return false, err
	}
	_, err := repo.Git(all, "commit", "-q", "-m", "待办：做完「"+o.Todo+"」", "--", todo.File)
	return err == nil, err
}

// ask 起会话，记用量。
func (f *Flow) ask(ctx context.Context, o *order.Order, name string, q claude.Req) (claude.Reply, error) {
	q.Out = filepath.Join(o.WorkDir(), name+"-"+time.Now().Format("0102150405.000"))
	q.Model = cmp.Or(q.Model, f.Lead)
	q.Name = "agentq " + o.ID + " " + o.Project + " " + name
	q.Log = func(format string, a ...any) { // 等额度、重试要在 ls 里看得到
		f.Log(format, a...)
		if err := o.Update(func() { o.Why = fmt.Sprintf(format, a...) }); err != nil {
			f.Log("状态没存下：%v", err)
		}
	}
	rep, err := f.Claude.Run(ctx, q)
	if serr := o.Update(func() {
		o.Tokens += rep.Tokens
		o.Used = append(o.Used, order.Use{At: time.Now(), Tokens: rep.Tokens})
	}); serr != nil {
		err = errors.Join(err, serr)
	}
	return rep, err
}

// hand 在交接单上记一笔：谁，接到、交回还是打回了什么。
func (f *Flow) hand(o *order.Order, who, format string, a ...any) {
	line := fmt.Sprintf("- %s **%s** %s\n", time.Now().Format("01-02 15:04"), who, oneLine(fmt.Sprintf(format, a...)))
	w, err := os.OpenFile(filepath.Join(o.WorkDir(), "HANDOFF.md"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		_, err = w.WriteString(line)
		err = errors.Join(err, w.Close())
	}
	if err != nil {
		f.Log("交接单没记上：%v", err)
	}
}

func (f *Flow) saveTask(o *order.Order, name string, t order.Task) error {
	return o.Update(func() { o.Tasks[name] = &t })
}

func (f *Flow) step(o *order.Order, state, why string) {
	f.Log("%s %s", state, why)
	if err := o.Update(func() { o.State, o.Why = state, why }); err != nil {
		f.Log("状态没存下：%v", err)
	}
}

// part 是这条需求的一个模块，没写是合到一起的。
func part(module string) string {
	if module == "" {
		return "all"
	}
	return "m-" + module
}

func branch(o *order.Order, module string) string { return "agentq/" + o.ID + "/" + part(module) }

// tree 放在 agentq 自己的目录里：放进用户的仓库，git add -A 会把它当子模块提交。
func (f *Flow) tree(o *order.Order, module string) string {
	return filepath.Join(o.WorkDir(), "trees", part(module))
}

// changed：dir 的内容和 from 不一样。
func changed(dir, from string) bool { return !repo.OK(dir, "diff", "--quiet", from, "HEAD") }

// Title 是这条需求的叫法。
func Title(o *order.Order) string {
	if o.Plan != nil && o.Plan.Title != "" {
		return o.Plan.Title
	}
	return claude.Short(o.Text, 40)
}

// how 是做了什么、怎么看。
var howRe = regexp.MustCompile(`怎么看[：:]\s*(.+)`)

// how 是给用户的一句「怎么看」：会话最后写的那一行，没有就用方案的总结。
func how(o *order.Order) string {
	if m := howRe.FindAllStringSubmatch(o.How, -1); m != nil {
		return claude.Short(strings.Trim(m[len(m)-1][1], "*"), 160)
	}
	return claude.Short(cmp.Or(o.How, o.Plan.Summary), 160)
}

// report 是做好了的那条消息：几行，一行一件事。
func report(o *order.Order) string {
	s := fmt.Sprintf("🟢 做好了 · %s\n\n%s　%s\n怎么看：%s\n用了 %s · 提交 %s", day(), o.Project, Title(o), how(o), o.Ended.Sub(o.Started).Round(time.Minute), o.Commit)
	if n := len(o.Bugs); n > 0 {
		s += fmt.Sprintf(" · 审计 %d 个大 bug，修了 %d 个", n, n-o.Invalid)
	}
	if len(o.Risks) > 0 {
		s += "\n\n⚠️ 留意"
	}
	for _, x := range o.Risks[:min(3, len(o.Risks))] {
		s += "\n" + claude.Short(x, 80)
	}
	return s
}

func day() string { return time.Now().Format("1月2日") }

func lastPara(s string) string {
	p := strings.Split(strings.TrimSpace(s), "\n\n")
	return strings.TrimSpace(p[len(p)-1])
}

func oneLine(s string) string { return claude.Short(s, 300) }
