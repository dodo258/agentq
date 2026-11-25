package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"agentq/internal/backlog"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
)

// 任务的临时目录：起 CLI 前建好（续跑接着用上一轮的），任务了结之后由 tick 清。门脚本的由门自己当场清，没清成的也在这里补。
// 正常结束、超时、被停、中断、引擎被杀都走这一处：不看任务是怎么结束的，只看登记和任务现在的状态。

// takeTmp 给要起的任务定临时目录，返回传给 CLI 的 TMPDIR。续跑是同一个会话，它记得自己写在临时目录里的文件，所以接着用原来那个。
// 建不了返回空：用原来的 TMPDIR 照跑。在任务里执行的 agentq 先换回原来的 TMPDIR：新任务和它的外壳都比这个任务活得久，不能用它的目录、建在它里面。
func (a *Service) takeTmp(j *state.Job) string {
	store := a.Tmps()
	store.Leave()
	if j.ResumeOf != "" && (j.Kind == "" || j.Kind == state.KindNew) {
		if t, ok := store.OfJob(j.ResumeOf); ok {
			t.Job, t.Marks, t.Note = j.ID, append(t.Marks, j.ID), ""
			if err := store.Save(t); err == nil {
				return t.Value()
			} else {
				a.Logf("任务 %s 接不上任务 %s 的临时目录，另建一个：%v", j.ID, j.ResumeOf, err)
			}
		}
	}
	t, err := store.New(j.ID, j.ID)
	if err != nil {
		a.Logf("任务 %s：%v，用原来的临时目录", j.ID, err)
		return ""
	}
	return t.Value()
}

// SweepTmp 定这次清哪些临时目录，记下「要删」后返回，由 RemoveTmp 在放开 tick.lock 之后删。调用方拿着 tick.lock：
// 没有任务正在起，也没有人正要续跑。没收尾、在过门、等用户定、可续跑的，还有进程在用的，说不准的，都不清。
func (a *Service) SweepTmp(st *state.State) []proc.Tmp {
	store := a.Tmps()
	list, bad := store.List()
	for _, p := range bad {
		a.Logf("临时目录：登记 %s 读不懂，它指的目录不动，登记改名留着", a.Tilde(p))
		if err := os.Rename(p, p+".bad"); err != nil {
			a.Logf("临时目录：%v", err)
		}
	}
	var out []proc.Tmp
	scan := sync.OnceValues(func() (proc.TmpScan, error) { return proc.ScanTmp(context.Background()) }) // 有要清的才扫
	for _, t := range list {
		hold, why := false, ""
		if !t.Removing {
			hold, why = a.tmpHeld(st, t)
		}
		if !hold && why == "" {
			why = a.tmpBusy(st, t, scan)
		}
		if hold {
			continue
		}
		if why != "" {
			a.noteTmp(t, why)
			continue
		}
		t.Removing = true
		if err := store.Save(t); err != nil {
			a.Logf("临时目录：%v，这次不清", err)
			continue
		}
		out = append(out, t)
	}
	return out
}

// RemoveTmp 删 SweepTmp 定下的。不用拿 tick.lock：定下的续跑不会再拿去用。中途被杀的下次 tick 接着删。
func (a *Service) RemoveTmp(list []proc.Tmp) {
	if len(list) == 0 {
		return
	}
	store := a.Tmps()
	lock, err := fsx.AcquireLock(filepath.Join(store.Dir, "remove.lock"), 0)
	if err != nil {
		a.Logf("临时目录：拿不到删除的锁，下次 tick 再清：%v", err)
		return
	}
	if lock == nil { // 上一次还在删
		return
	}
	defer lock.Close()
	for _, t := range list {
		if err := store.Remove(t); err != nil {
			a.noteTmp(t, "没清掉："+err.Error())
			continue
		}
		a.Logf("临时目录：%s 用完了，清了 %s", tmpOwner(t), t.Path)
	}
}

// noteTmp 记没清的原因，同样的只记一次。登记已经去掉的只记日志。
func (a *Service) noteTmp(t proc.Tmp, why string) {
	if t.Note == why {
		return
	}
	a.Logf("临时目录：%s 的 %s 先留着，下次 tick 再看：%s", tmpOwner(t), t.Path, why)
	store := a.Tmps()
	if _, err := os.Lstat(filepath.Join(store.Dir, filepath.Base(t.Path)+".json")); err != nil {
		return
	}
	t.Note = why
	if err := store.Save(t); err != nil {
		a.Logf("临时目录：%v", err)
	}
}

func tmpOwner(t proc.Tmp) string {
	if t.Job != "" {
		return "任务 " + t.Job
	}
	return t.Marks[0]
}

// tmpHeld：这个目录还归任务（门）用，hold 是正常还要用（不用说），why 是说不准、要记日志的原因。
func (a *Service) tmpHeld(st *state.State, t proc.Tmp) (hold bool, why string) {
	if t.Job == "" { // 截图脚本在拿着 tick.lock 的 tick 里跑，走到这里一定不在跑了
		return a.Gate.RunHeld(st, t.Marks[0]), ""
	}
	j, err := a.tmpJob(st, t.Job)
	if err != nil {
		return false, fmt.Sprintf("认不出任务 %s 的状态：%v", t.Job, err)
	}
	if j == nil { // 记录过了保留期，或者登记之后还没起就没了
		return false, ""
	}
	switch {
	case j.Status == state.StatusRunning, state.GateActive(j), state.GateStuckNow(j), j.ResumePending, j.StuckPending != "":
		return true, ""
	case j.ResumedBy != "" || j.Kind != "" || j.ProjectDir == "":
		return false, ""
	}
	items, err := a.tmpItems(j)
	if err != nil {
		return false, "读不了 BACKLOG，不知道这一条了结没有：" + err.Error()
	}
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	for _, it := range items {
		// 又有任务接了这一条：它从分支接着做，不用这个目录。按 id 比：过了 30 天的任务不在 state.json 里，是从 meta.json 读的
		if last := LastTaskJob(st, p, it); !JobOnItem(j, p, it) || (last != nil && last.ID != j.ID) {
			continue
		}
		if it.Stuck { // 等用户定
			return true, ""
		}
		// 同 resumableFor：用户点名就用同一会话接着做
		if it.InProgress && j.Status == state.StatusInterrupted && j.SessionID != "" && !j.SessionInRoot() && a.BackendFor(j.Backend).Caps().Resume {
			return true, ""
		}
	}
	return false, ""
}

// tmpItems 同 jobItems，但看不到不当成没有：项目目录不见了（外置盘没挂上、目录改名）、BACKLOG 没了、任务记下的分支查不到，都返回错误。
// jobItems 把没有 BACKLOG 当成空的，在这里会把等用户定、可续跑的当成已了结。
func (a *Service) tmpItems(j *state.Job) ([]backlog.Item, error) {
	onBranch := j.DefaultBranch == "" || a.Gitx.OnBranch(j.ProjectDir, j.DefaultBranch) // 还没有提交的仓库也在分支上，只是查不到这个分支
	if !onBranch && !a.Gitx.HasBranch(j.ProjectDir, j.DefaultBranch) {
		return nil, fmt.Errorf("%s 看不到 %s 分支", j.ProjectDir, j.DefaultBranch)
	}
	if onBranch { // jobItems 这时读工作目录
		if _, err := os.Stat(backlog.Project{Name: j.Project, Dir: j.ProjectDir}.BacklogPath()); err != nil {
			return nil, err
		}
	}
	return a.jobItems(j)
}

// tmpJob 找登记的任务：state.json 里没有就看 meta.json（状态丢了、刚要起就被杀）。都没有返回 nil。
func (a *Service) tmpJob(st *state.State, id string) (*state.Job, error) {
	for _, j := range st.Jobs {
		if j.ID == id {
			return j, nil
		}
	}
	b, err := os.ReadFile(filepath.Join(a.JobDir(id), "meta.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var j state.Job
	if err == nil {
		err = json.Unmarshal(b, &j)
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// tmpBusy：还有进程在用的原因，查不了也算。机器在任务启动后重启过，记的组号可能已经是别的程序的，不看它；
// 按标记和打开的文件照查：续跑接着用的目录，前一个任务是重启前起的，后一个是这次开机起的。
func (a *Service) tmpBusy(st *state.State, t proc.Tmp, scan func() (proc.TmpScan, error)) string {
	for _, j := range st.Jobs {
		if slices.Contains(t.Marks, j.ID) && !a.bootChanged(j) && proc.GroupLeft(j.PID, j.ID) != proc.ProcGone {
			return fmt.Sprintf("进程组 %d 还有进程", j.PID)
		}
	}
	s, err := scan()
	if err != nil {
		return "查不了还有没有进程在用：" + err.Error()
	}
	return s.Users(t)
}
