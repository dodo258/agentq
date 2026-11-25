package proc

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentq/internal/fsx"
)

// 任务和门的子进程各用自己的临时目录（TMPDIR），收尾整个删掉，项目的测试自己不清也不会堆在公共临时目录里。
// 建一个登记一个，清只清登记过的：不按名字去扫公共目录。

const (
	// tmpPrefix 是目录名的开头。名字越短越好：子进程会在里面建 Unix socket，macOS 的路径上限是 103 字节。
	tmpPrefix = "agq"
	// tmpScanWait 是查谁在用目录时一条外部命令的上限。
	tmpScanWait = 30 * time.Second
)

// macUserTmp 是 macOS 每个用户默认的 TMPDIR。它自己就有 48 字节，本机有程序在里面建 98 字节的 socket，再往下放一层就超了，所以换到 macUserTmpAlt。
var macUserTmp = regexp.MustCompile(`^(/private)?/var/folders/[^/]+/[^/]+/T$`)

// macUserTmpAlt：够短，而且系统不清它。/tmp 不行：com.apple.tmp_cleaner 每天零点删里面 3 天没动的文件，等用户定、可续跑的就被掏空了。
const macUserTmpAlt = "/var/tmp"

// TmpStore 登记 agentq 建的临时目录，每个一份 <Dir>/<目录名>.json。Dir 空的不建（直接用 RunScript 的测试）。
type TmpStore struct{ Dir string }

// Tmp 是一份登记。
type Tmp struct {
	Path string `json:"path"`
	// Inode：删之前核对还是当初建的那个目录。设备号不记：macOS 重启后会变。
	Inode uint64 `json:"inode"`
	// Caller 是建它的 agentq 进程自己的 TMPDIR 原值。
	Caller string `json:"caller,omitempty"`
	// Job 是正在用它的任务，门脚本、截图的是空的。
	Job string `json:"job,omitempty"`
	// Marks 是用过它的进程的 AGENTQ_JOB 值，续跑接着用时往后加。
	Marks []string `json:"marks"`
	// Removing：定了要删，续跑不能再拿去用。
	Removing bool `json:"removing,omitempty"`
	// Note 是上次没清的原因，同样的只记一次日志。
	Note string `json:"note,omitempty"`
}

// New 建一个只有自己能进的临时目录并登记。先建后登记：两步之间被杀只会留下一个空目录，不会把别人的目录记成自己的。
func (s TmpStore) New(job, mark string) (Tmp, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Tmp{}, fmt.Errorf("建临时目录的登记处：%w", err)
	}
	base := s.base()
	for range 20 {
		name := tmpPrefix + strings.ToLower(rand.Text()[:6])
		if _, err := os.Lstat(s.record(name)); err == nil { // 别处有一个同名的
			continue
		}
		path := filepath.Join(base, name)
		if err := os.Mkdir(path, 0o700); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return Tmp{}, fmt.Errorf("建临时目录：%w", err)
		}
		t, err := s.register(path, job, mark)
		if err != nil {
			return Tmp{}, errors.Join(err, os.Remove(path))
		}
		return t, nil
	}
	return Tmp{}, fmt.Errorf("建临时目录：%s 下试了 20 个名字都有了", base)
}

func (s TmpStore) register(path, job, mark string) (Tmp, error) {
	// macOS 的 /var/tmp 属于 wheel 组，新文件跟着目录走，会和在默认 TMPDIR 里不一样。改不了不影响用
	_ = os.Chown(path, -1, os.Getegid())
	fi, err := os.Lstat(path)
	if err != nil {
		return Tmp{}, fmt.Errorf("看临时目录：%w", err)
	}
	t := Tmp{Path: path, Inode: inode(fi), Caller: os.Getenv("TMPDIR"), Job: job, Marks: []string{mark}}
	return t, s.Save(t)
}

// base 定建在哪。
func (s TmpStore) base() string {
	cur := os.TempDir()
	switch {
	case within(realPath(s.Dir), realPath(cur)): // 这个 agentq 自己就装在临时目录里（评测现场、测试）：放在数据目录里，跟着现场一起清
		return s.Dir
	case macUserTmp.MatchString(filepath.Clean(cur)):
		return macUserTmpAlt
	}
	return cur
}

// Leave：agentq 自己是在登记过的临时目录里起的（任务里执行 agentq 命令）就换回原来的 TMPDIR。
// 不换的话，它起的任务、门会建在这个目录里面，外面那个任务收尾时把它们的也删了。
func (s TmpStore) Leave() {
	if s.Dir == "" {
		return
	}
	cur := realPath(os.TempDir())
	list, _ := s.List()
	for _, t := range list {
		if !within(cur, realPath(t.Path)) {
			continue
		}
		if t.Caller == "" {
			_ = os.Unsetenv("TMPDIR")
		} else {
			_ = os.Setenv("TMPDIR", t.Caller)
		}
		return
	}
}

// List 列出登记。读不懂的登记文件在 bad 里：目录是谁的说不准，不能动。
func (s TmpStore) List() (list []Tmp, bad []string) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, nil // 还没建过
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(s.Dir, e.Name())
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) { // 另一个进程刚清完
			continue
		}
		var t Tmp
		if err == nil {
			err = json.Unmarshal(b, &t)
		}
		if err != nil || !filepath.IsAbs(t.Path) || filepath.Base(t.Path) != name || !strings.HasPrefix(name, tmpPrefix) || t.Inode == 0 || len(t.Marks) == 0 {
			bad = append(bad, path)
			continue
		}
		list = append(list, t)
	}
	return list, bad
}

// OfJob 是这个任务登记的、还在的那个。
func (s TmpStore) OfJob(job string) (Tmp, bool) {
	list, _ := s.List()
	i := slices.IndexFunc(list, func(t Tmp) bool { return t.Job == job && !t.Removing })
	if i < 0 {
		return Tmp{}, false
	}
	if ok, err := list[i].intact(); err != nil || !ok {
		return Tmp{}, false
	}
	return list[i], true
}

// Save 写回登记。
func (s TmpStore) Save(t Tmp) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(s.record(filepath.Base(t.Path)), append(b, '\n')); err != nil {
		return fmt.Errorf("登记临时目录 %s：%w", t.Path, err)
	}
	return nil
}

func (s TmpStore) record(name string) string { return filepath.Join(s.Dir, name+".json") }

// Remove 删掉目录和登记。调用方先确认没人在用。已经不是当初建的那个目录（被换了）、里面挂着别的卷的不动，返回原因。
func (s TmpStore) Remove(t Tmp) error {
	ok, err := t.intact()
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("看临时目录：%w", err)
	case !ok: // 再也对不上了，留着登记只会每次都说一遍
		return errors.Join(fmt.Errorf("%s 已经不是 agentq 建的那个目录，没动它，只去掉登记", t.Path), s.forget(t))
	default:
		if err := RemoveTree(t.Path); err != nil {
			return fmt.Errorf("删 %s：%w", t.Path, err)
		}
	}
	return s.forget(t)
}

func (s TmpStore) forget(t Tmp) error {
	if err := os.Remove(s.record(filepath.Base(t.Path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("去掉登记：%w", err)
	}
	return nil
}

// intact：路径上还是当初建的那个目录（不是软链接）。
func (t Tmp) intact() (bool, error) {
	fi, err := os.Lstat(t.Path)
	if err != nil {
		return false, err
	}
	return fi.IsDir() && inode(fi) == t.Inode, nil
}

// Env 把 env 里的 TMPDIR 换成这个目录。原来的值末尾带斜杠（macOS 默认就带）就也带上：有脚本直接写 ${TMPDIR}名字，没有斜杠就写到外面去了。
func (t Tmp) Env(env []string) []string {
	return WithTMPDIR(env, t.Value())
}

// Value 是传给子进程的 TMPDIR。
func (t Tmp) Value() string {
	if strings.HasSuffix(t.Caller, "/") {
		return t.Path + "/"
	}
	return t.Path
}

// WithTMPDIR 换掉 env 里的 TMPDIR，dir 空的原样返回。
func WithTMPDIR(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "TMPDIR=") })
	return append(out, "TMPDIR="+dir)
}

// TmpScan 是某一刻本机进程的样子，用来认还有谁在用临时目录。一次扫描给所有目录用：扫一遍要零点几秒。
type TmpScan struct{ procs []procText }

// procText 是一个进程（不含自己）看得到的文字：环境和命令行，工作目录和打开的文件。
type procText struct {
	pid   string
	marks []string // AGENTQ_JOB 的值
	text  string
	files []string
}

// Users 找还在用这个目录的进程：环境里带着它的 AGENTQ_JOB，环境、命令行里写着这个目录，或者工作目录、打开的文件在里面。
// 最后一种不靠环境：macOS 看不到系统自带程序的环境。目录名是随机的，出现就算。返回空是没有。
func (s TmpScan) Users(t Tmp) string {
	dir := "/" + filepath.Base(t.Path)
	var pids []string
	for _, p := range s.procs {
		if slices.ContainsFunc(p.marks, func(m string) bool { return slices.Contains(t.Marks, m) }) || strings.Contains(p.text, dir) ||
			slices.ContainsFunc(p.files, func(f string) bool { return strings.Contains(f, dir) }) {
			pids = append(pids, p.pid)
		}
	}
	if len(pids) == 0 {
		return ""
	}
	return "进程 " + strings.Join(pids[:min(len(pids), 3)], "、") + " 还在用"
}

// GroupLeft：进程组里还有没有带这个标记（AGENTQ_JOB 的值）的进程。有认不出的（系统自带的程序看不到环境）或查不了是 ProcUnsure；
// 没有活进程，或者都认得出、都不是（组号被别的程序占用）是 ProcGone。
func GroupLeft(pgid int, mark string) Liveness {
	if pgid <= 0 || !GroupAlive(pgid) {
		return ProcGone
	}
	out, err := PsOutput("-A", "-o", "pid=", "-o", "pgid=", "-o", "stat=")
	if err != nil {
		return ProcUnsure
	}
	left := ProcGone
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != strconv.Itoa(pgid) || strings.HasPrefix(f[2], "Z") {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			return ProcUnsure
		}
		found, readable := procEnvHas(pid, JobEnv+"="+mark)
		switch {
		case found:
			return ProcOurs
		case !readable && syscall.Kill(pid, 0) == nil:
			left = ProcUnsure
		}
	}
	return left
}

// RemoveTree 删整个目录，本来就没有不算错。先走一遍目录：里面挂着别的卷就不删（会删到卷上的东西），没写权限的目录（Go 模块缓存）改回能写。
// 不跟软链接：现场里有指向钥匙串的链接，删的只是链接，WalkDir 也不进去。
func RemoveTree(root string) error {
	rfi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dev := device(rfi)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if device(fi) != dev {
			return fmt.Errorf("%s 是挂上去的别的卷，不删", p)
		}
		if fi.Mode().Perm()&0o700 != 0o700 { // 先回调目录本身再读内容，这里改完就进得去
			return os.Chmod(p, fi.Mode().Perm()|0o700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func device(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

// realPath 解掉软链接（macOS 的 /var、/tmp 都是），解不了就用原样。
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within：p 就是 root 或在它里面。
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
