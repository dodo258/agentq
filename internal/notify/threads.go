package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/state"
)

const ThreadKeep = 14 * 24 * time.Hour

const ThreadMax = 500

const ThreadJob = "job" // 只点名一个任务的提醒、只读追问的回答

const ThreadDecide = "decide" // 只含一条的「要你定」

// ThreadDiscuss：商量大方向的卡片。回复它就是回答，不起模型。Fingerprint 放商量的编号。
const ThreadDiscuss = "discuss"

// Thread 是一条发出去的消息对应的那件事。
type Thread struct {
	Kind        string    `json:"kind"`
	JobID       string    `json:"job_id,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	ProjectDir  string    `json:"project_dir,omitempty"`
	Project     string    `json:"project,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	At          time.Time `json:"at"`
}

// AddThread 登记一条消息并清理：超过 threadKeep 的去掉，超过 threadMax 条时去掉最早的。id 为 0（没走 Telegram）不登记。
func (s *State) AddThread(id int64, t Thread, now time.Time) {
	if id == 0 {
		return
	}
	if s.Threads == nil {
		s.Threads = map[int64]Thread{}
	}
	t.At = now
	s.Threads[id] = t
	s.PruneThreads(now)
}

func (s *State) PruneThreads(now time.Time) {
	for id, t := range s.Threads {
		if now.Sub(t.At) > ThreadKeep {
			delete(s.Threads, id)
		}
	}
	for len(s.Threads) > ThreadMax {
		var oldest int64
		for id, t := range s.Threads {
			if oldest == 0 || t.At.Before(s.Threads[oldest].At) || t.At.Equal(s.Threads[oldest].At) && id < oldest {
				oldest = id
			}
		}
		delete(s.Threads, oldest)
	}
}

// WithNotifyState 在已经拿着 notify.lock 时直接用那一份，否则拿锁再改。
func (a *Service) WithNotifyState(fn func(s *State)) error {
	if a.notifyHeld != nil {
		fn(a.notifyHeld)
		return nil
	}
	return a.WithNotify(func(s *State) error { fn(s); return nil })
}

// LookupJob 找任务记录：先看 state.json，结束太久被清掉的看 meta.json。查不到返回空。
func (a *Service) LookupJob(st *state.State, id string) *state.Job {
	if id == "" {
		return nil
	}
	for _, j := range st.Jobs {
		if j.ID == id {
			return j
		}
	}
	b, err := os.ReadFile(filepath.Join(a.JobDir(id), "meta.json"))
	if err != nil {
		return nil
	}
	var j state.Job
	if json.Unmarshal(b, &j) != nil {
		return nil
	}
	return &j
}

// noticeThread：这条消息只说一件事时返回要登记的内容。调用方保证这次只发了这一条。
func (a *Service) noticeThread(st *state.State, n Notice) (Thread, bool) {
	switch {
	case n.Card != nil && n.Card.DiscussKey != "":
		return Thread{Kind: ThreadDiscuss, Project: n.Card.Project, Fingerprint: n.Card.DiscussKey}, true
	case n.Cat == "decide" && n.Project != "":
		return Thread{Kind: ThreadDecide, Project: n.Project, Fingerprint: n.Key}, true
	case n.JobID != "":
		if st == nil {
			st = &state.State{}
		}
		j := a.LookupJob(st, n.JobID)
		if j == nil {
			return Thread{}, false
		}
		return JobThread(j), true
	}
	return Thread{}, false
}

func JobThread(j *state.Job) Thread {
	if j.SessionInRoot() { // 新项目第一条的会话在 projects_root：回复只能在那里只读接上
		th := Thread{Kind: ThreadJob, JobID: j.ID, SessionID: j.SessionID, ProjectDir: j.NewReport.Root, Project: j.Project}
		return th
	}
	return Thread{Kind: ThreadJob, JobID: j.ID, SessionID: j.SessionID, ProjectDir: j.ProjectDir, Project: j.Project, Fingerprint: j.Fingerprint}
}

func ThreadExtra(msgs []InMsg) string {
	var b strings.Builder
	b.WriteString("\n\n用户在 Telegram 回复了这个任务，按他说的调整：\n")
	WriteInMsgs(&b, msgs)
	return b.String()
}
