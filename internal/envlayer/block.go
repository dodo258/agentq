package envlayer

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
)

// 块方式：后端没有规则目录，只有一个用户级说明文件（backend.RulesBlock）。规则写成一段带标记的块接在文件末尾，别的字节一个不动。
// 改之前备份；关掉和卸载只删自己的块，连接时加的空行也一起拿走，文件回到原来的样子。
// 块里只放 rules/core.md：语言规则靠 paths 按文件类型加载，说明文件做不到，全放进去每个会话都要多读，而且这类文件有总长度上限。
const (
	blockFile  = "core.md"
	blockBegin = "<!-- agentq:rules:begin 这一段由 agentq 管理，改了就不再自动更新；不要就运行 agentq env off -->"
	blockEnd   = "<!-- agentq:rules:end -->"
)

// rulesFile 是这个后端放块的文件和让它失效的文件。
func (l *Layer) rulesFile() (file, shadow string, ok bool) {
	b, ok := l.backend.(backend.RulesBlock)
	if !ok {
		return "", "", false
	}
	file, shadow = b.RulesFile(l.home)
	return file, shadow, true
}

func (l *Layer) wantBlock() ([]byte, error) {
	want, err := l.bundled()
	if err != nil {
		return nil, err
	}
	core, ok := want[blockFile]
	if !ok {
		return nil, fmt.Errorf("这个版本没带 %s", blockFile)
	}
	return []byte(blockBegin + "\n" + strings.TrimRight(string(core), "\n") + "\n" + blockEnd + "\n"), nil
}

// blockState 是文件里块的样子。
type blockState int

const (
	blockNone blockState = iota
	blockOne
	blockBroken // 标记残缺或重复：判断不了哪段是自己的，不动
)

// findBlock 找块的位置：from 到 to（含结束标记后面的换行）。
func findBlock(data []byte) (from, to int, st blockState) {
	begins, ends := bytes.Count(data, []byte(blockBegin)), bytes.Count(data, []byte(blockEnd))
	if begins == 0 && ends == 0 {
		return 0, 0, blockNone
	}
	from, to = bytes.Index(data, []byte(blockBegin)), bytes.Index(data, []byte(blockEnd))
	if begins != 1 || ends != 1 || to < from {
		return 0, 0, blockBroken
	}
	to += len(blockEnd)
	if to < len(data) && data[to] == '\n' {
		to++
	}
	return from, to, blockOne
}

// backupFile 把改之前的文件存一份，和记录文件放一起。留着不清，文件很小。
func (l *Layer) backupFile(file string, data []byte) error {
	dir := filepath.Join(filepath.Dir(RecordPath(l.home)), "env-layer-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("备份 %s 失败，没动它：%w", file, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(file)+"."+time.Now().Format("20060102-150405")+"-*")
	if err != nil {
		return fmt.Errorf("备份 %s 失败，没动它：%w", file, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("备份 %s 失败，没动它：%w", file, err)
	}
	return nil
}

// writeKeepMode 原子写回，权限照原来的。
func writeKeepMode(path string, data []byte, absent bool) error {
	mode := fs.FileMode(0o644)
	if !absent {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
	}
	if err := fsx.WriteFileAtomic(path, data); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// applyBlock 让文件里的块和这个版本一致。只动自己写的、没被改过的；keepDeleted 为真时记录里有、文件里没有算用户删的。
func (l *Layer) applyBlock(r record, keepDeleted bool) (Report, error) {
	rep := Report{Dir: r.File}
	want, err := l.wantBlock()
	if err != nil {
		return rep, err
	}
	if err := fsx.CheckParents(l.home, r.File); err != nil {
		return rep, fmt.Errorf("没装规则：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.File), 0o755); err != nil {
		return rep, fmt.Errorf("建目录失败：%w", err)
	}
	data, absent, regular, err := read(r.File)
	if err != nil {
		return rep, fmt.Errorf("读 %s 失败：%w", r.File, err)
	}
	if !absent && !regular {
		return rep, fmt.Errorf("%s 不是普通文件（软链接或目录），没动它", r.File)
	}
	if r.Files == nil {
		r.Files = map[string]string{}
	}
	r.State = StateOn
	from, to, st := findBlock(data)
	_, recorded := r.Files[blockFile]
	switch {
	case st == blockBroken:
		rep.Kept = append(rep.Kept, blockFile)
	case st == blockNone && slicesContains(r.Declined, blockFile):
	case st == blockNone && keepDeleted && recorded:
		delete(r.Files, blockFile)
		r.Declined = append(r.Declined, blockFile)
		rep.Declined = append(rep.Declined, blockFile)
	case st == blockNone:
		sep := ""
		if len(data) > 0 {
			sep = "\n"
			if data[len(data)-1] != '\n' {
				sep = "\n\n"
			}
			if err := l.backupFile(r.File, data); err != nil {
				return rep, err
			}
		}
		if err := writeKeepMode(r.File, append(append(append([]byte(nil), data...), sep...), want...), absent); err != nil {
			return rep, l.saveAfter(r, fmt.Errorf("写 %s 失败：%w", r.File, err))
		}
		r.Sep, r.Created = sep, absent
		r.Files[blockFile] = digest(want)
		r.Declined = nil
		rep.Written = append(rep.Written, blockFile)
	case digest(data[from:to]) == digest(want):
		r.Files[blockFile] = digest(want)
		r.Declined = nil
	case digest(data[from:to]) == r.Files[blockFile]:
		if err := l.backupFile(r.File, data); err != nil {
			return rep, err
		}
		next := append(append(append([]byte(nil), data[:from]...), want...), data[to:]...)
		if err := writeKeepMode(r.File, next, false); err != nil {
			return rep, l.saveAfter(r, fmt.Errorf("写 %s 失败：%w", r.File, err))
		}
		r.Files[blockFile] = digest(want)
		rep.Updated = append(rep.Updated, blockFile)
	default:
		rep.Kept = append(rep.Kept, blockFile)
	}
	return rep, l.saveRecord(r)
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// clearBlock 只删哈希对得上的块。改过的留着报告，文件不在算没删。
func (l *Layer) clearBlock(r record, to State) (Report, error) {
	rep := Report{Dir: r.File}
	if want, ok := r.Files[blockFile]; ok {
		data, absent, regular, err := read(r.File)
		if err != nil {
			return rep, l.saveAfter(r, fmt.Errorf("读 %s 失败：%w", r.File, err))
		}
		if !absent {
			from, end, st := findBlock(data)
			switch {
			case !regular || st == blockBroken || (st == blockOne && digest(data[from:end]) != want):
				rep.Kept = append(rep.Kept, blockFile)
			case st == blockOne:
				if err := l.backupFile(r.File, data); err != nil {
					return rep, l.saveAfter(r, err)
				}
				rest := append(bytes.TrimSuffix(append([]byte(nil), data[:from]...), []byte(r.Sep)), data[end:]...)
				if r.Created && len(rest) == 0 {
					err = os.Remove(r.File)
				} else {
					err = writeKeepMode(r.File, rest, false)
				}
				if err != nil {
					return rep, l.saveAfter(r, fmt.Errorf("写 %s 失败：%w", r.File, err))
				}
				rep.Removed = append(rep.Removed, blockFile)
			}
		}
		delete(r.Files, blockFile)
	}
	r.State, r.Sep, r.Created = to, "", false
	return rep, l.saveRecord(r)
}

// statusBlock 是块方式的状态。
func (l *Layer) statusBlock(r record, file, shadow string) (Status, error) {
	s := Status{Supported: true, State: r.State, Dir: file}
	if shadow != "" {
		if fi, err := os.Stat(shadow); err == nil && !fi.IsDir() {
			s.Shadow = shadow
		}
	}
	data, absent, regular, err := read(file)
	if err != nil {
		return s, fmt.Errorf("读 %s 失败：%w", file, err)
	}
	from, to, st := findBlock(data)
	own := append(append([]byte(nil), data[:from]...), data[to:]...)
	if st == blockBroken {
		own = data
	}
	if !absent && (!regular || len(bytes.TrimSpace(own)) > 0) {
		s.UserRules = append(s.UserRules, file)
	}
	if s.Shadow != "" {
		s.UserRules = append(s.UserRules, s.Shadow)
	}
	want, err := l.wantBlock()
	if err != nil {
		return s, err
	}
	_, recorded := r.Files[blockFile]
	switch {
	case st == blockOne && (digest(data[from:to]) == r.Files[blockFile] || digest(data[from:to]) == digest(want)):
		s.Files = []FileStatus{{blockFile, FileIntact}}
	case st != blockNone:
		s.Files = []FileStatus{{blockFile, FileModified}}
	case recorded:
		s.Files = []FileStatus{{blockFile, FileMissing}}
	case slicesContains(r.Declined, blockFile):
		s.Files = []FileStatus{{blockFile, FileDeclined}}
	}
	return s, nil
}
