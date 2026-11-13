package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/ledger"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 集成测试：编译出真的 agentq 二进制，用临时 HOME、临时 projects_root 和假 claude 脚本跑子进程。
// 子进程的 HOME 指向临时目录，绝不读写真实的 ~/.agentq、~/projects、~/.claude。

var agentqBin string

func TestMain(m *testing.M) {
	// 测试二进制被当成长时间进程或替身重新拉起时（TestHelperSleep、TestHelperLostShell），用不着 agentq。
	// 这里再编译一遍的话，工作目录不对、go 不在 PATH、源码正改到一半、机器忙，都会让子进程提前退出或迟迟不到位。
	if os.Getenv("AGENTQ_TEST_HELPER") == "1" {
		os.Exit(m.Run())
	}
	if err := enterRepo(); err != nil {
		fmt.Fprintln(os.Stderr, "准备仓库根：", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "agentq-bin-")
	if err != nil {
		panic(err)
	}
	agentqBin = filepath.Join(dir, "agentq")
	// 推送没配 Telegram 时会弹本机通知：测试里一律改成写这个文件，绝不弹真通知（子进程继承这个环境变量）
	os.Setenv("AGENTQ_MACNOTIFY_LOG", filepath.Join(dir, "macnotify.log"))
	out, err := exec.Command("go", "build", "-o", agentqBin, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "编译 agentq 失败：%v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// 假 claude：auth status 回登录状态；否则记录参数/目录/环境，按 prompt 里的关键字睡几秒、写 JSON。
// READYMERGE：照新合同只做到分支可合并：认领（提交 BACKLOG 的进行中标记）、在任务分支提交改动和 .agentq/ready.json，不合并。
// 再带 TASKSCRIPT：提交前在任务 worktree 里跑 $HOME/fake-task.sh（用例自己写要改什么，提示词在 $FAKE_PROMPT）。TYPE=fix 之类：ready.json 的 type（默认 feature）。
// SELFMERGE：不写 ready.json，自己把条目移到「已完成」并提交，写上合并提交号（绕过门、升级前的老任务）。
// INTERRUPT：首次运行时像 agent 认领一样给 BACKLOG 那一条加进行中标记，然后停在「继续等通知」；
// 用 -r 续跑时把那一条勾掉并报已合并，prompt 里还有 AGAIN 就再中断一次。
// LIMITMID：首次运行时同样加进行中标记，然后撞用量上限报错；续跑时勾掉并报已合并。
// TRANSCRIPT：像真 claude 一样在 $HOME/.claude/projects 下写会话记录，子代理记录一直在长。
// TGDISPATCH：Telegram 对话任务，往 demo 的 BACKLOG「待做」写一条，回一条已派出的消息。
// ROUTERFILE：原样输出 $HOME/fake-router.json（Router 的各种结果由用例写）。
// 「闲时找活」（找活提示词里的固定字样）：原样输出 $HOME/fake-suggest.json。
// 「产品经理调研进程」：原样输出 $HOME/fake-pm.json，没有就给两条带出处的建议。
// FUPREPLY、FUPTODO：只读续跑的结构化回答（FUPTODO 带 todo）；FUPBUDGET：撞预算。
// NEWOK：agentq new 正常做完：在工作目录建真 git 项目 clip-tool，第一条做到分支可合并（不合并，待做里还有一条），结果最后一段是固定四行。
// NEWFIRSTSTUCK：第一条门没过，接手（「第一条死活」）也做不成。NEWBADVERIFY、NEWBADSMOKE：同上，门脚本退出 1。NEWSELFMERGE：又自己合并了。NEWNOREADY：分支上没有 ready.json。NEWNOTREADY：结尾写第一条没做到可合并。「第一条活」：接手第一条，修好再交。
// NEWWAIT：建了半截项目，停在「继续等通知」；NEWOUTSIDE：固定四行但项目路径不在 projects_root 下。
// USAGE：结果带 usage 和两个模型的 modelUsage（数字照真实 result.json 改的）；其余正常结果都没有用量字段。
// RESETS：撞上限，提示里的重置时间是 2 小时后（UTC 写法）。RATEOK：做了 40 个回合、结果里提到 rate limit 的正常完成。
// APIERR403、APIERR422、APIERR429、APIERR529：报错结束，带 api_error_status（403、422 的文案来自真实 result.json），没花钱。
// ORPHANDONE：接手任务做完：把条目勾掉、去掉进行中标记。
// TERMCHILD：留一个不理 SIGTERM 的子进程。KILLTICK：把启动自己的 tick（外壳的父进程）SIGKILL 掉，这时 tick 还没写完状态。
// CLAIMFIRST：像 agent 认领一样先在 BACKLOG 这一条写进行中标记（再按 SLEEP 睡）。
// INBOXWRITE：任务箱任务，把内容写进 prompt 里「产出文件：」那一行的路径；INBOXLINK：写成软链接，INBOXBLANK：写成空文件。
// INBOXBUDGET：首次写半成品后撞预算，-r 接着做时写完；INBOXBUDGETTWICE：每次都撞。INBOXRATE：写好产出，结论里提到 rate limit；INBOXLIMITFILE：写好产出但真撞上限（is_error 为真）。
// -p /usage：每次在 $HOME/fake-usage-calls 记一行（连同参数和目录）；$HOME/fake-usage.json 在就原样输出它，
// 否则输出真实 CLI 在读不到额度时的那句话（没有窗口，agentq 当未知）。
// 每次调用（连 auth status、/usage 都算）先在 $HOME/fake-claude-calls 记一行：断言「一个进程都不起」靠它。
// TASKOVERBUDGET：首次认领后撞预算，-r 续跑时做完。TASKOVERTWICE：首次认领，每次都撞。NEWOVER：首次建半截项目后撞预算，-r 接着做完。
// AGENTQ_FAKE_BUDGET：不看 prompt，直接撞预算。撞预算的结果照真实 CLI：subtype=error_max_budget_usd、is_error=true、退出码 1。
const fakeClaude = `#!/bin/sh
sedi() { sed -i.fakebak "$@" && rm -f docs/BACKLOG.md.fakebak; } # BSD 和 GNU 的 sed -i 写法不同，带后缀的两边都认
# new_first 像 agentq new 那样建项目 clip-tool：认领第一条，在任务分支上提交门脚本和 ready.json，不合并。$1 是后面的条目，$2、$3 是 verify、smoke 的退出码
new_first() {
  printf '# 待办\n\n## 待做\n\n- [ ] %s（进行中：agent/first 2026-01-01 10:00）\n%s\n\n## 已完成\n' "${FIRST:-第一条活}" "$1" > clip-tool/docs/BACKLOG.md
  printf '.claude/\n' > clip-tool/.gitignore
  git -C clip-tool init -q -b "${INITBRANCH:-main}"; git -C clip-tool add -A; git -C clip-tool -c user.name=t -c user.email=t commit -q -m init
  git -C clip-tool worktree add -q .claude/worktrees/first -b agent/first
  wt=clip-tool/.claude/worktrees/first
  mkdir -p "$wt/.agentq"
  printf '#!/bin/sh\nexit %s\n' "${2:-0}" > "$wt/.agentq/verify"; chmod 755 "$wt/.agentq/verify"
  printf '#!/bin/sh\nexit %s\n' "${3:-0}" > "$wt/.agentq/smoke"; chmod 755 "$wt/.agentq/smoke"
  echo x > "$wt/first.txt"
  printf '{"branch":"agent/first","type":"feature","acceptance":"看 first.txt","progress":"做出第一版"}\n' > "$wt/.agentq/ready.json"
  git -C "$wt" add -A; git -C "$wt" -c user.name=t -c user.email=t commit -q -m 第一版
}
echo "$1" >> "$HOME/fake-claude-calls"
if [ "$1" = "-p" ] && [ "$2" = "/usage" ]; then
  echo "$* PWD=$PWD" >> "$HOME/fake-usage-calls"
  if [ -f "$HOME/fake-usage.json" ]; then cat "$HOME/fake-usage.json"
  else echo '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage"}'; fi
  exit 0
fi
if [ "$1" = "auth" ]; then
  # BREAKSTATE：在 tick 挑活之后、启动任务之前把 state.json 换成目录，让启动后写状态文件失败
  if [ -n "$AGENTQ_FAKE_BREAK_STATE" ]; then rm -f "$HOME/.agentq/state.json"; mkdir -p "$HOME/.agentq/state.json/x"; fi
  if [ -n "$AGENTQ_FAKE_LOGGED_OUT" ]; then echo '{"loggedIn": false}'; else echo '{"loggedIn": true}'; fi
  exit 0
fi
prompt=""; resume=""; session=""; prev=""
for a in "$@"; do
  case "$prev" in -p) prompt="$a" ;; -r) resume="$a" ;; --session-id) session="$a" ;; esac
  prev="$a"
done
{
  for a in "$@"; do printf 'ARG[%s]\n' "$a"; done
  printf 'PWD[%s]\n' "$PWD"
  printf 'PPID[%s]\n' "$PPID"
  env | sed 's/^/ENV[/; s/$/]/'
} > "$AGENTQ_FAKE_LOG/$$.txt"
case "$prompt" in *IGNORETERM*) trap '' TERM ;; esac
case "$prompt" in *ORPHAN*) sleep 30 & ;; esac
case "$prompt" in *TERMCHILD*) ( trap '' TERM; sleep 30 ) & ;; esac
case "$prompt" in *KILLTICK*) kill -9 $(ps -o ppid= -p "$PPID") ;; esac
case "$prompt" in *TRANSCRIPT*)
  d="$HOME/.claude/projects/-fake/$session"; mkdir -p "$d/subagents"; echo '{}' > "$d.jsonl"
  ( while :; do echo '{}' >> "$d/subagents/agent-1.jsonl"; sleep 0.3; done ) & ;;
esac
case "$prompt" in *CLAIMFIRST*) sedi '/^- \[ \] .*CLAIMFIRST/ s/$/（进行中：agent\/t-cf 2026-01-01 10:00）/' docs/BACKLOG.md ;; esac
s=$(printf '%s' "$prompt" | sed -n 's/.*SLEEP=\([0-9]*\).*/\1/p')
[ -z "$s" ] && s=$AGENTQ_FAKE_SLEEP # 夜班、复盘的 prompt 是固定的说明文件，睡多久靠环境变量给
[ -n "$s" ] && sleep "$s"
for f in $AGENTQ_FAKE_WRITE; do mkdir -p "$(dirname "$f")"; echo report > "$f"; done # 夜班、复盘要写的报告
out=$(printf '%s\n' "$prompt" | sed -n 's/^产出文件：//p')
overbudget='{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s-over","total_cost_usd":150.01}'
if [ -n "$AGENTQ_FAKE_BUDGET" ]; then printf '%s\n' "$overbudget"; exit 1; fi
case "$prompt" in
  *大方向商量进程*) # 生成问题：$HOME/fake-discuss.json 在就原样输出；REPOTIDY 像真进程一样只读看工作目录里各仓库的 worktree 和分支数；否则一个问题两个方向
    if [ -f "$HOME/fake-discuss.json" ]; then cat "$HOME/fake-discuss.json"
    elif case "$prompt" in *REPOTIDY*) true ;; *) false ;; esac; then
      seen=""
      for g in */.git; do r=${g%/.git}; seen="$seen$r 有 $(git -C "$r" worktree list | wc -l | tr -d ' ') 个 worktree、$(git -C "$r" branch --list | wc -l | tr -d ' ') 个分支；"; done
      printf '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-dc","structured_output":{"questions":[{"text":"命令行还是窗口界面","default":"命令行（依据：要扫的是 git 仓库）"},{"text":"能清的指什么","default":"多余的 worktree 和已合并分支（依据：%s）；开发测试只用临时目录里造的数据，真实数据只读跑"}],"directions":[]}}\n' "$seen"
    else printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-dc","structured_output":{"questions":[{"text":"想要什么感觉","default":"简洁"}],"directions":["纸墨风","终端风"]}}'; fi ;;
  *产品经理调研进程*) # 产品经理调研：$HOME/fake-pm.json 在就原样输出，否则两条带出处的建议
    if [ -f "$HOME/fake-pm.json" ]; then cat "$HOME/fake-pm.json"
    else printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-pm","structured_output":{"suggestions":[{"title":"加导出 CSV","why":"同类工具都有，用户在 issue 里反复要","source":"https://github.com/example/clipper/issues/12"},{"title":"第一次打开放示例数据","why":"README 写的定位是给新手用，空白页留不住人","source":"README 写的定位：给新手用的小工具"}]}}'; fi ;;
  *界面门的独立审查进程*) # 界面门的审查进程：$HOME/fake-ui-review.json 在就原样输出，否则通过
    if [ -f "$HOME/fake-ui-review.json" ]; then cat "$HOME/fake-ui-review.json"
    else printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"看过了","session_id":"s-ui","structured_output":{"verdict":"pass","skill":"loaded","summary":"没问题","findings":[]}}'; fi ;;
  *审查门的独立审查进程*) # 引擎的审查进程：$HOME/fake-review.json 在就原样输出，否则通过
    if [ -f "$HOME/fake-review.json" ]; then cat "$HOME/fake-review.json"
    else printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"审过了","session_id":"s-rv","structured_output":{"verdict":"pass","summary":"没问题","findings":[]}}'; fi ;;
  *「主动感知」*) [ -f "$HOME/fake-sense.err" ] && cat "$HOME/fake-sense.err" >&2; cat "$HOME/fake-sense.json" ;;
  *发版准备任务*) cat "$HOME/fake-release.json" ;;
  *TASKOVERBUDGET*)
    if [ -z "$resume" ]; then
      sedi '/^- \[ \] .*TASKOVERBUDGET/ s/$/（进行中：agent\/t-over 2026-01-01 10:00）/' docs/BACKLOG.md
      printf '%s\n' "$overbudget"; exit 1
    fi
    sedi -e '/^- \[ \] .*TASKOVERBUDGET/ s/（进行中.*$//' -e '/^- \[ \] .*TASKOVERBUDGET/ s/^- \[ \]/- [x]/' docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":30,"result":"接着做完了。\n\n已合并，提交 eee5555；用户看 README。","session_id":"s-over"}' ;;
  *TASKOVERTWICE*)
    [ -z "$resume" ] && sedi '/^- \[ \] .*TASKOVERTWICE/ s/$/（进行中：agent\/t-over 2026-01-01 10:00）/' docs/BACKLOG.md
    printf '%s\n' "$overbudget"; exit 1 ;;
  *NEWOVER*)
    mkdir -p clip-tool/docs
    if [ -z "$resume" ]; then printf '%s\n' "$overbudget"; exit 1; fi
    new_first ''
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"做完了。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first","session_id":"s-over"}' ;;
  *INBOXBUDGETTWICE*) printf '半成品\n' >> "$out"; printf '%s\n' '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s-ib"}' ;;
  *INBOXBUDGET*)
    if [ -z "$resume" ]; then printf '半成品\n' > "$out"; printf '%s\n' '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s-ib"}'
    else printf '接着写完\n' >> "$out"; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"结论：接着做完了。","session_id":"s-ib"}'; fi ;;
  *INBOXRATE*) printf '# 限流调研\n' > "$out"
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"结论：Anthropic API 的 rate limit 和 usage limit 都有退避。","session_id":"s-ib"}' ;;
  *INBOXLIMITFILE*) printf '# 半成品\n' > "$out"
    printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"num_turns":2,"result":"You'"'"'ve hit your session limit","session_id":"s-ib"}' ;;
  *INBOXWRITE*) printf '# 调研结果\n\n正文\n' > "$out"
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"结论：有 5 个例子，都是订阅。\n\n详情看文件 <b>。","session_id":"s-ib"}' ;;
  *INBOXLINK*) ln -s /etc/hosts "$out"; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"写好了","session_id":"s-ib"}' ;;
  *INBOXBLANK*) : > "$out"; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"写好了","session_id":"s-ib"}' ;;
  *ROUTERFILE*) cat "$HOME/fake-router.json" ;;
  *闲时找活*) cat "$HOME/fake-suggest.json" ;;
  *第一条死活*) # 接手新项目的第一条：没做成，退出时分支上没有 ready.json
    git -C .claude/worktrees/first rm -q -f .agentq/ready.json; git -C .claude/worktrees/first -c user.name=t -c user.email=t commit -q -m 还没好
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"还是没做好。","session_id":"s-first"}' ;;
  *NEWMASTER*)
    mkdir -p clip-tool/docs
    INITBRANCH=master new_first '- [ ] 第二条活'
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"做完了。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first","session_id":"s-new"}' ;;
  *NEWFIRSTSTUCK*)
    mkdir -p clip-tool/docs
    FIRST=第一条死活 new_first '- [ ] 第二条活' 1 0
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"做完了。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first","session_id":"s-new"}' ;;
  *第一条活*) # 接手新项目的第一条：把门脚本修好，再交一次 ready.json
    wt=.claude/worktrees/first
    printf '#!/bin/sh\nexit 0\n' > "$wt/.agentq/verify"
    printf '{"branch":"agent/first","type":"feature","acceptance":"看 first.txt","progress":"做出第一版"}\n' > "$wt/.agentq/ready.json"
    echo y >> "$wt/first.txt"
    git -C "$wt" add -A; git -C "$wt" -c user.name=t -c user.email=t commit -q -m 修好
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"修好了。\n\n分支可以合并了；用户看 first.txt。","session_id":"s-first"}' ;;
  *NEWSELFMERGE*) # 不听合同：写了 ready.json 又自己合并
    mkdir -p clip-tool/docs
    new_first '- [ ] 第二条活'
    git -C clip-tool -c user.name=t -c user.email=t merge -q --no-edit agent/first
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"做完了。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first","session_id":"s-new"}' ;;
  *NEWNOREADY*) # 建了项目，第一条标了进行中，分支上没有 ready.json
    mkdir -p clip-tool/docs
    new_first '- [ ] 第二条活'
    git -C clip-tool/.claude/worktrees/first rm -q -f .agentq/ready.json; git -C clip-tool/.claude/worktrees/first -c user.name=t -c user.email=t commit -q -m 去掉
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"做完了。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first","session_id":"s-new"}' ;;
  *NEWNOTREADY*) # 第一条没做到可合并，结尾照约定写原因
    mkdir -p clip-tool/docs
    new_first '- [ ] 第二条活'
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"没做完。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：无\n第一版去哪看：README\n任务分支：没有，构建没过","session_id":"s-new"}' ;;
  *NEWBADVERIFY*|*NEWBADSMOKE*|*NEWOK*)
    mkdir -p clip-tool/docs
    case "$prompt" in
      *NEWBADVERIFY*) new_first '- [ ] 第二条活' 1 0 ;;
      *NEWBADSMOKE*) new_first '- [ ] 第二条活' 0 1 ;;
      *) new_first '- [ ] 第二条活' ;;
    esac
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"都做完了，正文里顺带提到任务分支：agent/fake。\n\n项目路径：'"$PWD"'/clip-tool\n补的假设：只给自己用；<b>不做同步</b>\n第一版去哪看：cd clip-tool 后 make run\n任务分支：agent/first","session_id":"s-new"}' ;;
  *NEWWAIT*) mkdir -p half-done/.git
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"审查人还在复跑测试，继续等通知。","session_id":"s-nw","origin":{"kind":"task-notification"},"result_index":1}' ;;
  *NEWOUTSIDE*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"做完了。\n\n项目路径：/etc\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first","session_id":"s-no"}' ;;
  *FUPTODO*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"reply":"原因是缓存没清","todo":"清缓存 —— 启动时清一次；验收：重启后不再报错（进行中：假的 10-10 03:00）"},"session_id":"s-fup"}' ;;
  *FUPREPLY*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"reply":"已经合并了，看 README","todo":""},"session_id":"s-fup"}' ;;
  *FUPBUDGET*) printf '%s\n' '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s-fup"}' ;;
  *TGDISPATCH*) # Telegram 对话：像按 dispatch 派活一样往 demo 的 BACKLOG 写一条，回答是要发给用户的消息
    sedi 's/^## 待做$/## 待做\
\
- [ ] 加个按月对比 —— 来自 Telegram/' demo/docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"🟢 <b>已派出 · 今天</b>\n\n🟢 demo　加个按月对比\n<i>12:00 · agentq 对话</i>","session_id":"s-tg"}' ;;
  *READYMERGE*)
    b=$(printf '%s' "$prompt" | sed -n 's/.*BRANCH=\([a-z0-9/-]*\).*/\1/p'); [ -z "$b" ] && b=agent/t-ready
    wt=$PWD/.claude/worktrees/$(basename "$b")
    def=$(printf '%s' "$prompt" | sed -n 's/.*默认分支是 \([^，]*\)，.*/\1/p'); [ -z "$def" ] && def=main # 只照提示词说的：合同没写清就开不出分支
    if [ -z "$resume" ]; then
      pat="READYMERGE"; case "$prompt" in *BRANCH=*) pat="READYMERGE.*BRANCH=$b([^a-z0-9/-]|\$)" ;; esac # 同项目并行时只认领自己那一条
      sedi -E "\#^- \[ \] .*$pat# s#\$#（进行中：$b 2026-01-01 10:00）#" docs/BACKLOG.md
      git add docs/BACKLOG.md; git -c user.name=t -c user.email=t commit -q -m "待办：认领"
      git worktree add -q "$wt" -b "$b" "$def"
    else
      git -C "$wt" -c user.name=t -c user.email=t merge -q -X ours --no-edit "$def" >/dev/null 2>&1
    fi
    n=$(printf '%s' "$prompt" | sed -n 's/.*FILE=\([a-z0-9.]*\).*/\1/p'); [ -z "$n" ] && n=work.txt
    echo "$$" >> "$wt/$n"
    case "$prompt" in *TASKSCRIPT*) (cd "$wt" && FAKE_PROMPT="$prompt" sh "$HOME/fake-task.sh") ;; esac
    mkdir -p "$wt/.agentq"
    ty=$(printf '%s' "$prompt" | sed -n 's/.*TYPE=\([a-z]*\).*/\1/p'); [ -z "$ty" ] && ty=feature
    printf '{"branch":"%s","type":"%s","acceptance":"看 %s","progress":"做好了 %s"}\n' "$b" "$ty" "$n" "$n" > "$wt/.agentq/ready.json"
    git -C "$wt" add -A; git -C "$wt" -c user.name=t -c user.email=t commit -q -m "做事 $n"
    case "$prompt" in *SELFREADY*) git -c user.name=t -c user.email=t merge -q --no-edit "$b" >/dev/null 2>&1 ;; esac # 不听合同，自己合并
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n分支可以合并了；用户看 README。","session_id":"s-ready"}' ;;
  *SELFMERGE*) sedi '/^- \[ \] .*SELFMERGE/d' docs/BACKLOG.md
    git add docs/BACKLOG.md; git -c user.name=t -c user.email=t commit -q -m done
    sha=$(git rev-parse HEAD)
    printf -- '- [x] 修东西 SELFMERGE —— 合并提交 %s\n' "$sha" >> docs/BACKLOG.md
    git add docs/BACKLOG.md; git -c user.name=t -c user.email=t commit -q -m done2
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n合并提交 '"$sha"'；用户看 README。","session_id":"s-vf"}' ;;
  *INTERRUPT*)
    [ -z "$resume" ] && sedi '/^- \[ \] .*INTERRUPT/ s/$/（进行中：agent\/t-int 10-10 03:00）/' docs/BACKLOG.md
    case "$resume:$prompt" in
      :*|*AGAIN*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"审查人还在复跑测试，继续等通知。","session_id":"s-int","origin":{"kind":"task-notification"},"result_index":1}' ;;
      *) sedi -e '/^- \[ \] .*INTERRUPT/ s/（进行中.*$//' -e '/^- \[ \] .*INTERRUPT/ s/^- \[ \]/- [x]/' docs/BACKLOG.md
         printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"接着做完了。\n\n已合并，提交 def5678；用户看 README。","session_id":"s-int"}' ;;
    esac ;;
  *STUCKRETAKE*) # 任务自己标卡住、引擎再接一次。再接的合同里有「引擎去掉了卡住标记」：RETAKEOK 换思路做到可合并，否则又卡住
    b=agent/t-rt; wt=$PWD/.claude/worktrees/t-rt
    claim() { sedi "/^- \[ \] .*STUCKRETAKE/ s#\$#（进行中：$b $1）#" docs/BACKLOG.md; git add docs/BACKLOG.md; git -c user.name=t -c user.email=t commit -q -m "待办：认领"; }
    stuck() { sedi -e "/^- \[ \] .*STUCKRETAKE/ s/（进行中.*\$//" -e "/^- \[ \] .*STUCKRETAKE/ s#\$#（卡住：$1，分支 $b）#" docs/BACKLOG.md; git add docs/BACKLOG.md; git -c user.name=t -c user.email=t commit -q -m "待办：卡住"; }
    case "$prompt" in
      *引擎去掉了卡住标记*)
        printf '%s\n' "$prompt" >> "$HOME/retake-prompts.txt"
        claim "2026-01-02 10:00"
        case "$prompt" in
          *RETAKEOK*)
            echo again >> "$wt/rt.txt"; mkdir -p "$wt/.agentq"
            printf '{"branch":"%s","type":"feature","acceptance":"看 rt.txt","progress":"换思路做好了"}\n' "$b" > "$wt/.agentq/ready.json"
            git -C "$wt" add -A; git -C "$wt" -c user.name=t -c user.email=t commit -q -m 换思路
            printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"查了官方文档，换成分批跑。\n\n分支 agent/t-rt 可以合并了；用户看 rt.txt。","session_id":"s-rt2"}' ;;
          *)
            stuck "上游接口一直 500"
            printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"查了上游 issue 12，换了重试还是 500。\n\n没合并，分支 agent/t-rt；第二次试了重试，查到上游 issue 12，卡在上游接口 500。","session_id":"s-rt2"}' ;;
        esac ;;
      *)
        reason="测试一直超时"; case "$prompt" in *STUCKUSER*) reason="等用户：付费，要开通付费账号" ;; esac
        claim "2026-01-01 10:00"
        git worktree add -q "$wt" -b "$b" main
        echo first > "$wt/rt.txt"; git -C "$wt" add -A; git -C "$wt" -c user.name=t -c user.email=t commit -q -m 第一次
        stuck "$reason"
        printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"试了加超时、拆测试都不行。\n\n没合并，分支 agent/t-rt；第一次试了加超时和拆测试，卡在测试超时。","session_id":"s-rt1"}' ;;
    esac ;;
  *STUCKMARK*) sedi '/^- \[ \] .*STUCKMARK/ s/$/（卡住：agent\/t-stuck 两次没过）（进行中：agent\/t-stuck 10-10 03:00）/' docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"两次换思路都没过，标了卡住，没合并。","session_id":"s-st"}' ;;
  *DONEMOVE*) sedi '/^- \[ \] .*DONEMOVE/d' docs/BACKLOG.md; printf '%s\n' '- [x] 移完 —— 合并提交 1a2b3c4；验收：看 README' >> docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做了很多。\n\n第二轮审查结论是可以合并。","session_id":"s-dm"}' ;;
  *OLDDONE*) sedi '/^- \[ \] .*OLDDONE/ s/$/（卡住：agent\/t-old 两次没过）/' docs/BACKLOG.md; printf '%s\n' '- [x] 同名 —— 合并提交 9f8e7d6；以前做过的同名条目' >> docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"两次换思路都没过，标了卡住，没合并。","session_id":"s-od"}' ;;
  *COMMITFIRST*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"已经合并，提交号 aaa1111（PR #1）。\n\n**做了什么**\n- 加了协议\n\n第二轮审查结论是可以合并。\n\n**怎么验收**\n看 PR #1。","session_id":"s-cf"}' ;;
  *APIERR403*) printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"api_error_status":403,"num_turns":1,"total_cost_usd":0,"result":"Your organization has disabled Claude subscription access for Claude Code · Use an Anthropic API key instead, or ask your admin to enable access","session_id":"s-403"}' ;;
  *APIERR422*) printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"api_error_status":422,"num_turns":1,"total_cost_usd":0,"result":"API Error: 422 格式转换错误: Responses upstream service_unavailable_error: Our servers are currently overloaded. Please try again later.","session_id":"s-422"}' ;;
  *APIERR429*) printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"num_turns":1,"total_cost_usd":0,"result":"Request rejected (429)","session_id":"s-429"}' ;;
  *APIERR529*) printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"api_error_status":529,"num_turns":1,"total_cost_usd":0,"result":"API Error: Repeated 529 Overloaded errors.","session_id":"s-529"}' ;;
  *ORPHANDONE*) sedi -e '/^- \[ \] .*ORPHANDONE/ s/（进行中.*$//' -e '/^- \[ \] .*ORPHANDONE/ s/^- \[ \]/- [x]/' docs/BACKLOG.md
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":30,"result":"接着做完了。\n\n已合并，提交 ddd4444；用户看 README。","session_id":"s-od2"}' ;;
  *USAGE*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n已合并，提交 ccc3333；用户看 README。","session_id":"s-us","total_cost_usd":4.6071268,"usage":{"input_tokens":92,"cache_creation_input_tokens":157333,"cache_read_input_tokens":5681707,"output_tokens":54650},"modelUsage":{"claude-opus-5-5":{"inputTokens":100,"outputTokens":60000,"cacheReadInputTokens":6000000,"cacheCreationInputTokens":250000,"costUSD":4.4},"claude-haiku-5-5":{"inputTokens":22,"outputTokens":9768,"cacheReadInputTokens":388024,"cacheCreationInputTokens":42335,"costUSD":0.2071268}}}' ;;
  *NOMERGE*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做了一半。","session_id":"s-nm"}' ;;
  *LIMITMID*)
    if [ -z "$resume" ]; then
      sedi '/^- \[ \] .*LIMITMID/ s/$/（进行中：agent\/t-lm 10-10 03:00）/' docs/BACKLOG.md
      printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"num_turns":12,"result":"You'"'"'ve hit your session limit","session_id":"s-lm"}'
    else
      sedi -e '/^- \[ \] .*LIMITMID/ s/（进行中.*$//' -e '/^- \[ \] .*LIMITMID/ s/^- \[ \]/- [x]/' docs/BACKLOG.md
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":30,"result":"接着做完了。\n\n已合并，提交 aaa1111；用户看 README。","session_id":"s-lm"}'
    fi ;;
  *RESETS*) r=$(TZ=UTC date -v+2H '+%-I:%M%p' 2>/dev/null || TZ=UTC date -d '+2 hours' '+%-I:%M%p')
    printf '{"type":"result","is_error":true,"num_turns":1,"result":"You'"'"'ve hit your session limit · resets %s (UTC)","session_id":"s-rs"}\n' "$r" ;;
  *RATEOK*) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":40,"result":"修好了 API rate limit 重试，撞到 usage limit 时会退避。\n\n已合并，提交 bbb2222；用户看 README。","session_id":"s-rate"}' ;;
  *HITOK*) printf '%s\n' '{"type":"result","is_error":false,"num_turns":1,"result":"You'"'"'ve hit your session limit","session_id":"s-hit"}' ;;
  *LIMIT*) printf '%s\n' '{"type":"result","is_error":true,"result":"Claude AI usage limit reached|1760000000","session_id":"s-limit"}' ;;
  *FAIL*) printf '%s\n' '{"type":"result","is_error":true,"result":"出错了：测试失败","session_id":"s-fail"}' ;;
  *EMPTY*) echo "boom: 进程崩了" >&2 ;;
  *) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。\n\n已合并，提交 abc1234；用户看 README。","session_id":"s-ok"}' ;;
esac
`

type testEnv struct {
	t      *testing.T
	bin    string // 要跑的 agentq，空的是测试编出来的那个
	home   string
	root   string
	fake   string
	fakeLg string
	extra  []string
	tuning string // config.TestTuningEnv 的值：newEnv 从配置里分出来的写死项
	dir    string // 工作目录，空就是测试进程的
}

func newEnv(t *testing.T, cfg map[string]any) *testEnv {
	t.Helper()
	e := &testEnv{t: t, home: t.TempDir(), root: t.TempDir(), fakeLg: t.TempDir()}
	e.fake = filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(e.fake, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	// 多数用例逐次手动 tick、断言每次的收尾/启动个数，所以默认关掉任务结束后的自动 tick、tick 里的巡逻、推送和产品经理调研；测它们的用例显式打开。
	full := map[string]any{"projects_root": e.root, "claude_path": e.fake, "tick_after_job": false, "patrol_enabled": false, "notify_enabled": false, "pm_enabled": false}
	for k, v := range cfg {
		full[k] = v
	}
	// 每个 claude 的父进程是任务外壳，claude 结束后它还要执行一次 tick；等它们都退出再删临时目录，
	// 否则那次 tick 会在已删掉的临时 HOME 下重新建 .agentq。（这个 Cleanup 比 TempDir 的晚注册，先执行。）
	t.Cleanup(e.waitShells)
	full, e.tuning = splitTuning(full)
	b, _ := json.Marshal(full)
	os.MkdirAll(filepath.Join(e.home, ".agentq"), 0o755)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

// run 执行 agentq；环境模仿 launchd：没有 LANG/LC_*，HOME 指向临时目录；故意带上 CLAUDECODE 验证会被去掉。
func (e *testEnv) run(args ...string) (string, int) {
	e.t.Helper()
	bin := agentqBin
	if e.bin != "" {
		bin = e.bin
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = e.dir
	var envs []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || name == "LANG" || strings.HasPrefix(name, "LC_") {
			continue
		}
		envs = append(envs, kv)
	}
	envs = append(envs, "HOME="+e.home, "AGENTQ_FAKE_LOG="+e.fakeLg, "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=parent")
	cmd.Env = append(append(envs, config.TestTuningEnv+"="+e.tuning), e.extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func (e *testEnv) mustRun(args ...string) string {
	e.t.Helper()
	out, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("agentq %v 退出码 %d：\n%s", args, code, out)
	}
	return out
}

// waitShells 等这个环境启动的任务外壳都退出（外壳在 claude 结束后还要 exec 一次 agentq tick，会写临时 HOME）。
// 假 claude 的日志叫 <它的 pid>.txt，里面的 PPID 是外壳。日志可能还没写到 PPID 那一行，甚至还没建出来（刚启动），所以要等到它写出来或者假 claude 已经退出，
// 不能看一眼没有就跳过：跳过了外壳还在，用例结束时它和 TempDir 的 RemoveAll 抢着写，报 directory not empty。
func (e *testEnv) waitShells() {
	e.t.Helper()
	pids := map[int]bool{}
	if st, err := state.Load(filepath.Join(e.home, ".agentq", "state.json")); err == nil { // 状态文件可能被用例写坏，读不了就只看日志
		for _, j := range st.Jobs {
			if j.PID > 0 {
				pids[j.PID] = true
			}
		}
	}
	logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	for _, f := range logs {
		if pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".txt")); err == nil {
			pids[pid] = true
		}
	}
	ppid := regexp.MustCompile(`PPID\[(\d+)\]`)
	for pid := range pids {
		deadline := time.Now().Add(10 * time.Second)
		for {
			b, _ := os.ReadFile(filepath.Join(e.fakeLg, strconv.Itoa(pid)+".txt"))
			if m := ppid.FindSubmatch(b); m != nil {
				shell, _ := strconv.Atoi(string(m[1]))
				waitExit(e.t, shell, 70*time.Second)
				break
			}
			if syscall.Kill(pid, 0) != nil || time.Now().After(deadline) {
				break // 假 claude 已经退出还没写（比如刚启动就被杀了），或者用例换了别的假 claude，不写这个日志
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (e *testEnv) state() *State {
	e.t.Helper()
	st, err := state.Load(filepath.Join(e.home, ".agentq", "state.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *testEnv) jobByTitle(title string) *Job {
	e.t.Helper()
	for _, j := range e.state().Jobs {
		if j.Title == title {
			return j
		}
	}
	e.t.Fatalf("没有标题为 %q 的任务", title)
	return nil
}

func (e *testEnv) nightly() string {
	day := time.Now().Format("2006-01-02") + "-agentq.md"
	b, err := os.ReadFile(filepath.Join(e.home, ".claude", "nightly", day))
	if err != nil { // 默认后端不是 Claude
		b, _ = os.ReadFile(filepath.Join(e.home, ".agentq", "nightly", day))
	}
	return string(b)
}

// waitExit 等进程退出（孤儿进程由 launchd 回收）。
func waitExit(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("进程 %d 在 %s 内没有退出", pid, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func countRunning(st *State) int { return len(st.Running()) }

// waitShells 不能看一眼日志没写完就跳过：要等到写出 PPID，再等外壳退出。
func TestWaitShellsWaitsForUnfinishedLog(t *testing.T) {
	start := func(d string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("/bin/sleep", d)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = cmd.Wait() }() // 收掉僵尸，kill(pid, 0) 才看得出退出了
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}
	for _, c := range []struct {
		name    string
		partial string // 日志一开始的内容，空表示还没建出来
	}{
		{"日志还没写到 PPID", "ARG[-p]\n"},
		{"日志还没建出来", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &testEnv{t: t, home: t.TempDir(), fakeLg: t.TempDir()}
			claude, shell := start("30"), start("1")
			log := filepath.Join(e.fakeLg, strconv.Itoa(claude.Process.Pid)+".txt")
			if c.partial != "" {
				os.WriteFile(log, []byte(c.partial), 0o644)
			} else {
				st := &State{Jobs: []*Job{{ID: "x", PID: claude.Process.Pid, Status: state.StatusRunning}}}
				os.MkdirAll(filepath.Join(e.home, ".agentq"), 0o755)
				if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
					t.Fatal(err)
				}
			}
			time.AfterFunc(200*time.Millisecond, func() {
				os.WriteFile(log, []byte(c.partial+"PPID["+strconv.Itoa(shell.Process.Pid)+"]\n"), 0o644)
			})
			e.waitShells()
			if syscall.Kill(shell.Process.Pid, 0) == nil {
				t.Error("外壳还在就返回了")
			}
		})
	}
}

func TestIntegrationLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil) // 默认 max_parallel 2、per_project 2；条目都没写范围，同项目一次一条
	alpha := testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n- [ ] 甲二 —— SLEEP=1\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 乙一 —— SLEEP=1\n")
	testkit.MkProject(t, e.root, "项目丙", "- [ ] 丙一 —— SLEEP=8\n")

	// tick 1：全局上限 2，按字母序启动 alpha、beta 各一个；每个项目每次最多 1 个
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 2") {
		t.Fatalf("tick 1 应启动 2 个：\n%s", out)
	}
	st := e.state()
	if countRunning(st) != 2 {
		t.Fatalf("应有 2 个运行中：%+v", st.Jobs)
	}
	j1 := e.jobByTitle("甲一")
	j2 := e.jobByTitle("乙一")
	if j1.Status != state.StatusRunning || j1.Project != "alpha" || j1.ProjectDir != alpha {
		t.Errorf("甲一：%+v", j1)
	}
	if !strings.HasPrefix(jobHome(j1, e), e.home) {
		t.Fatalf("任务目录不在临时 HOME 下")
	}
	prompt, _ := os.ReadFile(filepath.Join(jobHome(j1, e), "prompt.txt"))
	if !strings.Contains(string(prompt), "把 "+alpha+" 的 docs/BACKLOG.md「待做」里的这一条做到分支可合并：甲一 —— SLEEP=1。") {
		t.Errorf("prompt 不对：%s", prompt)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "运行中（2/2）") || strings.Contains(out, "进程已结束") {
		t.Errorf("status 应显示 2 个运行中：\n%s", out)
	}

	waitExit(t, j1.PID, 10*time.Second)
	waitExit(t, j2.PID, 10*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j1.PID)))
	for _, want := range []string{"ARG[-p]", "ARG[--model]\nARG[opus]", "ARG[--effort]\nARG[high]",
		"ARG[--permission-mode]\nARG[bypassPermissions]", "ARG[--output-format]\nARG[json]",
		"ARG[--name]\nARG[agentq alpha 甲一]", "PWD[" + testkit.RealPath(t, alpha) + "]"} {
		if !strings.Contains(string(fakeLog), want) {
			t.Errorf("假 claude 收到的参数缺 %q：\n%s", want, fakeLog)
		}
	}
	if strings.Contains(string(fakeLog), "ENV[CLAUDE") {
		t.Errorf("CLAUDE* 环境变量不应传给子进程：\n%s", fakeLog)
	}

	// tick 2：收尾 2 个；alpha 启动甲二；beta 的乙一在 retry_after 内不再拉起；项目丙启动丙一
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 2，启动 2") {
		t.Fatalf("tick 2：\n%s", out)
	}
	if j := e.jobByTitle("甲一"); j.Status != state.StatusDone || j.SessionID != "s-ok" || !strings.Contains(j.Summary, "已合并，提交 abc1234") {
		t.Errorf("甲一应 done 并带摘要：%+v", j)
	}
	n := e.nightly()
	if strings.Count(n, "跑完") != 2 || !strings.Contains(n, "alpha「甲一」跑完：已合并，提交 abc1234；用户看 README。") {
		t.Errorf("当天汇总不对：\n%s", n)
	}
	j3 := e.jobByTitle("甲二")
	jc := e.jobByTitle("丙一")
	// 中文项目名在没有 LANG 的环境下也要能认出进程还活着（ps 输出不能被转义）
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 0") {
		t.Errorf("丙一还在睡，不应被收尾：\n%s", out)
	}
	waitExit(t, j3.PID, 10*time.Second)
	waitExit(t, jc.PID, 15*time.Second)
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 2，启动 0") {
		t.Errorf("全部收尾后不应重复启动已做过的条目：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 4 || countRunning(st) != 0 {
		t.Errorf("应有 4 个结束的任务：%+v", st.Jobs)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "alpha：2 条（进行中 0，卡住 0）") || !strings.Contains(out, "最近结束") {
		t.Errorf("status：\n%s", out)
	}
	out = e.mustRun("logs", j1.ID)
	if !strings.Contains(out, "跑完") || !strings.Contains(out, "已合并，提交 abc1234") || !strings.Contains(out, "s-ok") {
		t.Errorf("logs：\n%s", out)
	}
}

// Home 返回任务目录（测试辅助）。
func jobHome(j *Job, e *testEnv) string { return filepath.Join(e.home, ".agentq", "jobs", j.ID) }

func TestIntegrationTimeout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"timeout": "1s"})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 睡很久 —— SLEEP=30\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 不理 TERM —— IGNORETERM SLEEP=30\n")
	e.mustRun("tick")
	ja, jb := e.jobByTitle("睡很久"), e.jobByTitle("不理 TERM")
	time.Sleep(1500 * time.Millisecond)
	start := time.Now()
	out := e.mustRun("tick")
	if time.Since(start) < proc.KillGrace {
		t.Errorf("不理 TERM 的进程应等满 %s 再 SIGKILL", proc.KillGrace)
	}
	if !strings.Contains(out, "本次收尾 2") || !strings.Contains(out, "SIGTERM 后 SIGKILL") {
		t.Errorf("tick 输出：\n%s", out)
	}
	for _, j := range []*Job{ja, jb} {
		if got := e.jobByTitle(j.Title); got.Status != state.StatusTimeout {
			t.Errorf("%s 应为 timeout：%+v", j.Title, got)
		}
		if err := syscall.Kill(-j.PID, 0); err == nil {
			t.Errorf("%s 的进程组 %d 应已被杀掉", j.Title, j.PID)
		}
	}
	if n := e.nightly(); strings.Count(n, "超时") != 2 {
		t.Errorf("当天汇总应有 2 条超时：\n%s", n)
	}
	// 超时也算试过：retry_after 内不再拉起
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Errorf("超时的条目不应马上再启动：\n%s", out)
	}
}

func TestIntegrationCooldownAndFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 4})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 撞上限 —— LIMIT\n- [ ] 甲二 —— 正常\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 报错 —— FAIL\n")
	testkit.MkProject(t, e.root, "delta", "- [ ] 成功但撞上限 —— HITOK\n")
	testkit.MkProject(t, e.root, "gamma", "- [ ] 崩溃 —— EMPTY\n")
	e.mustRun("tick")
	for _, title := range []string{"撞上限", "报错", "崩溃", "成功但撞上限"} {
		waitExit(t, e.jobByTitle(title).PID, 10*time.Second)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 4，启动 0") || !strings.Contains(out, "冷却") {
		t.Fatalf("撞上限后应收尾且不启动甲二：\n%s", out)
	}
	st := e.state()
	if d := time.Until(st.CooldownOf("")); d < 55*time.Minute || d > 61*time.Minute {
		t.Errorf("冷却截止应约为 1 小时后：%v", st.CooldownOf(""))
	}
	if j := e.jobByTitle("撞上限"); j.Status != state.StatusFailed || !j.LimitHit {
		t.Errorf("撞上限：%+v", j)
	}
	// is_error 为假但结果是上限文案（CLI 现行措辞）：仍算跑完，但要冷却、不算试过
	if j := e.jobByTitle("成功但撞上限"); j.Status != state.StatusDone || !j.LimitHit {
		t.Errorf("成功但撞上限：%+v", j)
	}
	if j := e.jobByTitle("报错"); j.Status != state.StatusFailed || j.LimitHit || !strings.Contains(j.Summary, "出错了") {
		t.Errorf("报错：%+v", j)
	}
	if j := e.jobByTitle("崩溃"); j.Status != state.StatusFailed || !strings.Contains(j.Summary, "boom") {
		t.Errorf("崩溃应 failed 且摘要带 stderr：%+v", j)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "用量冷却中") {
		t.Errorf("status 应显示冷却：\n%s", out)
	}
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "冷却") {
		t.Errorf("冷却中 run 应拒绝：%d\n%s", code, out)
	}
}

func TestIntegrationTickLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲 —— x\n")
	lock, err := fsx.AcquireLock(filepath.Join(e.home, ".agentq", "tick.lock"), 0)
	if err != nil || lock == nil {
		t.Fatalf("测试拿锁失败：%v", err)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "上一个 tick 还没结束") {
		t.Errorf("拿不到锁应直接退出：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("拿不到锁不应启动任务")
	}
	lock.Close()
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Errorf("释放锁后应能启动：\n%s", out)
	}
	waitExit(t, e.state().Jobs[0].PID, 10*time.Second)
}

func TestIntegrationNotLoggedIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.extra = []string{"AGENTQ_FAKE_LOGGED_OUT=1"}
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲 —— x\n")
	out := e.mustRun("tick")
	if !strings.Contains(out, "未登录") || !strings.Contains(out, "启动 0") {
		t.Errorf("未登录时只记日志不启动：\n%s", out)
	}
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "未登录") {
		t.Errorf("未登录时 run 应拒绝：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("不应有任务：%+v", st.Jobs)
	}
}

func TestIntegrationRunStopPauseLogs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=30\n- [ ] 甲二 关键字 —— SLEEP=30\n")
	e.mustRun("pause")
	b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "config.json"))
	if !strings.Contains(string(b), `"paused": true`) {
		t.Fatalf("pause 应写入配置：%s", b)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "已暂停") || !strings.Contains(out, "启动 0") {
		t.Errorf("暂停时 tick 不启动：\n%s", out)
	}
	out := e.mustRun("run", "alpha", "关键字")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "甲二 关键字") || !strings.Contains(out, "已暂停自动接活") {
		t.Fatalf("run 应按关键字启动甲二：\n%s", out)
	}
	j := e.jobByTitle("甲二 关键字")
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "per_project") {
		t.Errorf("同项目已满应拒绝：\n%s", out)
	}
	if out, code := e.run("run", "nosuch"); code == 0 || !strings.Contains(out, "不是 git 主目录") {
		t.Errorf("不存在的项目应报错：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "已暂停") || !strings.Contains(out, "运行中（1/2）") {
		t.Errorf("status：\n%s", out)
	}
	if out := e.mustRun("logs", j.ID); !strings.Contains(out, "还在运行") {
		t.Errorf("logs 运行中：\n%s", out)
	}
	// 停掉的条目要等 retry_after 才自动再接：stop 和 status 都说到几点、现在就做用哪条命令
	retryHint := `后才会再自动接；现在就做：agentq run alpha "甲二 关键字"`
	if out := e.mustRun("stop", j.ID); !strings.Contains(out, "已停止任务") || !strings.Contains(out, retryHint) {
		t.Errorf("stop 应说停掉的条目几点后再接：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "「甲二 关键字」上次已停止") || !strings.Contains(out, retryHint) {
		t.Errorf("status 应列出在等 retry_after 的条目：\n%s", out)
	}
	if err := syscall.Kill(-j.PID, 0); err == nil {
		t.Error("stop 后进程组应已不在")
	}
	if got := e.jobByTitle("甲二 关键字"); got.Status != state.StatusKilled {
		t.Errorf("应为 killed：%+v", got)
	}
	if out := e.mustRun("logs", j.ID); !strings.Contains(out, "已停止") {
		t.Errorf("logs：\n%s", out)
	}
	if !strings.Contains(e.nightly(), "已停止") {
		t.Errorf("汇总应记录停止：\n%s", e.nightly())
	}
	if out, code := e.run("stop", j.ID); code == 0 || !strings.Contains(out, "不用停") {
		t.Errorf("重复 stop：\n%s", out)
	}
	e.mustRun("resume")
	b, _ = os.ReadFile(filepath.Join(e.home, ".agentq", "config.json"))
	if !strings.Contains(string(b), `"paused": false`) {
		t.Errorf("resume 应写入配置：%s", b)
	}
	// run 不看 retry_after：刚停掉的甲二仍能手动再启动
	out = e.mustRun("run", "alpha", "关键字")
	if !strings.Contains(out, "甲二 关键字") {
		t.Errorf("run 应忽略 retry_after：\n%s", out)
	}
	for _, jj := range e.state().Running() {
		e.mustRun("stop", jj.ID)
	}
}

func TestIntegrationUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	if out, code := e.run(); code != 2 || !strings.Contains(out, "用法") {
		t.Errorf("无参数：%d\n%s", code, out)
	}
	if out, code := e.run("bogus"); code != 2 || !strings.Contains(out, "不认识") {
		t.Errorf("未知命令：%d\n%s", code, out)
	}
	if _, code := e.run("logs"); code != 2 {
		t.Errorf("参数不够应退出码 2")
	}
	for _, args := range [][]string{{"tick", "x"}, {"tick", "--after"}, {"tick", "--after", ""}, {"tick", "x", "y"}} {
		if _, code := e.run(args...); code != 2 {
			t.Errorf("agentq %v 应退出码 2，得到 %d", args, code)
		}
	}
}

// 冷却到期后恢复启动；撞上限的那条不受 retry_after 限制，可以再试。
func TestIntegrationCooldownExpires(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"limit_cooldown": "2s", "max_parallel": 1})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 撞上限 —— LIMIT\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("撞上限").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "冷却") {
		t.Fatalf("应进入冷却：\n%s", out)
	}
	time.Sleep(2200 * time.Millisecond)
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 1") {
		t.Fatalf("冷却过后应再试撞上限的条目：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 2 || st.Jobs[1].Title != "撞上限" {
		t.Errorf("第二个任务应是同一条：%+v", st.Jobs)
	}
	waitExit(t, st.Jobs[1].PID, 10*time.Second)
}

// 没写范围的条目同项目一次一条，即使 per_project 允许更多（之前手设 per_project≥2 时会并行，现在改成串行）。
func TestIntegrationUnscopedItemsSerial(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"per_project": 2, "max_parallel": 3})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=5\n- [ ] 甲二 —— SLEEP=5\n- [ ] 甲三 —— SLEEP=5\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("第一次 tick 只应启动 1 个：\n%s", out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Fatalf("没写范围的不并行，第二次 tick 不应再启动：\n%s", out)
	}
	for _, j := range e.state().Running() {
		e.mustRun("stop", j.ID)
	}
}

// claude 退出后同进程组里还有残留进程：任务外壳等到 claude 退出就发 SIGTERM 清掉，记进 launchd.log。
func TestIntegrationShellCleansGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 留孤儿 —— ORPHAN\n")
	e.mustRun("tick")
	j := e.jobByTitle("留孤儿")
	waitExit(t, j.PID, 10*time.Second)
	if !proc.WaitGroupGone(j.PID, 5*time.Second) {
		t.Error("残留进程组应被外壳清掉")
		syscall.Kill(-j.PID, syscall.SIGKILL)
	}
	waitFor(t, 5*time.Second, "launchd.log 记下清理残留进程", func() bool {
		return strings.Contains(e.launchdLog(), "任务 "+j.ID+" 的 claude 已退出，进程组")
	})
}

// waitFor 每 50ms 检查一次 cond，d 内没成立就失败。
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s 内没等到：%s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *testEnv) findJob(title string) *Job {
	for _, j := range e.state().Jobs {
		if j.Title == title {
			return j
		}
	}
	return nil
}

func (e *testEnv) launchdLog() string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "launchd.log"))
	return string(b)
}

// 任务结束时外壳马上 tick 一次：A 结束后不用再手动 tick，B 几秒内就被启动；B 结束后也被自动收尾。
func TestIntegrationTickAfterJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"tick_after_job": true}) // 没写范围：甲二只能等甲一结束
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n- [ ] 甲二 —— SLEEP=1\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("第一次 tick 应只启动甲一：\n%s", out)
	}
	a := e.jobByTitle("甲一")
	waitExit(t, a.PID, 10*time.Second)
	aExited := time.Now()
	waitFor(t, 5*time.Second, "甲一结束后自动启动甲二", func() bool { return e.findJob("甲二") != nil })
	t.Logf("甲一进程退出到甲二出现在 state.json：%s", time.Since(aExited).Round(time.Millisecond))
	a = e.jobByTitle("甲一")
	b := e.jobByTitle("甲二")
	if a.Status != state.StatusDone || a.EndedAt == nil {
		t.Fatalf("甲一应已被自动收尾为跑完：%+v", a)
	}
	if gap := b.StartedAt.Sub(*a.EndedAt); gap < 0 || gap > 3*time.Second {
		t.Errorf("甲二应在甲一收尾后马上启动，实际间隔 %s", gap)
	}
	if b.PID <= 0 || b.PID == a.PID {
		t.Errorf("甲二的 pid 不对：%+v", b)
	}
	// pid 是 claude 本身（假 claude 按自己的 pid 写参数记录），不是外壳
	if _, err := os.Stat(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", b.PID))); err != nil {
		t.Errorf("记下的 pid 应是 claude 进程：%v", err)
	}
	waitExit(t, b.PID, 10*time.Second)
	// 收尾后的 state.json 先落盘，那次 tick 的汇总日志行最后才写，所以等日志行而不是等状态
	waitFor(t, 5*time.Second, "甲二结束后被自动收尾", func() bool {
		return e.jobByTitle("甲二").Status == state.StatusDone && strings.Contains(e.launchdLog(), "tick（任务 "+b.ID+" 结束后）：")
	})
	log := e.launchdLog()
	for _, want := range []string{
		"tick（任务 " + a.ID + " 结束后）：运行中 1/2，本次收尾 1，启动 1",
		"启动任务 " + b.ID + "：alpha「甲二」",
		"tick（任务 " + b.ID + " 结束后）：运行中 0/2，本次收尾 1，启动 0",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("launchd.log 缺 %q：\n%s", want, log)
		}
	}
	if n := e.nightly(); strings.Count(n, "跑完") != 2 {
		t.Errorf("当天汇总应有 2 条跑完：\n%s", n)
	}
}

// 任务结束时别的 tick 正拿着锁：结束后的那次 tick 等锁而不是跳过，锁一放就接上下一条。
func TestIntegrationTickAfterJobWaitsForLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"tick_after_job": true})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n- [ ] 甲二 —— SLEEP=1\n")
	e.mustRun("tick")
	a := e.jobByTitle("甲一")
	lock, err := fsx.AcquireLock(filepath.Join(e.home, ".agentq", "tick.lock"), 0)
	if err != nil || lock == nil {
		t.Fatalf("测试拿锁失败：%v", err)
	}
	waitExit(t, a.PID, 10*time.Second)
	time.Sleep(1500 * time.Millisecond)
	if e.findJob("甲二") != nil {
		t.Fatal("锁被占着时不应启动甲二")
	}
	lock.Close()
	waitFor(t, 5*time.Second, "放锁后启动甲二", func() bool { return e.findJob("甲二") != nil })
	if strings.Contains(e.launchdLog(), "本次跳过") {
		t.Errorf("任务结束后的 tick 应等锁而不是跳过：\n%s", e.launchdLog())
	}
	waitExit(t, e.jobByTitle("甲二").PID, 10*time.Second)
}

// claude 启动不了时，外壳把错误报回启动方，不留下外壳进程。
func TestStartJobReportsStartError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := runner.StartJob(agentqBin, dir, backend.Invocation{Path: filepath.Join(dir, "没有这个 claude"), Dir: dir}, filepath.Join(dir, "launchd.log"))
	if err == nil || !strings.Contains(err.Error(), "没有这个 claude") {
		t.Errorf("应报回启动错误：%v", err)
	}
}

// claude 正常退出（is_error 为假）但 BACKLOG 那一条仍标进行中：记中断，下一次 tick 用同一会话续跑一次，续跑做完就算跑完。
func TestIntegrationInterruptResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	alpha := testkit.MkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT SLEEP=1\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断一").PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 1") || !strings.Contains(out, "续跑任务") {
		t.Fatalf("应收尾中断的任务并续跑：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 2 {
		t.Fatalf("应有原任务和续跑任务：%+v", st.Jobs)
	}
	orig, res := st.Jobs[0], st.Jobs[1]
	if orig.Status != state.StatusInterrupted || orig.SessionID != "s-int" || orig.ResumePending || orig.ResumedBy != res.ID {
		t.Errorf("原任务应记中断并指向续跑任务：%+v", orig)
	}
	for _, want := range []string{"仍标进行中", "agent/t-int", "后台通知", "续跑一次", "继续等通知"} {
		if !strings.Contains(orig.Summary, want) {
			t.Errorf("中断摘要缺 %q：%s", want, orig.Summary)
		}
	}
	if res.Status != state.StatusRunning || res.ResumeOf != orig.ID || res.Fingerprint != orig.Fingerprint {
		t.Errorf("续跑任务：%+v", res)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "续跑中（接中断的 "+orig.ID+"）") ||
		!strings.Contains(out, "「中断一」中断（已续跑："+res.ID+"）") {
		t.Errorf("status 应显示续跑中和中断：\n%s", out)
	}
	prompt, _ := os.ReadFile(filepath.Join(jobHome(res, e), "prompt.txt"))
	if !strings.Contains(string(prompt), "后台的活") || !strings.Contains(string(prompt), "中断一 —— INTERRUPT SLEEP=1") {
		t.Errorf("续跑 prompt 不对：%s", prompt)
	}
	waitExit(t, res.PID, 10*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", res.PID)))
	for _, want := range []string{"ARG[-r]\nARG[s-int]\nARG[-p]", "ARG[--disallowed-tools]\nARG[Monitor]",
		"ARG[--name]\nARG[agentq alpha 中断一]", "PWD[" + testkit.RealPath(t, alpha) + "]"} {
		if !strings.Contains(string(fakeLog), want) {
			t.Errorf("续跑的参数缺 %q：\n%s", want, fakeLog)
		}
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("续跑做完后应收尾、不再启动：\n%s", out)
	}
	if j := e.state().Jobs[1]; j.Status != state.StatusDone || !strings.Contains(j.Summary, "已合并，提交 def5678") {
		t.Errorf("续跑任务应跑完：%+v", j)
	}
	n := e.nightly()
	for _, want := range []string{"alpha「中断一」中断：BACKLOG 里这一条仍标进行中", "alpha「中断一」续跑中：上次中断（任务 " + orig.ID,
		"alpha「中断一」跑完：已合并，提交 def5678"} {
		if !strings.Contains(n, want) {
			t.Errorf("当天汇总缺 %q：\n%s", want, n)
		}
	}
}

// 续跑后又中断：记中断、写进汇总等用户，不再续跑第二次。
func TestIntegrationResumeInterruptedAgain(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 再中断 —— INTERRUPT AGAIN\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("再中断").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("应续跑一次：\n%s", out)
	}
	res := e.state().Jobs[1]
	waitExit(t, res.PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("续跑再中断后不应再启动：\n%s", out)
	}
	j := e.state().Jobs[1]
	if j.Status != state.StatusInterrupted || j.ResumePending || j.ResumedBy != "" || !strings.Contains(j.Summary, "续跑后仍中断，等你处理") {
		t.Errorf("续跑任务应记中断且不再续跑：%+v", j)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || len(e.state().Jobs) != 2 {
		t.Errorf("之后的 tick 也不应再续跑：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "「再中断」中断（续跑自 ") {
		t.Errorf("status 应显示续跑后的中断：\n%s", out)
	}
	if n := e.nightly(); !strings.Contains(n, "alpha「再中断」中断：续跑后仍中断，等你处理：BACKLOG 里这一条仍标进行中") {
		t.Errorf("当天汇总应写明续跑后仍中断：\n%s", n)
	}
}

// 条目没标进行中，但结果里没写合并情况：按中断报，不续跑。
func TestIntegrationNoMergeParagraph(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 没写合并 —— NOMERGE\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("没写合并").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("不应续跑：\n%s", out)
	}
	j := e.jobByTitle("没写合并")
	if j.Status != state.StatusInterrupted || j.ResumePending || !strings.Contains(j.Summary, "结果里没写合并情况") || !strings.Contains(j.Summary, "不续跑") {
		t.Errorf("应记中断、不续跑：%+v", j)
	}
}

// 摘要优先带合并提交号：条目移到「已完成」写了合并提交的，号放在摘要开头；结果前面段落写了提交号的，取那一段。
func TestIntegrationSummaryCommit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 移完 —— DONEMOVE\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 号在前 —— COMMITFIRST\n")
	testkit.MkProject(t, e.root, "gamma", "- [ ] 同名 —— OLDDONE\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("移完").PID, 10*time.Second)
	waitExit(t, e.jobByTitle("号在前").PID, 10*time.Second)
	waitExit(t, e.jobByTitle("同名").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 3，启动 0") {
		t.Fatalf("应收尾三个任务：\n%s", out)
	}
	// 待做里同标题的条目标了卡住没合并：不带出「已完成」里以前同名条目的旧号
	if j := e.jobByTitle("同名"); j.Status != state.StatusDone || j.Summary != "两次换思路都没过，标了卡住，没合并。" {
		t.Errorf("不应带出旧条目的合并提交：%+v", j)
	}
	if j := e.jobByTitle("移完"); j.Status != state.StatusDone || j.Summary != "合并提交 1a2b3c4；第二轮审查结论是可以合并。" {
		t.Errorf("摘要应以已完成里的合并提交开头：%+v", j)
	}
	if j := e.jobByTitle("号在前"); j.Status != state.StatusDone || j.Summary != "已经合并，提交号 aaa1111（PR #1）。" {
		t.Errorf("摘要应取写着提交号的段落：%+v", j)
	}
	out := e.mustRun("status")
	n := e.nightly()
	for _, want := range []string{"「移完」跑完：合并提交 1a2b3c4；", "「号在前」跑完：已经合并，提交号 aaa1111"} {
		if !strings.Contains(out, want) {
			t.Errorf("status 缺 %q：\n%s", want, out)
		}
		if !strings.Contains(n, want) {
			t.Errorf("当天汇总缺 %q：\n%s", want, n)
		}
	}
}

// agent 放下这一条时标了卡住、但没清进行中标记：不算中断（结果里写了没合并的原因）。
func TestIntegrationStuckNotInterrupted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 放下 —— STUCKMARK\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("放下").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("不应续跑：\n%s", out)
	}
	if j := e.jobByTitle("放下"); j.Status != state.StatusDone || j.ResumePending {
		t.Errorf("标了卡住的应算跑完：%+v", j)
	}
}

// 正文提到「卡住」的条目照常被挑中；真正标了卡住的仍跳过（哪怕排在前面）。
func TestIntegrationStuckMarkerPick(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 放下的 —— x（卡住：agent/1010-x 测试两次没过）\n- [ ] 修检测 —— 验收：正文提到卡住的照常可挑\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("应只启动正文提到卡住的那条：\n%s", out)
	}
	j := e.jobByTitle("修检测")
	waitExit(t, j.PID, 10*time.Second)
	for _, j := range e.state().Jobs {
		if j.Title == "放下的" {
			t.Errorf("标了卡住的不该被挑：%+v", j)
		}
	}
}

// 连续 3 个任务秒退：熔断（默认 30 分钟），当天汇总写报警，status 显示熔断，tick 不再启动；
// 熔断到期后第一个任务又秒退：直接再熔断，时长翻倍。
func TestIntegrationBreaker(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		testkit.MkProject(t, e.root, name, "- [ ] "+name+" 秒退 —— FAIL\n")
	}
	testkit.MkProject(t, e.root, "zeta", "- [ ] 正常 —— x\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 3") {
		t.Fatalf("应启动 3 个：\n%s", out)
	}
	for _, j := range e.state().Jobs {
		waitExit(t, j.PID, 10*time.Second)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 3，启动 0") || !strings.Contains(out, "连环失败熔断中") || !strings.Contains(out, "熔断：连续 3 个任务都在 3m 内结束且没做完") {
		t.Fatalf("3 个秒退后应熔断、不启动 zeta：\n%s", out)
	}
	st := e.state()
	if d := time.Until(st.BreakerUntil); d < 29*time.Minute || d > 30*time.Minute {
		t.Errorf("熔断应约 30 分钟：%v", st.BreakerUntil)
	}
	if st.BreakerLevel != 1 || st.FailStreak != 0 {
		t.Errorf("熔断计数：level %d streak %d", st.BreakerLevel, st.FailStreak)
	}
	if n := e.nightly(); !strings.Contains(n, "熔断：连续 3 个任务都在 3m 内结束且没做完，暂停启动新任务 30m") || !strings.Contains(n, "agentq logs ") {
		t.Errorf("当天汇总应写熔断报警：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：连环失败熔断中") {
		t.Errorf("status 应显示熔断：\n%s", out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "熔断中") {
		t.Errorf("熔断中 tick 不启动：\n%s", out)
	}
	// run 是用户明确要做的，不受熔断限制
	out = e.mustRun("run", "zeta")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "连环失败熔断中") {
		t.Errorf("run 应不受熔断限制并提示：\n%s", out)
	}
	waitExit(t, e.jobByTitle("正常").PID, 10*time.Second)
	// 用户 run 试跑成功：收尾时解除熔断，汇总记一行
	e.mustRun("tick")
	if st := e.state(); !st.BreakerUntil.IsZero() || st.BreakerLevel != 0 {
		t.Errorf("跑完应解除熔断：%v level %d", st.BreakerUntil, st.BreakerLevel)
	}
	if n := e.nightly(); !strings.Contains(n, "熔断解除：任务 ") {
		t.Errorf("汇总应记熔断解除：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：正常") {
		t.Errorf("解除后 status 应正常：\n%s", out)
	}
}

func TestIntegrationBreakerDoubles(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3, "breaker_cooldown": "2s"})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		testkit.MkProject(t, e.root, name, "- [ ] "+name+" 一 —— FAIL\n- [ ] "+name+" 二 —— FAIL\n")
	}
	e.mustRun("tick")
	for _, j := range e.state().Jobs {
		waitExit(t, j.PID, 10*time.Second)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "暂停启动新任务 2s") {
		t.Fatalf("应熔断 2 秒：\n%s", out)
	}
	time.Sleep(2200 * time.Millisecond)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 3") {
		t.Fatalf("熔断到期后应恢复启动：\n%s", out)
	}
	for _, j := range e.state().Running() {
		waitExit(t, j.PID, 10*time.Second)
	}
	out := e.mustRun("tick")
	if !strings.Contains(out, "熔断过后第一个任务又秒退，冷却时长翻倍") || !strings.Contains(out, "暂停启动新任务 4s") {
		t.Fatalf("熔断后第一个秒退应直接再熔断且翻倍：\n%s", out)
	}
	st := e.state()
	if st.BreakerLevel != 2 {
		t.Errorf("熔断过两次，level 应为 2：%d", st.BreakerLevel)
	}
	if d := time.Until(st.BreakerUntil); d < 2500*time.Millisecond || d > 4*time.Second {
		t.Errorf("第二次熔断应约 4 秒：%s", d)
	}
	if n := e.nightly(); strings.Count(n, "熔断：") != 2 {
		t.Errorf("汇总应有两行熔断（熔断中结束的另外两个不再加码）：\n%s", n)
	}
}

// 成功结果里出现 rate limit（做了很多回合）不算撞上限；撞上限时能解析出重置时间就冷却到那时。
func TestIntegrationLimitDetection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 提到限流 —— RATEOK\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("提到限流").PID, 10*time.Second)
	if out := e.mustRun("tick"); strings.Contains(out, "冷却") {
		t.Errorf("成功结果提到 rate limit 不应冷却：\n%s", out)
	}
	if j, st := e.jobByTitle("提到限流"), e.state(); j.Status != state.StatusDone || j.LimitHit || !st.CooldownOf("").IsZero() {
		t.Errorf("应跑完、不算撞上限：%+v，冷却到 %v", j, st.CooldownOf(""))
	}

	testkit.MkProject(t, e.root, "beta", "- [ ] 有重置时间 —— RESETS\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("有重置时间").PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "按提示的重置时间") {
		t.Fatalf("应按提示的重置时间冷却：\n%s", out)
	}
	// 提示只精确到分钟，重置后再加 1 分钟
	if d := time.Until(e.state().CooldownOf("")); d < 119*time.Minute || d > 121*time.Minute {
		t.Errorf("冷却应约 2 小时（按 resets 时间），实际 %s", d)
	}
}

// 做到一半撞上限（BACKLOG 已标进行中）：记中断，冷却过后用同一会话续跑。
func TestIntegrationLimitMidwayResumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"limit_cooldown": "2s"})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 半路撞上限 —— LIMITMID\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("半路撞上限").PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 0") || !strings.Contains(out, "冷却") {
		t.Fatalf("撞上限后应冷却、先不续跑：\n%s", out)
	}
	j := e.state().Jobs[0]
	if j.Status != state.StatusInterrupted || !j.LimitHit || !j.ResumePending ||
		!strings.Contains(j.Summary, "仍标进行中（分支 agent/t-lm 不存在）") || !strings.Contains(j.Summary, "冷却过后用同一会话续跑") {
		t.Fatalf("应记中断、等冷却后续跑：%+v", j)
	}
	time.Sleep(2200 * time.Millisecond)
	if out := e.mustRun("tick"); !strings.Contains(out, "续跑任务") || !strings.Contains(out, "启动 1") {
		t.Fatalf("冷却过后应续跑：\n%s", out)
	}
	res := e.state().Jobs[1]
	if res.ResumeOf != j.ID {
		t.Errorf("续跑任务应指向原任务：%+v", res)
	}
	waitExit(t, res.PID, 10*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", res.PID)))
	if !strings.Contains(string(fakeLog), "ARG[-r]\nARG[s-lm]") {
		t.Errorf("续跑应用同一会话：\n%s", fakeLog)
	}
	e.mustRun("tick")
	if r := e.state().Jobs[1]; r.Status != state.StatusDone || !strings.Contains(r.Summary, "aaa1111") {
		t.Errorf("续跑应跑完：%+v", r)
	}
}

// 每个任务的用量：有 usage 的结果记进 meta、status、当天汇总，末尾小结列最费的任务和缓存命中率；缺 usage 的照常收尾、不显示用量。
func TestIntegrationJobUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 有用量 —— USAGE\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 没用量 —— 普通\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("有用量").PID, 10*time.Second)
	waitExit(t, e.jobByTitle("没用量").PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 2，启动 0") {
		t.Fatalf("应收尾两个任务：\n%s", out)
	}
	want := backend.JobUsage{Input: 122, Output: 69768, CacheRead: 6388024, CacheWrite: 292335, CostUSD: 4.6071268}
	ju := e.jobByTitle("有用量")
	if ju.Status != state.StatusDone || ju.Usage == nil || *ju.Usage != want {
		t.Fatalf("state 里的用量不对：%+v %+v", ju, ju.Usage)
	}
	var meta Job
	b, _ := os.ReadFile(filepath.Join(jobHome(ju, e), "meta.json"))
	if err := json.Unmarshal(b, &meta); err != nil || meta.Usage == nil || *meta.Usage != want {
		t.Errorf("meta.json 里的用量不对：%v\n%s", err, b)
	}
	if !strings.Contains(string(b), `"cache_read_tokens": 6388024`) {
		t.Errorf("meta.json 字段名不对：\n%s", b)
	}
	jn := e.jobByTitle("没用量")
	if jn.Status != state.StatusDone || jn.Usage != nil {
		t.Errorf("缺 usage 的结果应照常跑完、不记用量：%+v %+v", jn, jn.Usage)
	}

	out := e.mustRun("status")
	if !strings.Contains(out, "「有用量」跑完：已合并，提交 ccc3333；用户看 README。（token 6.75M）") {
		t.Errorf("status 最近结束应带 token 量：\n%s", out)
	}
	if !strings.Contains(out, "「没用量」跑完：已合并，提交 abc1234；用户看 README。\n") {
		t.Errorf("缺 usage 的任务不显示用量：\n%s", out)
	}
	n := e.nightly()
	for _, w := range []string{
		"alpha「有用量」跑完：已合并，提交 ccc3333；用户看 README。；用量 token 6.75M（输入 122、输出 69.8K、缓存读 6.39M、缓存写 292.3K）；日志 ",
		"beta「没用量」跑完：已合并，提交 abc1234；用户看 README。；日志 ",
		"合计 1 个任务（没有用量记录的不算）", "缓存命中率 95.6%", "1. alpha「有用量」token 6.75M；" + ju.ID,
	} {
		if !strings.Contains(n, w) {
			t.Errorf("当天汇总缺 %q：\n%s", w, n)
		}
	}
	if !strings.HasSuffix(n, ledger.CostEnd+"\n") || strings.Index(n, ledger.CostBegin) < strings.LastIndex(n, "；日志 ") {
		t.Errorf("花费小结应在当天汇总末尾：\n%s", n)
	}
}
