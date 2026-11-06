package sense

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"agentq/internal/fsx"
)

// senseSource 是一个来源（某个 App 的评价、某个 App 的 TestFlight 反馈、某个 GitHub 仓库）看到哪了。
type senseSource struct {
	// Baseline：第一次看到这个来源时已有内容里最新的时间（空的就是当时）。比它早的永远不算新的。
	Baseline time.Time            `json:"baseline"`
	Seen     map[string]time.Time `json:"seen,omitempty"`
}

// SenseCandidate 是判断完、要写进「## 建议」的一条（只有清洗后的模型摘要）。写不进去时留在 Pending，下次不用再花钱判断。
type SenseCandidate struct {
	Project string `json:"project"`
	Title   string `json:"title"`
	Line    string `json:"line"` // 不含编号的正文
}

type SenseStore struct {
	Sources map[string]*senseSource `json:"sources,omitempty"`
	Pending []SenseCandidate        `json:"pending,omitempty"`
	// Day/Runs：Day 那天起了几次模型。
	Day  string `json:"day,omitempty"`
	Runs int    `json:"runs,omitempty"`
	// LastRun：上次读取的时间（status 显示）。
	LastRun time.Time `json:"last_run,omitempty"`
	// Misses：条目 → 连续几次交给模型却没得到合格判断。超过 senseMissMax 就放弃。
	Misses map[string]int `json:"misses,omitempty"`
	// CatchUpDay：开机补跑那天读过一遍了（复盘一直起不来时 tick 不会每 5 分钟读一次）。
	CatchUpDay string `json:"catch_up_day,omitempty"`
}

func (a *Service) SensePath() string { return filepath.Join(a.DataDir(), "sense.json") }

func (a *Service) senseLockPath() string { return filepath.Join(a.DataDir(), "sense.lock") }

// readSense 只读：不存在或内容坏了都返回空的（--dry-run、status 用，不备份不重置）；ok 为假表示内容坏了或读不了。
func (a *Service) readSense() (*SenseStore, bool) {
	s := &SenseStore{}
	b, err := os.ReadFile(a.SensePath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, true
	}
	if err != nil || json.Unmarshal(b, s) != nil {
		return &SenseStore{}, false
	}
	return s, true
}

// loadSense：不存在是正常的；读不了返回错误；内容坏了备份成 .bad-<时间> 并重置成空的（之后的来源重新记基线，不会把历史当新内容）。
func (a *Service) loadSense() (*SenseStore, error) {
	b, err := os.ReadFile(a.SensePath())
	if errors.Is(err, fs.ErrNotExist) {
		return &SenseStore{}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &SenseStore{}
	if err := json.Unmarshal(b, s); err != nil {
		bak := a.SensePath() + ".bad-" + a.Now().Format("20060102-150405")
		if werr := fsx.WriteFileAtomic(bak, b); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），备份到 %s 也失败：%w", a.SensePath(), err, bak, werr)
		}
		if werr := fsx.WriteFileAtomic(a.SensePath(), []byte("{}\n")); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），已备份为 %s，但重置它失败：%w", a.SensePath(), err, bak, werr)
		}
		a.Logf("主动感知：%s 格式不对（%v），已备份为 %s 并重置成空的（各来源重新记基线）", a.SensePath(), err, bak)
		return &SenseStore{}, nil
	}
	return s, nil
}

// WithSense 拿着 sense.lock 读、改、原子写回。只做短的读写。
func (a *Service) WithSense(fn func(s *SenseStore) error) error {
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(a.senseLockPath(), senseLockWait)
	if err != nil {
		return fmt.Errorf("拿 %s：%w", a.senseLockPath(), err)
	}
	if lock == nil {
		return fmt.Errorf("等了 %s 还拿不到 %s", senseLockWait, a.senseLockPath())
	}
	defer lock.Close()
	s, err := a.loadSense()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(a.SensePath(), append(b, '\n'))
}

func (src *senseSource) isNew(key string, at time.Time) bool {
	if src == nil {
		return false
	}
	if _, ok := src.Seen[key]; ok {
		return false
	}
	return at.IsZero() || !at.Before(src.Baseline) // 时间读不出：不知道新旧，当新的
}

// mark 记下见过；超过上限时去掉最早的，并把基线抬到去掉的那些的时间（去掉之后它们也不能算新的）。
func (src *senseSource) mark(key string, at time.Time) {
	if src.Seen == nil {
		src.Seen = map[string]time.Time{}
	}
	src.Seen[key] = at
	if len(src.Seen) <= senseSeenMax {
		return
	}
	keys := make([]string, 0, len(src.Seen))
	for k := range src.Seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, k int) bool { return src.Seen[keys[i]].Before(src.Seen[keys[k]]) })
	for _, k := range keys[:len(keys)-senseSeenMax] {
		if t := src.Seen[k]; t.After(src.Baseline) {
			src.Baseline = t
		}
		delete(src.Seen, k)
	}
}
