package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 自我进化 E2：从本机已经落盘的数据里收信号，不调模型，不起进程。
//   - 来源：门的结果（gate/<任务>/last.json）、任务记录（jobs/<任务>/meta.json）按文件记，只读新出现或改过的；巡逻（patrol.json）只看上次之后首次发现的问题。
//   - 同类归并：Key 是种类 + 门名 + 去掉路径、哈希、数字后的原因前 60 字。同一份证据（任务、巡逻问题）只算一次。
//   - 门 error 档没过、退回了任务的拦截先记着；这一条后来合并了（门是对的）或用户放弃了，才算确认，并进项目约束（constraints.go）。
//     用户放行、再审过了的不算：放行记成 gate-override 信号，是门的问题。
//   - agentq 自己的信号（见 selfKind）同一 Key 出现两次以上，写一条提案（proposal.go），同一 Key 只提一次。
//   - 用户和真实使用里冒出来的问题（亲手 stop、门卡片上退回或放弃、验收后返工、巡逻疑似误报、retry_after 挡太久）见 usersignals.go。
//   - tick 每小时最多收一次；没有新数据就什么都不写，下次 tick 再看（stat 门结果和任务记录，读一次 patrol.json）。
const (
	collectEvery    = time.Hour
	signalKeepDays  = 30
	evidenceMax     = 20
	keyReasonRunes  = 60
	proposeAt       = 2
	proposedKeysMax = 500
	evolveLockWait  = 30 * time.Second
	// gateCrashMark：门进程连续没留下结果、转用户时原因的开头（gate.crashed 写的）。
	gateCrashMark = "门进程连续"
)

// 信号的种类。
const (
	KindGateFail     = "gate-fail"
	KindGateUnknown  = "gate-unknown"
	KindGateOverride = "gate-override"
	KindPlan         = "plan"
	KindStuck        = "stuck"
	KindTimeout      = "timeout"
	KindFailed       = "failed"
	KindCrash        = "crash"
	KindPatrol       = "patrol"
)

// selfKind：说的是 agentq 自己的问题，不分项目归并，够次数写提案。其余是项目自己的事。
func selfKind(kind string) bool {
	switch kind {
	case KindGateUnknown, KindGateOverride, KindPlan, KindCrash, KindTimeout, KindPatrol,
		KindUserStop, KindUserDecide, KindRework, KindPatrolGone, KindRetryWait:
		return true
	}
	return false
}

var kindWords = map[string]string{
	KindGateFail:     "门没过",
	KindGateUnknown:  "门判不了",
	KindGateOverride: "用户放行了门拦下的任务",
	KindPlan:         "退回用完还没过",
	KindStuck:        "卡在门上",
	KindTimeout:      "任务超时",
	KindFailed:       "任务失败",
	KindCrash:        "门进程没留下结果",
	KindPatrol:       "巡逻发现问题",
	KindUserStop:     "用户亲手停了任务",
	KindUserDecide:   "用户在门卡片上退回或放弃",
	KindRework:       "验收后返工",
	KindPatrolGone:   "巡逻报了又自己消失",
	KindRetryWait:    "条目被 retry_after 挡住超过 6 小时",
}

// Signal 是归并后的一类信号。
type Signal struct {
	Kind string `json:"kind"`
	// Project：项目自己的信号才有；agentq 自己的信号跨项目归并，项目写在证据里。
	Project  string    `json:"project,omitempty"`
	Key      string    `json:"key"`
	Summary  string    `json:"summary"`
	Evidence []string  `json:"evidence"`
	Seen     int       `json:"seen"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Proposed bool      `json:"proposed,omitempty"`
}

// pendingFail 是门 error 档没过、退回了任务的一次拦截，等这一条合并或被放弃才算确认。
type pendingFail struct {
	Job         string    `json:"job"`
	Project     string    `json:"project"`
	Fingerprint string    `json:"fingerprint"`
	Title       string    `json:"title"`
	Gate        string    `json:"gate"`
	Summary     string    `json:"summary"`
	Key         string    `json:"key"`
	Head        string    `json:"head,omitempty"`
	At          time.Time `json:"at"`
}

// SignalStore 是 ~/.agentq/evolve/signals.json。
type SignalStore struct {
	// Checked：上次收集到新数据、写下这个文件的时刻（agentq 的钟），控频用。
	Checked time.Time `json:"checked"`
	// Files：读过的门结果和任务记录（相对 ~/.agentq 的路径 → 修改时间和大小）。按文件记，不按一个时间游标：
	// 门进程写好 result.json、下一个 tick 才改名成 last.json，改名保留写的时间，游标会把它跳过去。
	Files map[string]string `json:"files,omitempty"`
	// PatrolAfter：读过的巡逻问题里最晚的首次发现时间。
	PatrolAfter time.Time     `json:"patrol_after"`
	Signals     []Signal      `json:"signals"`
	Pending     []pendingFail `json:"pending,omitempty"`
	ProposedKey []string      `json:"proposed_keys,omitempty"` // 提过案的 Key：信号过期删掉了也不再提
	// AuditAfter：读过的门审计日志里最晚的一行的时间（用户退回、放弃）。
	AuditAfter time.Time `json:"audit_after,omitzero"`
	// PatrolOpen：上次读到的、算 agentq 问题的巡逻问题。这次不在了、又是当天首次发现的，算疑似误报。
	PatrolOpen []patrolIssue `json:"patrol_open,omitempty"`
	// ReworkSeen：记过的返工意见（见 reworkKey）。意见留在「已完成」里，信号过期了也不再算。
	ReworkSeen []string `json:"rework_seen,omitempty"`
}

func (s *Service) evolveDir() string         { return filepath.Join(s.DataDir(), "evolve") }
func (s *Service) SignalsPath() string       { return filepath.Join(s.evolveDir(), "signals.json") }
func (s *Service) ProposalsPath() string     { return filepath.Join(s.evolveDir(), "proposals.md") }
func (s *Service) evolveLockPath() string    { return filepath.Join(s.evolveDir(), "evolve.lock") }
func (s *Service) patrolPath() string        { return filepath.Join(s.DataDir(), "patrol.json") }
func (s *Service) gateLast(id string) string { return filepath.Join(s.Gate.Dir(id), "last.json") }

var (
	keyPathRe = regexp.MustCompile(`[^\s，。；：、（）()「」"'` + "`" + `]*[/\\][^\s，。；：、（）()「」"'` + "`" + `]*`)
	keyHashRe = regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`)
	keyNumRe  = regexp.MustCompile(`[0-9]+`)
)

// reasonKey：原因去掉路径、哈希、数字，压成一行取前 60 字。同一类问题换了文件、行号、提交也归到一起。
func reasonKey(s string) string {
	s = keyPathRe.ReplaceAllString(s, "")
	s = keyHashRe.ReplaceAllString(s, "")
	s = keyNumRe.ReplaceAllString(s, "")
	return textfmt.OneLine(s, keyReasonRunes)
}

// SignalKey 是同类归并用的 Key。
func SignalKey(kind, gateName, reason string) string {
	return kind + "|" + gateName + "|" + reasonKey(reason)
}

// LoadSignals：文件不存在是还没收过；读不了返回错误。只读的命令看到坏文件就报错。
func (s *Service) LoadSignals() (*SignalStore, error) {
	b, err := os.ReadFile(s.SignalsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &SignalStore{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 %s：%w", s.SignalsPath(), err)
	}
	var st SignalStore
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w：%s：%v", errSignalsBad, s.SignalsPath(), err)
	}
	return &st, nil
}

var errSignalsBad = errors.New("信号文件坏了")

// loadForCollect：收集时坏文件备份成 signals.json.bad-<时间>、从空的开始，不然每个 tick 都卡在这里（提过的提案可能再提一次）。
func (s *Service) loadForCollect() (*SignalStore, error) {
	st, err := s.LoadSignals()
	if !errors.Is(err, errSignalsBad) {
		return st, err
	}
	b, rerr := os.ReadFile(s.SignalsPath())
	if rerr != nil {
		return nil, fmt.Errorf("读 %s：%w", s.SignalsPath(), rerr)
	}
	bak := s.SignalsPath() + ".bad-" + s.Now().Format("20060102-150405")
	if werr := fsx.WriteFileAtomic(bak, b); werr != nil {
		return nil, fmt.Errorf("%v；备份到 %s 也失败：%w", err, bak, werr)
	}
	if werr := os.Remove(s.SignalsPath()); werr != nil {
		return nil, fmt.Errorf("%v；已备份为 %s，但删不掉坏文件：%w", err, bak, werr)
	}
	s.Logf("自我进化：%v，已备份为 %s，从空的开始（提过的提案可能再提一次）", err, bak)
	return &SignalStore{}, nil
}

func (s *Service) saveSignals(st *SignalStore) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("写信号：%w", err)
	}
	if err := fsx.WriteFileAtomic(s.SignalsPath(), append(b, '\n')); err != nil {
		return fmt.Errorf("写信号：%w", err)
	}
	return nil
}

// add 记一次信号。同一份证据已经记过就不算，返回 false。
func (st *SignalStore) add(now time.Time, kind, project, key, summary, evidence string) bool {
	if selfKind(kind) {
		project = ""
	}
	for i := range st.Signals {
		sig := &st.Signals[i]
		if sig.Key != key || sig.Project != project {
			continue
		}
		if slices.Contains(sig.Evidence, evidence) {
			return false
		}
		sig.Evidence = append(sig.Evidence, evidence)
		if len(sig.Evidence) > evidenceMax {
			sig.Evidence = sig.Evidence[len(sig.Evidence)-evidenceMax:]
		}
		sig.Seen++
		sig.Last = now
		sig.Summary = summary
		return true
	}
	st.Signals = append(st.Signals, Signal{Kind: kind, Project: project, Key: key, Summary: summary, Evidence: []string{evidence}, Seen: 1, First: now, Last: now,
		Proposed: slices.Contains(st.ProposedKey, key)})
	return true
}

// prune：30 天没再出现的信号和没等到结论的拦截删掉。
func (st *SignalStore) prune(now time.Time) {
	old := func(t time.Time) bool { return now.Sub(t) > signalKeepDays*24*time.Hour }
	st.Signals = slices.DeleteFunc(st.Signals, func(s Signal) bool { return old(s.Last) })
	st.Pending = slices.DeleteFunc(st.Pending, func(p pendingFail) bool { return old(p.At) })
	if len(st.ProposedKey) > proposedKeysMax {
		st.ProposedKey = st.ProposedKey[len(st.ProposedKey)-proposedKeysMax:]
	}
}

// gateWord：「测试门」这样的叫法。
func gateWord(name string) string {
	if name == "" {
		return "门"
	}
	return name + " 门"
}

// CollectIfDue：tick 调用。开关开着、离上次收到新数据满一小时才收；出错只记日志，不影响 tick。
func (s *Service) CollectIfDue() {
	defer func() { // 收集出了没想到的错也不能让 tick 非 0 退出
		if p := recover(); p != nil {
			s.Logf("自我进化：收信号出错，本次作废，不影响 tick：%v", p)
		}
	}()
	if _, err := s.Collect(false); err != nil {
		s.Logf("自我进化：%v", err)
	}
}

// Collect 收一次信号。force 为假时离上次收到新数据不满一小时就不收。返回这次有没有新数据（有才写文件）。
func (s *Service) Collect(force bool) (bool, error) {
	st, err := s.loadForCollect()
	if err != nil {
		return false, err
	}
	now := s.Now()
	if !force && !st.Checked.IsZero() && !now.Before(st.Checked) && now.Sub(st.Checked) < collectEvery {
		return false, nil
	}
	// tick 和命令都没有上层 ctx：读 git 的那几次各有超时
	in := s.scan(context.Background(), st, now)
	if in.empty() {
		return false, nil
	}
	if err := os.MkdirAll(s.evolveDir(), 0o700); err != nil {
		return false, fmt.Errorf("建 %s：%w", s.evolveDir(), err)
	}
	lock, err := fsx.AcquireLock(s.evolveLockPath(), evolveLockWait)
	if err != nil {
		return false, fmt.Errorf("拿 %s：%w", s.evolveLockPath(), err)
	}
	if lock == nil {
		return false, fmt.Errorf("等了 %s 还拿不到 %s", evolveLockWait, s.evolveLockPath())
	}
	defer lock.Close()
	if st, err = s.LoadSignals(); err != nil { // 拿着锁再读一次：手动命令可能刚写过
		return false, err
	}
	learned := map[string][]confirmed{}
	for _, g := range in.gates {
		s.absorbGate(st, g, now)
	}
	for _, j := range in.metas {
		for _, c := range s.absorbMeta(st, j, now) {
			learned[c.project] = append(learned[c.project], c)
		}
	}
	if in.patrolRead {
		s.absorbPatrol(st, in.patrol, now)
		for _, is := range in.patrolGone {
			s.absorbPatrolGone(st, is, now)
		}
		st.PatrolOpen = in.patrolOpen
	}
	for _, d := range in.decisions {
		s.absorbDecision(st, d, now)
	}
	if in.auditAfter.After(st.AuditAfter) {
		st.AuditAfter = in.auditAfter
	}
	for _, r := range in.rework {
		s.absorbRework(st, r, now)
	}
	for _, w := range in.retry {
		s.absorbRetryWait(st, w, now)
	}
	for project, cs := range learned {
		if err := s.learn(project, cs, now); err != nil {
			s.Logf("自我进化：%s 的约束没存下：%v", project, err)
		}
	}
	st.prune(now)
	s.propose(st, now)
	st.Checked = now
	st.Files = in.files
	if err := s.saveSignals(st); err != nil {
		return true, err
	}
	return true, nil
}

// gateObs 是一份新的门结果。
type gateObs struct {
	job *state.Job
	res gate.RunResult
	at  time.Time
}

type patrolIssue struct {
	Fingerprint string    `json:"fingerprint"`
	Check       string    `json:"check"`
	Title       string    `json:"title"`
	FirstSeen   time.Time `json:"first_seen"`
}

type scanned struct {
	gates      []gateObs
	metas      []*state.Job
	patrol     []patrolIssue
	patrolRead bool          // patrol.json 读出来了（问题可能一个都没有）
	patrolOpen []patrolIssue // 这次还在的、算 agentq 问题的巡逻问题
	patrolGone []patrolIssue // 当天报了又消失的
	decisions  []gateDecision
	auditAfter time.Time
	rework     []reworkItem
	retry      []retryWait
	files      map[string]string // 这次看到的文件，写回 SignalStore.Files
	touched    bool              // 有没读过的文件，或巡逻、审计日志、BACKLOG、状态文件里有新的
}

func (in *scanned) empty() bool { return !in.touched }

// scan 读没读过的（新的、改过的）门结果和任务记录，和巡逻的新发现。读不了、坏了的跳过：信号是可有可无的事，不挡别的。
// 没记过、30 天前就改过的文件不看（第一次收集只看最近 30 天）；文件没了就从记录里去掉。
func (s *Service) scan(ctx context.Context, st *SignalStore, now time.Time) *scanned {
	in := &scanned{files: map[string]string{}}
	fresh := func(path string) (time.Time, bool) {
		fi, err := os.Stat(path)
		if err != nil {
			return time.Time{}, false
		}
		rel, err := filepath.Rel(s.DataDir(), path)
		if err != nil {
			return time.Time{}, false
		}
		sig := fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
		old, known := st.Files[rel]
		if !known && now.Sub(fi.ModTime()) > signalKeepDays*24*time.Hour {
			return time.Time{}, false
		}
		in.files[rel] = sig
		if known && old == sig {
			return time.Time{}, false
		}
		in.touched = true
		return fi.ModTime(), true
	}
	lasts, _ := filepath.Glob(filepath.Join(s.DataDir(), "gate", "*", "last.json")) // 模式是固定的，不会报错
	for _, p := range lasts {
		at, ok := fresh(p)
		if !ok {
			continue
		}
		id := filepath.Base(filepath.Dir(p))
		j := s.readMeta(filepath.Join(s.JobDir(id), "meta.json"))
		b, err := os.ReadFile(p)
		var res gate.RunResult
		if j == nil || err != nil || json.Unmarshal(b, &res) != nil {
			continue
		}
		in.gates = append(in.gates, gateObs{job: j, res: res, at: at})
	}
	sort.SliceStable(in.gates, func(a, b int) bool { return in.gates[a].at.Before(in.gates[b].at) })
	metas, _ := filepath.Glob(filepath.Join(s.DataDir(), "jobs", "*", "meta.json"))
	type metaAt struct {
		j  *state.Job
		at time.Time
	}
	var ms []metaAt
	for _, p := range metas {
		at, ok := fresh(p)
		if !ok {
			continue
		}
		if j := s.readMeta(p); j != nil {
			ms = append(ms, metaAt{j, at})
		}
	}
	sort.SliceStable(ms, func(a, b int) bool { return ms[a].at.Before(ms[b].at) })
	for _, m := range ms {
		in.metas = append(in.metas, m.j)
	}
	// 巡逻每小时都重写 patrol.json：不看文件时间，看有没有上次之后首次发现、算 agentq 问题的
	if b, err := os.ReadFile(s.patrolPath()); err == nil {
		var ps struct {
			Issues []patrolIssue `json:"issues"`
		}
		if json.Unmarshal(b, &ps) == nil {
			in.patrol, in.patrolRead = ps.Issues, true
			if slices.ContainsFunc(ps.Issues, func(is patrolIssue) bool { return is.FirstSeen.After(st.PatrolAfter) && selfPatrol(is.Check) }) {
				in.touched = true
			}
			s.scanPatrolGone(st, ps.Issues, now, in)
		}
	}
	s.scanDecisions(st, now, in)
	s.scanRework(ctx, st, in)
	s.scanRetryWait(st, now, in)
	return in
}

// readMeta 读一份任务记录。读不了、坏了、和目录名对不上的是 nil。
func (s *Service) readMeta(path string) *state.Job {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var j state.Job
	if json.Unmarshal(b, &j) != nil || j.ID == "" || j.ID != filepath.Base(filepath.Dir(path)) {
		return nil
	}
	return &j
}

// absorbGate：一份门结果。error 档没过的记成 gate-fail，同时记下等确认；判不了的记成 gate-unknown。
func (s *Service) absorbGate(st *SignalStore, g gateObs, now time.Time) {
	j := g.job
	ev := "任务 " + j.ID + "（" + j.Project + "）"
	for _, r := range g.res.Results {
		if r.Defer != "" || r.Cached {
			continue
		}
		switch {
		case g.res.Outcome == "fail" && r.Outcome == gate.Fail && r.Level == gate.LevelError:
			key := SignalKey(KindGateFail, r.Gate, r.Summary)
			summary := gateWord(r.Gate) + "：" + textfmt.OneLine(r.Summary, 200)
			st.add(now, KindGateFail, j.Project, key, summary, ev)
			if !slices.ContainsFunc(st.Pending, func(p pendingFail) bool { return p.Job == j.ID && p.Key == key }) {
				st.Pending = append(st.Pending, pendingFail{Job: j.ID, Project: j.Project, Fingerprint: j.Fingerprint, Title: j.Title,
					Gate: r.Gate, Summary: textfmt.OneLine(r.Summary, 200), Key: key, Head: g.res.Head, At: now})
			}
		case r.Outcome == gate.Unknown:
			st.add(now, KindGateUnknown, j.Project, SignalKey(KindGateUnknown, r.Gate, r.Summary), gateWord(r.Gate)+"判不了："+textfmt.OneLine(r.Summary, 200), ev)
		}
	}
}

// confirmed 是一次确认过的拦截：门是对的。
type confirmed struct {
	project string
	pending pendingFail
	source  []string
}

// sameItem：拦截和这个任务做的是同一条（退回后续跑、接手的是新任务，指纹或标题对得上）。
func sameItem(p pendingFail, j *state.Job) bool {
	return p.Project == j.Project && (p.Fingerprint != "" && p.Fingerprint == j.Fingerprint || p.Title != "" && p.Title == j.Title)
}

// takePending 把这一条等确认的拦截都取出来。
func (st *SignalStore) takePending(j *state.Job) []pendingFail {
	var out []pendingFail
	st.Pending = slices.DeleteFunc(st.Pending, func(p pendingFail) bool {
		if sameItem(p, j) {
			out = append(out, p)
			return true
		}
		return false
	})
	return out
}

func short(commit string) string { return textfmt.TruncRunes(commit, 12) }

// absorbMeta：一份任务记录。返回这次确认的拦截。
func (s *Service) absorbMeta(st *SignalStore, j *state.Job, now time.Time) []confirmed {
	ev := "任务 " + j.ID + "（" + j.Project + "）"
	var out []confirmed
	if g := j.Gate; g != nil {
		switch g.State {
		case state.GateMerged:
			pend := st.takePending(j)
			switch {
			case g.Passes > 0 || g.Override != "":
				// 用户放行：门拦错了，不学；记成门的问题。合并后收尾还会再写 meta.json：这个任务记过一次就不再记，
				// 不然第二次读时等确认的拦截已经取走，Key 变了，同一次放行会算成另一类
				if slices.ContainsFunc(st.Signals, func(sig Signal) bool { return sig.Kind == KindGateOverride && slices.Contains(sig.Evidence, ev) }) {
					break
				}
				gateName, why := "", g.Override
				if len(pend) > 0 {
					gateName, why = pend[0].Gate, pend[0].Summary
				}
				st.add(now, KindGateOverride, j.Project, SignalKey(KindGateOverride, gateName, why), "用户放行了"+gateWord(gateName)+"拦下的任务："+textfmt.OneLine(why, 200), ev)
			case g.Reruns > 0: // 再审过了：门前后结论不一样，说明不了是对的
			default:
				for _, p := range pend {
					src := []string{"任务 " + p.Job, "任务 " + j.ID}
					if g.Merge != "" {
						src = append(src, "提交 "+short(g.Merge))
					}
					out = append(out, confirmed{project: j.Project, pending: p, source: src})
				}
			}
		case state.GateAbandoned:
			for _, p := range st.takePending(j) {
				src := []string{"任务 " + p.Job, "任务 " + j.ID + "（用户放弃）"}
				if p.Head != "" {
					src = append(src, "提交 "+short(p.Head))
				}
				out = append(out, confirmed{project: j.Project, pending: p, source: src})
			}
		case state.GateStuck:
			switch {
			case strings.HasPrefix(g.Report, gateCrashMark):
				st.add(now, KindCrash, j.Project, SignalKey(KindCrash, "", g.Report), "门进程没留下结果："+textfmt.OneLine(g.Report, 200), ev)
			case g.Round >= gate.MaxRounds:
				st.add(now, KindPlan, j.Project, SignalKey(KindPlan, "", ""), fmt.Sprintf("退回 %d 轮还没过，多半是条目拆法或验收的问题：%s", g.Round, textfmt.OneLine(g.Report, 200)), ev)
			default:
				st.add(now, KindStuck, j.Project, SignalKey(KindStuck, "", g.Report), "卡在门上："+textfmt.OneLine(g.Report, 200), ev)
			}
		}
	}
	s.absorbStop(st, j, now)
	switch j.Status {
	case state.StatusTimeout:
		kind := j.Kind
		if kind == "" {
			kind = "待做"
		}
		st.add(now, KindTimeout, j.Project, SignalKey(KindTimeout, kind, ""), kind+"任务超时："+textfmt.OneLine(j.Summary, 200), ev)
	case state.StatusFailed:
		st.add(now, KindFailed, j.Project, SignalKey(KindFailed, "", j.Summary), "任务失败："+textfmt.OneLine(j.Summary, 200), ev)
	}
	return out
}

// selfPatrol：这类巡逻问题算 agentq 的问题。等用户决定、额度、任务失败（任务记录里已经有）、磁盘、外部推送脚本不算。
func selfPatrol(check string) bool {
	switch check {
	case "decide", "usage", "job", "disk", "autopush":
		return false
	}
	return true
}

// absorbPatrol：巡逻上次之后新发现的问题。
func (s *Service) absorbPatrol(st *SignalStore, issues []patrolIssue, now time.Time) {
	for _, is := range issues {
		if !is.FirstSeen.After(st.PatrolAfter) || !selfPatrol(is.Check) {
			continue
		}
		st.add(now, KindPatrol, "", SignalKey(KindPatrol, is.Check, is.Title), "巡逻："+textfmt.OneLine(is.Title, 200), "巡逻 "+is.Fingerprint+" "+is.FirstSeen.Format("01-02 15:04"))
	}
	for _, is := range issues {
		if is.FirstSeen.After(st.PatrolAfter) {
			st.PatrolAfter = is.FirstSeen
		}
	}
}
