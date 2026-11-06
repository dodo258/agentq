package sense

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/textfmt"
)

// SenseVerdict 是模型输出的一条。
type SenseVerdict struct {
	Refs     []string `json:"refs"`
	Verdict  string   `json:"verdict"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Evidence string   `json:"evidence"`
}

// parseSenseOutput：必须正好是 {"items":[…]}，字段、类型都对；缺失、畸形、多出字段一律报错（一条都不写）。
// 没有 structured_output 时试 result 文本（同样严格）。
func parseSenseOutput(res backend.Result) ([]SenseVerdict, error) {
	raw := bytes.TrimSpace(res.Structured)
	if len(raw) == 0 || string(raw) == "null" {
		raw = bytes.TrimSpace([]byte(res.Text))
	}
	if len(raw) == 0 {
		return nil, errors.New("没有结构化输出")
	}
	var out struct {
		Items *[]SenseVerdict `json:"items"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, errors.New("结构化输出格式不对")
	}
	if dec.More() {
		return nil, errors.New("结构化输出后面还有多余内容")
	}
	if out.Items == nil {
		return nil, errors.New("结构化输出里没有 items")
	}
	if len(*out.Items) > SenseMaxItems {
		return nil, fmt.Errorf("结构化输出有 %d 条，超过上限 %d", len(*out.Items), SenseMaxItems)
	}
	return *out.Items, nil
}

// copiesRaw：text 和某段原文有连续 senseCopyRun 个字相同（比较时去掉空白、标点，不分大小写）。
func copiesRaw(text string, raws []string) bool {
	k := []rune(backlog.TitleKey(text))
	if len(k) < senseCopyRun {
		return false
	}
	for _, raw := range raws {
		nr := backlog.TitleKey(raw)
		for i := 0; i+senseCopyRun <= len(k); i++ {
			if strings.Contains(nr, string(k[i:i+senseCopyRun])) {
				return true
			}
		}
	}
	return false
}

// senseLinkRe：写进 BACKLOG 的文字里不能有链接、域名路径和常见的命令写法（多半是照着外部文字写的）。
var senseLinkRe = regexp.MustCompile(`(?i)://|www\.|\b[a-z0-9-]+\.[a-z]{2,}/|\b(curl|wget|sudo|chmod|bash|zsh)\b|\brm\s+-|\bsh\s+-c\b|\|\s*sh\b`)

var senseSeverityCN = map[string]string{"high": "高", "medium": "中", "low": "低"}

// senseJudge 校验模型输出：refs 必须都在这批里、同一项目、没被别条用过（项目由 ref 决定，不信模型），verdict、severity 在枚举内。
// 只有 bug 变成候选。带链接或照抄原文的文字整段丢掉，换成代码写的一句话：真问题不丢，原文不进 BACKLOG。
// 返回候选、各判断的条数、没通过的说明，和被合格判断覆盖的 ref（没通过校验的那条里的 ref 不算）。
func senseJudge(vs []SenseVerdict, batch senseBatch, rawRef string, now time.Time) (cands []SenseCandidate, counts map[string]int, skipped []string, covered map[string]bool) {
	counts = map[string]int{}
	covered = map[string]bool{}
	byRef := map[string]senseBatchItem{}
	for _, it := range batch.Items {
		byRef[it.Ref] = it
	}
	used := map[string]bool{}
	for i, v := range vs {
		fail := func(why string) { skipped = append(skipped, fmt.Sprintf("第 %d 条：%s", i+1, why)) }
		if !slices.Contains([]string{"bug", "feature", "praise", "suspicious", "noise"}, v.Verdict) {
			fail("verdict 不在枚举里")
			continue
		}
		if senseSeverityCN[v.Severity] == "" {
			fail("severity 不在枚举里")
			continue
		}
		if len(v.Refs) == 0 {
			fail("没有 refs")
			continue
		}
		project, kind, bad := "", "", ""
		var raws []string
		self := map[string]bool{}
		for _, r := range v.Refs {
			it, ok := byRef[r]
			switch {
			case !ok:
				bad = "ref 不在这批里"
			case used[r] || self[r]:
				bad = "ref 被用了两次"
			case project != "" && it.Project != project:
				bad = "refs 跨项目"
			}
			if bad != "" {
				break
			}
			self[r] = true
			project, kind = it.Project, it.Kind
			raws = append(raws, it.Title, it.Body, it.Nickname)
		}
		if bad != "" {
			fail(bad)
			continue
		}
		var keys []string
		for _, r := range v.Refs {
			used[r], covered[r] = true, true
			keys = append(keys, byRef[r].Key)
		}
		counts[v.Verdict]++
		if v.Verdict != "bug" {
			continue
		}
		rawTitle := strings.TrimSpace(strings.ToValidUTF8(v.Title, ""))
		title, summary, evidence := backlog.SuggestField(rawTitle, senseTitleMax), backlog.SuggestField(v.Summary, senseFieldMax), backlog.SuggestField(v.Evidence, senseEvidMax)
		unsafe := func(s string) bool { return senseLinkRe.MatchString(s) || copiesRaw(s, raws) }
		if title == "" || len([]rune(rawTitle)) > senseTitleMax || unsafe(v.Title) {
			sum := sha1.Sum([]byte(keys[0])) // 按条目算：同一天另一批的 r1 不能和它撞标题，否则会被当重复丢掉
			title = fmt.Sprintf("%s反馈待看（%s %s）", kind, now.Format("01-02"), hex.EncodeToString(sum[:3]))
		}
		if summary == "" || unsafe(v.Summary) {
			summary = fmt.Sprintf("模型判为真问题，但它写的摘要带链接或照抄原文，已丢弃，看原文文件里的 %s", strings.Join(keys[:min(len(keys), 3)], " "))
		}
		line := fmt.Sprintf("%s —— %s；轻重：%s；", title, summary, senseSeverityCN[v.Severity])
		if evidence != "" && !unsafe(v.Evidence) {
			line += "依据：" + evidence + "；"
		}
		line += fmt.Sprintf("来源：%s %d 条；原文 %s", kind, len(v.Refs), rawRef)
		cands = append(cands, SenseCandidate{Project: project, Title: title, Line: line})
	}
	return cands, counts, skipped, covered
}

// loadSenseBatch 读任务目录外的批次文件。
func (a *Service) loadSenseBatch(id string) (senseBatch, error) {
	var batch senseBatch
	raw, err := os.ReadFile(a.senseBatchPath(id))
	if err != nil {
		return batch, err
	}
	return batch, json.Unmarshal(raw, &batch)
}

// senseSettle 记下这批的结果：被合格判断覆盖的记成见过；没覆盖的次数加一，下次再判断（受每天次数上限约束），
// 超过 senseMissMax 次就放弃：也记成见过，写一行进当天汇总，用户能看到。返回放弃了几条。
func (a *Service) senseSettle(j *state.Job, batch senseBatch, covered map[string]bool) int {
	var gaveUp []string
	err := a.WithSense(func(s *SenseStore) error {
		if s.Sources == nil {
			s.Sources = map[string]*senseSource{}
		}
		if s.Misses == nil {
			s.Misses = map[string]int{}
		}
		for _, it := range batch.Items {
			if !covered[it.Ref] {
				s.Misses[it.Key]++
				if s.Misses[it.Key] <= senseMissMax {
					continue
				}
				gaveUp = append(gaveUp, it.Key)
			}
			src := s.Sources[it.Source]
			if src == nil {
				src = &senseSource{Baseline: it.At}
				s.Sources[it.Source] = src
			}
			src.mark(it.Key, it.At)
			delete(s.Misses, it.Key)
		}
		return nil
	})
	if err != nil {
		a.Logf("主动感知：任务 %s 这批的结果没记下（下次会重复判断，重复的标题不会重复写）：%v", j.ID, err)
		return 0
	}
	if len(gaveUp) > 0 {
		msg := fmt.Sprintf("主动感知：%d 条外部内容判断了 %d 次都没有合格结果，放弃，不再判断；要看去原文文件 %s：%s",
			len(gaveUp), senseMissMax+1, a.Tilde(batch.RawPath), textfmt.OneLine(strings.Join(gaveUp, " "), 300))
		a.Logf("%s", msg)
		a.Ledger.AppendNightlyLine(a.Now(), msg)
	}
	return len(gaveUp)
}

// SenseResult 在判断任务收尾时调用（拿着 tick.lock）。结果文字可能带外部原文，这里写的摘要只有数字和路径。
// 输出合格才写「## 建议」，只把判断到的记成见过；其余下次再判断。模型的问题（输出不合格、撞回合或预算上限）算一次没判断，接口的问题不算。
func (a *Service) SenseResult(j *state.Job, res backend.Result, status, summary string) (string, string) {
	batch, berr := a.loadSenseBatch(j.ID)
	if status != state.StatusDone {
		if res.Stop.SelfLimit() {
			if berr == nil {
				a.senseSettle(j, batch, nil)
			}
			return status, "撞到回合或预算上限，这批下次再判断"
		}
		return status, strings.TrimPrefix(summary+"；这批下次再判断", "；")
	}
	if berr != nil {
		a.Logf("主动感知：任务 %s 的批次文件读不了，这批下次再判断：%v", j.ID, berr)
		return state.StatusFailed, "批次文件读不了，这批下次再判断"
	}
	vs, err := parseSenseOutput(res)
	if err != nil {
		a.Logf("主动感知：任务 %s %v，一条都不写，这批下次再判断", j.ID, err)
		a.senseSettle(j, batch, nil)
		return state.StatusFailed, "一条都没写：" + err.Error()
	}
	cands, counts, skipped, covered := senseJudge(vs, batch, a.Tilde(batch.RawPath), a.Now())
	for _, s := range skipped {
		a.Logf("主动感知：任务 %s 跳过一条：%s", j.ID, s)
	}
	gaveUp := a.senseSettle(j, batch, covered)
	written := a.senseWrite(cands)
	msg := fmt.Sprintf("这批 %d 条，判断了 %d 条：真问题 %d、功能建议 %d、夸奖 %d、可疑 %d、没信息量 %d；写进「建议」%d 条，原文 %s",
		len(batch.Items), len(covered), counts["bug"], counts["feature"], counts["praise"], counts["suspicious"], counts["noise"], len(written), a.Tilde(batch.RawPath))
	if len(skipped) > 0 {
		msg += fmt.Sprintf("；%d 条输出不合格被跳过", len(skipped))
	}
	if n := len(batch.Items) - len(covered) - gaveUp; n > 0 {
		msg += fmt.Sprintf("；%d 条模型没判断，下次再判断", n)
	}
	if gaveUp > 0 {
		msg += fmt.Sprintf("；%d 条放弃", gaveUp)
	}
	a.noticeSense(written)
	return state.StatusDone, msg
}

// senseWrite 把候选按项目写进「## 建议」（每个项目一次提交），写不进去的放进 Pending。返回写进去的「编号　项目　标题」。
func (a *Service) senseWrite(cands []SenseCandidate) []string {
	var written []string
	var pending []SenseCandidate
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		a.Logf("主动感知：扫描 projects_root 失败，%d 条建议下次再写：%v", len(cands), err)
		pending = cands
	} else {
		byName := map[string]backlog.Project{}
		for _, p := range projects {
			byName[p.Name] = p
		}
		groups := map[string][]SenseCandidate{}
		var order []string
		for _, c := range cands {
			if _, ok := groups[c.Project]; !ok {
				order = append(order, c.Project)
			}
			groups[c.Project] = append(groups[c.Project], c)
		}
		for _, name := range order {
			p, ok := byName[name]
			if !ok {
				a.Logf("主动感知：项目 %s 不在了，它的 %d 条建议不写", textfmt.OneLine(name, 40), len(groups[name]))
				continue
			}
			lines, rest, err := a.SenseCommit(p, groups[name])
			if err != nil {
				a.Logf("主动感知：%s 的建议没写进去，下次再写：%v", name, err)
				pending = append(pending, groups[name]...)
				continue
			}
			written = append(written, lines...)
			pending = append(pending, rest...)
		}
	}
	if len(pending) == 0 {
		return written
	}
	if err := a.WithSense(func(s *SenseStore) error {
		s.Pending = append(s.Pending, pending...)
		if n := len(s.Pending) - sensePendingMax; n > 0 {
			a.Logf("主动感知：攒着的建议超过 %d 条，丢掉最早的 %d 条（原文还在原文文件里）", sensePendingMax, n)
			s.Pending = s.Pending[n:]
		}
		return nil
	}); err != nil {
		a.Logf("主动感知：没写进去的 %d 条建议存不下来，丢了：%v", len(pending), err)
	}
	return written
}

// SenseCommit 写一个项目的建议：复用闲时找活的编号和「## 建议」写法，回「做 N」的流程完全一样。标题和已有条目重复的不写（合并）。
// 一次最多写 sensePerProject 条，多的返回给调用方留到下次。
func (a *Service) SenseCommit(p backlog.Project, cands []SenseCandidate) ([]string, []SenseCandidate, error) {
	if why := a.Suggest.SuggestUnwritable(p); why != "" {
		return nil, nil, errors.New(why)
	}
	var rest []SenseCandidate
	if len(cands) > SensePerProject {
		a.Logf("主动感知：%s 这次有 %d 条真问题，先写 %d 条，其余留到下次", p.Name, len(cands), SensePerProject)
		cands, rest = cands[:SensePerProject], cands[SensePerProject:]
	}
	store, err := a.Suggest.LiveSuggestions(true)
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, e := range store.Proposed(p.Name) {
		known[backlog.TitleKey(e.Title)] = true
	}
	first, err := a.Suggest.ReserveSuggestNumbers(len(cands))
	if err != nil {
		return nil, nil, err
	}
	now := a.Now()
	var entries []suggest.SuggestEntry
	var out []string
	err = a.Backlog.CommitBacklog(p, "待办：主动感知的建议", func(old string) (string, error) {
		entries, out = nil, nil
		have := backlog.BacklogTitleKeys(old)
		rejected := a.Suggest.NotDoingKeys(p)
		var lines []string
		next := first
		for _, c := range cands {
			k := backlog.TitleKey(c.Title)
			if have[k] || known[k] || rejected[k] {
				continue
			}
			have[k] = true
			line := fmt.Sprintf("- [ ] %s（建议 %d）", c.Line, next)
			lines = append(lines, line)
			entries = append(entries, suggest.SuggestEntry{N: next, Project: p.Name, Fingerprint: backlog.Fingerprint(line), Title: c.Title, CreatedAt: now})
			out = append(out, fmt.Sprintf("%d　%s　%s", next, p.Name, c.Title))
			next++
		}
		if len(lines) == 0 {
			return old, nil
		}
		text := suggest.InsertSuggestions(old, lines)
		if err := suggest.VerifySuggestionPresent(text, lines); err != nil {
			return "", err
		}
		return text, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		return nil, rest, nil
	}
	if err := a.Suggest.WithSuggest(func(s *suggest.SuggestStore) error {
		s.Items = append(s.Items, entries...)
		s.Prune(now)
		return nil
	}); err != nil {
		a.Logf("主动感知：%s 写进了 BACKLOG，但编号没存下（回「做 N」用不了，手动移也行）：%v", p.Name, err)
	}
	a.Logf("主动感知：在 %s 的「建议」写了 %d 条", p.Name, len(entries))
	return out, rest, nil
}

// senseFlushPending：上次判断完但写不进去的建议，这次先写。
func (a *Service) senseFlushPending() {
	var pending []SenseCandidate
	if err := a.WithSense(func(s *SenseStore) error {
		pending, s.Pending = s.Pending, nil
		return nil
	}); err != nil {
		a.Logf("主动感知：读不了没写进去的建议：%v", err)
		return
	}
	if len(pending) > 0 {
		a.noticeSense(a.senseWrite(pending))
	}
}

// noticeSense：写了东西才推一条。
func (a *Service) noticeSense(written []string) {
	if len(written) == 0 {
		return
	}
	n := notify.Notice{Cat: "sense", Text: fmt.Sprintf("主动感知：%d 条真问题写进「建议」，等你点头", len(written))}
	for _, w := range written {
		n.Lines = append(n.Lines, "建议 "+textfmt.TruncRunes(w, 56))
	}
	n.Lines = append(n.Lines, "回复「做 "+strings.Join(suggest.SuggestNumbers(written, 2), " ")+"」移进待做，「不做 N」以后不再提")
	a.Notify.Notify(n)
}
