package flow

import (
	_ "embed"
	"fmt"
	"strings"

	"agentq/internal/order"
)

//go:embed standards.md
var standards string

// 每个会话都带的底线。
const ground = `
规矩：
- 先读项目的 CLAUDE.md、AGENTS.md、LESSONS.md（有就读）。现成能用的就用，走直路，不加没要的功能。
- 没有人能回答问题：小的取舍自己定，写进提交说明。
- 不付款、不对外发布、不推远程、不删用户数据、不改账号和系统设置、不动 ~/.claude 和 ~/.agentq。
- 不派子代理去审查；测试和构建都等它跑完，不留在后台。
- 验证做到够用就停：验收没写明要的长跑、压测、实盘盯着看，一律不做；要等时间过去的逻辑用假时钟测。一次等待不超过 3 分钟。
- 提交只新增，不 amend、不 rebase、不改已有历史。`

const planSchema = `{"type":"object","properties":{"small":{"type":"boolean"},"title":{"type":"string"},"summary":{"type":"string"},"spec":{"type":"string"},"stop":{"type":"string"},"later":{"type":"array","items":{"type":"string"}},"modules":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"goal":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}},"needs":{"type":"array","items":{"type":"string"}},"accept":{"type":"string"},"hard":{"type":"boolean"}},"required":["name","goal","paths","needs","accept","hard"]}}},"required":["small","title","summary","spec","stop","later","modules"]}`

func planPrompt(o *order.Order) string {
	return fmt.Sprintf(`你是产品经理。用户提了一条需求，你想清楚要做成什么样，划好模块，交给几个会话同时去做。你只读，不改文件。

需求（用户原话）：
%s

1. 先看项目现状和代码，连同 docs/specs/ 里以前的方案、AGENTS.md、DECISIONS.md：项目的规矩和定过的事以这些文件为准。分清是小改动还是大活。一个会话一两个小时能做完的（修 bug、改文案、加个小功能）算小改动：small 写 true，modules 只写一个，spec 写几句要点，不用上网。
2. 大活：上网看 3 到 5 个同类怎么做、用户抱怨什么，归纳出我们做成什么样、凭什么比它们好用，再定方案。带出处，不堆资料。
3. spec 用 markdown 写给做的人看：给谁用、要什么、不做什么、技术选型、模块划分、模块之间的接口（写到类型和函数签名）、怎么验收。有界面的写清设计语言，不要模板脸。需求里已经指了方案文件的就照它来，不另写一套。
   需求里写着做到一半的分支（进行中：agent/…、接着做：分支 …、卡住说明里的分支）的，先看那条分支做到哪了，照它接着做；第一块的 goal 写明先把那条分支合进来，再做剩下的。
4. modules 按模块划，每块只动自己的目录（paths 不重叠），能单独测试、单独替换。name 用英文短名。needs 写必须等哪几块先做完，能靠接口先约好的就不要等。多块都要用的公共接口、骨架单独划成第一块。一般 2 到 6 块，不为拆而拆。accept 写能看到的结果，几分钟内验得出来；除非用户原话要，不写“实跑多少分钟”。公共接口和骨架、并发、算法、钱和数据安全、牵动全局的 hard 写 true，交给最强的写；照接口实现的普通活写 false。
5. 项目没有可执行的 .agentq/verify（一条命令跑全部检查，退出码 0 算过），就在第一块里要求补上。
6. 只有必须用户出面才能做的（付款、发布、删用户数据、要账号密码）才在 stop 写一句要用户定什么，其余 stop 留空，自己定。
7. 一次做不完的大项目分期：spec 写全貌和分期，modules 只划第一期（第一期做完就要能用），后面每期在 later 写一条「标题 —— 要什么、验收（依赖：前一期标题）」，会写进项目待办。做得完的 later 留空。
8. title 十几个字，summary 两三句人话：做什么、分几块、怎么算做好。

%s%s`, o.Text, standards, ground)
}

func buildPrompt(o *order.Order, m order.Module) string {
	var b strings.Builder
	fmt.Fprintf(&b, "当前目录是你自己的 git worktree，分支已经开好。把下面这一块做完并提交。\n\n需求（用户原话）：\n%s\n\n方案：\n%s\n", o.Text, o.Plan.Spec)
	fmt.Fprintf(&b, "\n你做的这一块：%s\n要做：%s\n你的目录：%s\n验收：%s\n", m.Name, m.Goal, strings.Join(m.Paths, "、"), m.Accept)
	if len(o.Plan.Modules) > 1 {
		b.WriteString("\n别的块有别的会话同时在做：只改你的目录，按方案里的接口写，不替别的块写实现。接口确实不够用时按最小的改，提交说明写清。\n")
	}
	b.WriteString("\n写测试，跑和改动有关的测试、静态检查和构建；整套检查由 agentq 合到一起后跑，你不用跑。修 bug 在根因处改，附一个不修就会失败的测试。有界面的自己截图看一遍再交。\n")
	b.WriteString("做完全部提交，工作目录要干净。最后单独一行写「怎么看：……」，一句话、60 个字以内，告诉用户去哪看、点什么。\n")
	return b.String() + ground
}

func conflictPrompt(branch string) string {
	return fmt.Sprintf("你做完之后，别人的改动先合进来了。把分支 %s 合进当前分支（git merge，不 rebase），解决冲突，两边的功能都要在，跑有关的测试，提交。不借这次改别的。", branch)
}

func redPrompt(log, note string) string {
	return fmt.Sprintf("当前目录是合到一起的代码，跑 .agentq/verify %s，日志在 %s。看日志找根因，修好，跑有关的测试，提交。不靠改测试、跳过测试、放宽超时来过；测试本身写错的可以改，提交说明写清为什么。只修这个，不改别的。\n%s", note, log, ground)
}

const auditSchema = `{"type":"object","additionalProperties":false,"properties":{"summary":{"type":"string"},"bugs":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"module":{"type":"string"},"where":{"type":"string"},"what":{"type":"string"},"how":{"type":"string"}},"required":["module","where","what","how"]}},"risks":{"type":"array","items":{"type":"string"}}},"required":["summary","bugs","risks"]}`

func auditPrompt(o *order.Order) string {
	return fmt.Sprintf(`这条需求做完合到一起了，测试也过了。你做唯一一次全面审计，审完修一次就交付，不会再审。没人给你看方案和做的过程：你只照用户的原话看成品。你只看，不改文件；可以跑测试和程序。

需求（用户原话）：
%s

改动：git diff %s HEAD

像用户那样把主要用法从头到尾走一遍，再把代码全看一遍：并发、出错和中断时会怎样、数据会不会丢、权限和输入、密钥、依赖的版本和已知漏洞（拿不准上网查官方文档）。bugs 只放大 bug：需求要的主要功能没做到或用不了；原来好用的坏了；正常使用会崩；丢数据、算错钱、泄密；模块之间对不上；能被人利用的漏洞。
risks 放现在不会出事、以后要留意的，一句一条，最多 5 条，没有就空着。这些这次不改，只告诉用户。
不报：风格、命名、理论上的可能、要几个少见条件凑齐才出的、说明文字差一点。程序没有完美的，没有大 bug 就 bugs 留空，不凑数。
每条写 module（哪一块）、where（文件:行）、what（什么问题、用户会看到什么）、how（怎么复现，你亲自验证过）。
`, o.Text, o.Base) + ground
}

const fixSchema = `{"type":"object","properties":{"results":{"type":"array","items":{"type":"object","properties":{"result":{"type":"string","enum":["fixed","invalid","failed"]},"note":{"type":"string"}},"required":["result","note"]}}},"required":["results"]}`

func fixPrompt(o *order.Order) string {
	var b strings.Builder
	b.WriteString("当前目录是合到一起的代码。审计找到下面这些大 bug。先逐条照复现步骤核对：不成立的不改；成立的一次全修好。在根因处改，同样的写法在别处也有就一起改；每条附一个不修就会失败的测试。跑有关的测试，提交。只修这些。\n")
	for i, x := range o.Bugs {
		fmt.Fprintf(&b, "\n%d. [%s] %s\n   %s\n   复现：%s\n", i+1, x.Module, x.Where, x.What, x.How)
	}
	b.WriteString("\n修完在项目 LESSONS.md 里记一下为什么会犯：每类一行，「以后怎么做 —— 因为出过什么」，已经有的不重复写。\n最后照编号逐条给结果：fixed 修好并验证过，invalid 核对后不成立，failed 没修好；note 一句话说明。如实写。\n")
	return b.String() + ground
}

// ChatPrompt 是收到用户一条消息时的说明：是需求就开工，问的就答。
func ChatPrompt(self, text string) string {
	return fmt.Sprintf(`用户在手机上发来一条消息，你是 agentq 的入口。当前目录下每个子目录是一个项目。

消息：
%s

- 是要做的事：认出哪个项目（看目录名和各自的 AGENTS.md、README），把用户原话（补上分得清的上下文，不改意思）从标准输入交给 %s do -C <项目目录> - ，一件事一条。说的是项目待办（docs/BACKLOG.md）里的，用 %s todo -C <项目目录> 看，后面加关键字做一条，加 all 全做；“先记着”就写进待办的「待做」一节，一行 - [ ] 标题 —— 要什么，提交。认不出是哪个项目就列出候选问一句。别的你不动手做。
- 是问进度、要停、要接着做：用 %s ls、log、stop、retry。
- 是别的问题：看一看再答。只读，不改文件。
最后的回答就是发到手机上的话：简体中文，第一行一句结论，后面最多 4 行，一行一件事，不用 markdown，不贴长段落。
`, text, self, self, self)
}
