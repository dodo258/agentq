package sched

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"agentq/internal/discuss"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/state"
)

const (
	retentionLogMax  = 10 << 20 // launchd.log 超过这么大才截
	retentionLogKeep = 2 << 20  // 截完留多少尾部
)

var (
	retentionJobIDRe = regexp.MustCompile(`^\d{4}-\d{6}-.+$`)        // newJobID 的格式
	retentionReplyRe = regexp.MustCompile(`^\d{4}-\d{6}-.+\.md$`)    // replies/<任务id>.md
	retentionDayRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.md$`) // sense/<日期>.md
)

func (a *Service) retentionDayPath() string { return filepath.Join(a.DataDir(), "retention-day") }

// retentionEntry 是一个可能要删的目录或文件。
type retentionEntry struct {
	path   string
	newest time.Time
	// owner 是这个条目属于哪个任务，空的话按文件名认。
	owner string
	// inUse：还在用，多旧都不清。
	inUse bool
}

// retention 是 tick 的最后一步。调用方拿着 tick.lock，st 是刚存过的状态。
func (a *Service) retention(st *state.State) {
	day := a.Now().Format("2006-01-02")
	if b, err := os.ReadFile(a.retentionDayPath()); err == nil && strings.TrimSpace(string(b)) == day {
		return
	}
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		a.Logf("清理：建 %s 失败，今天不清：%v", a.DataDir(), err)
		return
	}
	// 先记再做：中途出问题也不会每次 tick 都重来一遍
	if err := fsx.WriteFileAtomic(a.retentionDayPath(), []byte(day+"\n")); err != nil {
		a.Logf("清理：写 %s 失败，今天不清：%v", a.retentionDayPath(), err)
		return
	}
	a.trimLog()
	if a.Cfg.RetentionDays == 0 {
		return
	}
	a.removeOldEntries(st)
}

// trimLog 把 launchd.log 截到只留尾部。在原文件上改：常驻服务（Telegram 监听）的文件描述符一直指着它，换成新文件它就写丢了。
func (a *Service) trimLog() {
	path := a.LaunchdLogPath()
	fi, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			a.Logf("清理：看不了 %s：%v", path, err)
		}
		return
	}
	if !fi.Mode().IsRegular() || fi.Size() <= retentionLogMax {
		return
	}
	if err := keepFileTail(path, fi.Size(), retentionLogKeep); err != nil {
		a.Logf("清理：截断 %s 失败：%v", path, err)
		return
	}
	a.Logf("清理：%s 超过 %d MB，只留了尾部", path, retentionLogMax>>20)
}

// keepFileTail 读末尾 keep 字节，从第一个完整的行开始写回文件开头再截断。
func keepFileTail(path string, size, keep int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("打开：%w", err)
	}
	defer f.Close()
	buf := make([]byte, keep)
	n, err := f.ReadAt(buf, size-keep)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("读：%w", err)
	}
	tail := buf[:n]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	if _, err := f.WriteAt(tail, 0); err != nil {
		return fmt.Errorf("写：%w", err)
	}
	if err := f.Truncate(int64(len(tail))); err != nil {
		return fmt.Errorf("截断：%w", err)
	}
	return nil
}

// removeOldEntries 删超过 retention_days 的运行数据。
//
// 基准不是当前时间，是「现在」和记录里最新时间中较早的那个：时钟往前跳一年时，不能把全部记录都算成过期。
// 比现在还晚的记录（时钟往回跳过）不算过期。代价是长期没有新任务时不清理，磁盘也没有在涨。
func (a *Service) removeOldEntries(st *state.State) {
	now := a.Now()
	keep := retentionKeepIDs(st)
	for id := range keep { // 审查记录写的是任务 id：和按文件名认的分开，同名不串
		keep["审查:"+id] = true
	}
	entries := a.retentionEntries()
	ref := time.Time{}
	for _, e := range entries {
		if e.newest.After(ref) {
			ref = e.newest
		}
	}
	if ref.IsZero() {
		return
	}
	if ref.After(now) {
		ref = now
	}
	cutoff := ref.Add(-time.Duration(a.Cfg.RetentionDays) * 24 * time.Hour)
	removed, failed := 0, 0
	for _, e := range entries {
		id := e.owner
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(e.path), ".md")
		}
		if e.inUse || keep[id] || !e.newest.Before(cutoff) {
			continue
		}
		if _, err := os.Lstat(gate.Worktree(e.path)); err == nil { // 门的 worktree 还在：Git 里还登记着，由过门流程收
			continue
		}
		if err := os.RemoveAll(e.path); err != nil {
			a.Logf("清理：删不掉 %s：%v", e.path, err)
			failed++
			continue
		}
		removed++
	}
	a.removeEmptyReviewDirs()
	if removed > 0 || failed > 0 {
		a.Logf("清理：删了 %d 个超过 %d 天的任务记录，%d 个删不掉", removed, a.Cfg.RetentionDays, failed)
	}
}

// reviewDirs：审查记录的目录（gate/reviews/<项目>、gate/reviews/<项目>/ui），不进软链接。
func (a *Service) reviewDirs() []string {
	root := filepath.Join(a.DataDir(), "gate", "reviews")
	var dirs []string
	for _, d := range a.retentionList(root) {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		dirs = append(dirs, dir)
		if ui := filepath.Join(dir, "ui"); a.retentionExists(ui) {
			dirs = append(dirs, ui)
		}
	}
	return dirs
}

func (a *Service) retentionExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil && a.retentionRealDir(path)
}

// removeEmptyReviewDirs：审查记录清完后的空目录顺手去掉，不空的 os.Remove 不动它。
func (a *Service) removeEmptyReviewDirs() {
	dirs := a.reviewDirs()
	for i := len(dirs) - 1; i >= 0; i-- { // ui 在它的项目目录后面：先子后父
		if err := os.Remove(dirs[i]); err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, fs.ErrNotExist) {
			a.Logf("清理：删不掉空目录 %s：%v", dirs[i], err)
		}
	}
}

// retentionKeepIDs：state.json 还引用的任务都留着，包括运行中、等续跑、HeldProjects、StuckPending，以及被续跑的那一个。
func retentionKeepIDs(st *state.State) map[string]bool {
	keep := map[string]bool{}
	for _, j := range st.Jobs {
		for _, id := range []string{j.ID, j.ResumeOf, j.ResumedBy} {
			if id != "" {
				keep[id] = true
			}
		}
	}
	return keep
}

// retentionEntries 列出 agentq 自己建的条目（名字和类型都对得上才算），不认识的东西不碰。
func (a *Service) retentionEntries() []retentionEntry {
	data := a.DataDir()
	if !a.retentionRealDir(data) {
		return nil
	}
	var entries []retentionEntry
	dirs := []string{filepath.Join(data, "jobs"), filepath.Join(data, "gate"), filepath.Join(data, "verify"), filepath.Join(data, "inbox-work")}
	sense := a.Ledger.SenseDir()
	senseOK := a.retentionRealDir(sense)
	if senseOK {
		dirs = append(dirs, filepath.Join(sense, "runs"))
	}
	for _, dir := range dirs {
		for _, d := range a.retentionList(dir) {
			if !d.IsDir() || !retentionJobIDRe.MatchString(d.Name()) {
				continue
			}
			path := filepath.Join(dir, d.Name())
			entries = append(entries, retentionEntry{path: path, newest: retentionNewest(path, d)})
		}
	}
	files := func(dir string, re *regexp.Regexp) {
		for _, d := range a.retentionList(dir) {
			if !d.Type().IsRegular() || !re.MatchString(d.Name()) {
				continue
			}
			path := filepath.Join(dir, d.Name())
			entries = append(entries, retentionEntry{path: path, newest: retentionNewest(path, d)})
		}
	}
	files(filepath.Join(data, "replies"), retentionReplyRe)
	if senseOK {
		files(sense, retentionDayRe)
	}
	// 审查记录按它是哪个任务的：任务还在记录里（在跑、在过门、卡住的都在）就留着，审完又跑门要用
	for _, dir := range a.reviewDirs() {
		for _, d := range a.retentionList(dir) {
			if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".json") {
				path := filepath.Join(dir, d.Name())
				entries = append(entries, retentionEntry{path: path, newest: retentionNewest(path, d), owner: "审查:" + gate.ReviewOwner(path)})
			}
		}
	}
	// 商量记录：还在商量的留着；了结的过期后清，同一条才会再问
	discussDir := filepath.Join(data, "discuss")
	for _, d := range a.retentionList(discussDir) {
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".json") {
			path := filepath.Join(discussDir, d.Name())
			entries = append(entries, retentionEntry{path: path, newest: retentionNewest(path, d), owner: "商量:" + d.Name(), inUse: discuss.RecordActive(path)})
		}
	}
	return entries
}

// retentionRealDir：path 是真的目录（不是软链接）。不存在也算否，只是不用说。
func (a *Service) retentionRealDir(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			a.Logf("清理：看不了 %s：%v", path, err)
		}
		return false
	}
	if !fi.IsDir() {
		a.Logf("清理：%s 不是目录（可能是软链接），不进去", path)
		return false
	}
	return true
}

func (a *Service) retentionList(dir string) []fs.DirEntry {
	if !a.retentionRealDir(dir) {
		return nil
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		a.Logf("清理：读不了 %s：%v", dir, err)
		return nil
	}
	return list
}

// retentionNewest：条目本身和目录下第一层文件里最新的修改时间（任务结束后 meta.json 还会写）。读不了的不算。
func retentionNewest(path string, d fs.DirEntry) time.Time {
	var newest time.Time
	if fi, err := d.Info(); err == nil {
		newest = fi.ModTime()
	}
	if !d.IsDir() {
		return newest
	}
	children, err := os.ReadDir(path)
	if err != nil {
		return newest
	}
	for _, c := range children {
		if fi, err := c.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}
