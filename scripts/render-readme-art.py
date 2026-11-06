#!/usr/bin/env python3
"""生成 README 的配图：logo、终端图、流程图、手机消息图，每张深浅各一份，放 docs/brand/readme/。

用法：python3 -I scripts/render-readme-art.py [输出目录]
要本机装 Chrome（CHROME 环境变量可改路径）。图里的文字都在这个文件里，改完重跑。
图里的内容来自真实运行结果，产品输出变了就照真实输出改这里，不要写程序做不到的事。
新增或改动图片后要把 sha256 更新进 config/clean-source.json 的 approved_files。
"""
import hashlib
import os
import subprocess
import sys
import tempfile

CHROME = os.environ.get("CHROME", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "docs", "brand", "readme")

# 颜色来自 logo：墨色、纸色、队列的琥珀点。琥珀只标「在排队、要注意、已完成的动作」。
THEMES = {
    "dark": dict(bg="#16181D", raise_="#1E2128", ink="#EDE6D8", mute="#9C988D", line="#30333B", amber="#F0A13A"),
    "light": dict(bg="#FBF8F1", raise_="#F3EEE3", ink="#16181D", mute="#6A665C", line="#DAD3C4", amber="#B5690F"),
}
MONO = "'SF Mono','SFMono-Regular',Menlo,'PingFang SC',monospace"
SANS = "-apple-system,'PingFang SC','Helvetica Neue',sans-serif"


def logo(t: dict[str, str]) -> str:
    return (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="180 180 660 660" width="240" height="240">'
        f'<path d="M518.6,627.9 A190,190 0 1 1 627.9,518.6" fill="none" stroke="{t["ink"]}" stroke-width="104" stroke-linecap="round"/>'
        f'<circle cx="635.8" cy="635.8" r="54" fill="{t["amber"]}"/>'
        f'<circle cx="713.6" cy="713.6" r="44" fill="{t["amber"]}"/>'
        f'<circle cx="781.5" cy="781.5" r="34" fill="{t["amber"]}"/></svg>'
    )


def page(t: dict[str, str], body: str, width: int) -> str:
    css = f"""
    *{{box-sizing:border-box;margin:0}}
    html,body{{background:transparent;width:{width}px}}
    .win{{background:{t['bg']};border:1px solid {t['line']};border-radius:12px;overflow:hidden;color:{t['ink']}}}
    .bar{{display:flex;align-items:center;gap:6px;height:38px;padding:0 16px;border-bottom:1px solid {t['line']};font:12.5px {MONO};color:{t['mute']}}}
    .bar .d{{border-radius:50%;background:{t['amber']}}}
    .bar .path{{margin-left:10px}}
    pre{{font:14.5px/1.72 {MONO};padding:18px 22px 20px;white-space:pre-wrap;color:{t['mute']}}}
    b{{font-weight:500;color:{t['ink']}}}
    i{{font-style:normal;color:{t['amber']}}}
    .gap{{display:block;height:13px}}
    """
    return f'<!doctype html><meta charset="utf-8"><style>{css}</style><body>{body}</body>'


# scripts/prepare-public.sh 用 AGENTQ_REPO=owner/repo 重新生成，把仓库名画进终端图。
REPO_HTML = os.environ.get("AGENTQ_REPO", "<owner>/agentq").replace("<", "&lt;").replace(">", "&gt;")

DOTS = '<span class="d" style="width:10px;height:10px"></span><span class="d" style="width:8px;height:8px"></span><span class="d" style="width:6px;height:6px"></span>'

# 安装段落是 install.sh 和 agentq start 的真实文字；后面的命令、status 和结果行来自一次试跑（项目 trial-launchd、trial-greeting）。
HERO = f"""<div class="win"><div class="bar">{DOTS}<span class="path">trial-launchd</span></div><pre><i>$</i> <b>curl -fsSL https://github.com/{REPO_HTML}/releases/latest/download/install.sh | sh</b>
<i>✓</i> 下载 agentq_darwin_arm64.tar.gz
<i>✓</i> 校验通过
<i>✓</i> 安装程序和技能
agentq 装好了。
· 你交代的任务由官方 CLI 的独立进程完成，它们能读写这个用户的所有文件（包括本地密钥）、运行命令。
· 任务内容会发给你所用的模型服务，用你自己的账号额度。agentq 本身不收集数据。
· 没有待做时什么都不做，不消耗额度。
<b>按回车同意并启动，输入 n 只安装不启动：⏎</b>
<i>✓</i> 后台已启动，每 5 分钟看一次待做。
<span class="gap"></span><i>$</i> <b>agentq add "做一个单页的产品介绍网页（大方向） —— 给一个叫「加法匠」的小工具做介绍页"</b>
<span class="gap"></span><i>$</i> <b>agentq status</b>
调度：正常
额度：会话 42%；本周 72%
运行中（1/2）：trial-greeting「实现 Greeting(name string) string」
<span class="gap"></span>门：trial-launchd「做一个单页的产品介绍网页（大方向）」过门合并，<i>合并提交 72f2bffb8398</i>
</pre></div>"""


def flow(t: dict[str, str], en: bool) -> str:
    def L(zh, e):
        return e if en else zh

    ink, mute, line, amber, raise_ = t["ink"], t["mute"], t["line"], t["amber"], t["raise_"]

    def box(x, y, w, h, title, lines, mono=False, strong=False):
        s = f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="{raise_ if strong else "none"}" stroke="{amber if strong else line}"/>'
        fam = MONO if mono else SANS
        s += f'<text x="{x + 16}" y="{y + 29}" font-family="{fam}" font-size="15" font-weight="600" fill="{ink}">{title}</text>'
        for i, text in enumerate(lines):
            s += f'<text x="{x + 16}" y="{y + 54 + i * 21}" font-family="{SANS}" font-size="13" fill="{mute}">{text}</text>'
        return s

    def arrow(x1, y1, x2, y2, color=None, dash=False):
        c = color or mute
        d = ' stroke-dasharray="5 5"' if dash else ""
        return f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{c}" stroke-width="1.4"{d} marker-end="url(#a{"m" if color else "n"})"/>'

    def label(x, y, s, color=None, anchor="start"):
        return f'<text x="{x}" y="{y}" font-family="{SANS}" font-size="12.5" fill="{color or mute}" text-anchor="{anchor}">{s}</text>'

    o = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1000 432" width="1000" height="432"><defs>'
    for name, c in (("n", mute), ("m", amber)):
        o += f'<marker id="a{name}" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path d="M0,0.8 L7,4 L0,7.2" fill="none" stroke="{c}" stroke-width="1.3"/></marker>'
    o += "</defs>"
    o += box(0, 20, 200, 124, L("你说一句", "You say it"), [L("Claude Code 会话", "A Claude Code session"), "Telegram", L("命令行 agentq add", "agentq add")])
    o += box(250, 20, 200, 124, "docs/BACKLOG.md", [L("每个项目一份", "One per project"), L("从上往下做", "Worked top to bottom"), L("随仓库提交", "Committed with the repo")], mono=True)
    o += box(500, 20, 210, 124, "agentq tick", [L("每 5 分钟，任务结束时也跑", "Every 5 min and after each job"), L("暂停、额度、熔断", "Pause, quota, breaker"), L("并发、登录", "Concurrency, sign-in")], mono=True, strong=True)
    o += box(760, 20, 240, 124, "claude -p", [L("独立进程，独立 worktree", "Own process, own worktree"), L("认领、实现、自测", "Claim, build, self-test"), L("写 ready.json 就退出", "Writes ready.json, exits")], mono=True)
    o += arrow(200, 82, 248, 82) + arrow(450, 82, 498, 82) + arrow(710, 82, 758, 82)
    o += box(760, 270, 240, 124, L("过门", "Gates"), [L("合并态上跑 .agentq/verify", "Runs .agentq/verify on the merge"), L("另开进程审查", "A separate process reviews"), L("都过了，引擎合到 main", "All pass: engine merges to main")])
    o += box(500, 270, 210, 124, L("收尾，告诉你", "Wraps up, tells you"), [L("当天汇总和早报", "Daily digest, morning report"), L("Telegram（可选）", "Telegram (optional)"), "agentq status"])
    o += arrow(880, 144, 880, 268)
    o += arrow(760, 332, 712, 332)
    o += label(870, 250, "ready.json", None, "end")
    # 两条回路
    o += f'<path d="M 840 144 C 840 196, 760 196, 610 146" fill="none" stroke="{amber}" stroke-width="1.4" stroke-dasharray="5 5" marker-end="url(#am)"/>'
    o += label(640, 216, L("中断、撞上限：同一会话续跑", "Interrupted or limited: same session resumes"), amber, "middle")
    o += f'<path d="M 960 270 V 146" fill="none" stroke="{amber}" stroke-width="1.4" stroke-dasharray="5 5" marker-end="url(#am)"/>'
    for i, text in enumerate(L(["门没过：", "退回接着改"], ["Not passed:", "sent back"])):
        o += label(952, 200 + i * 18, text, amber, "end")
    o += box(250, 270, 200, 124, L("等你定", "Your call"), [L("还不过、判不了", "Still failing, or can't tell"), L("Telegram 卡片（可选）", "Telegram card (optional)"), "agentq gate pass"])
    o += f'<line x1="500" y1="332" x2="452" y2="332" stroke="{amber}" stroke-width="1.4" stroke-dasharray="5 5" marker-end="url(#am)"/>'
    return o + "</svg>"


# 决定卡片的问题、方向和完成推送来自同一次试跑。不写时间和账号。
def phone(t: dict[str, str]) -> str:
    me = f'background:{t["amber"]};color:{t["bg"]};margin-left:auto'
    bot = f'background:{t["raise_"]};color:{t["ink"]}'
    bubble = "max-width:92%;padding:10px 13px;border-radius:12px;margin-bottom:10px;font:14px/1.62 " + SANS
    foot = f'<div style="margin-top:4px;font-size:12px;color:{t["mute"]}">agentq</div>'
    btn = f'border:1px solid {t["line"]};border-radius:8px;padding:7px 0;text-align:center;flex:1;font:13.5px {SANS};color:{t["ink"]}'
    return f"""<div class="win"><div class="bar">{DOTS}<span class="path">Telegram</span></div>
<div style="padding:16px 14px 6px;display:flex;flex-direction:column">
<div style="{bubble};{bot}">🔴 <b>要你定</b><br><br>🔴 trial-launchd　整体想要什么感觉？（默认：简洁、克制、有点工匠气的中文页面）<br>A. 纸墨工匠风<br>B. 极简工具风<br>C. 深色终端风{foot}</div>
<div style="display:flex;gap:8px;margin:-2px 0 12px"><div style="{btn}">A</div><div style="{btn}">B</div><div style="{btn}">C</div></div>
<div style="{bubble};{me}">B</div>
<div style="{bubble};{bot}">🟢 trial-launchd「做一个单页的产品介绍网页（大方向）」做完了，已合并；合并提交 72f2bffb8398{foot}</div>
</div></div>"""


def hero_height() -> int:
    body = HERO.split("<pre>", 1)[1]
    lines = body.count("\n")
    gaps = body.count('class="gap"')
    return int(39 + 38 + lines * 24.94 + gaps * 13 + 4)


def render(html: str, path: str, width: int, height: int) -> None:
    with tempfile.TemporaryDirectory() as d:
        src = os.path.join(d, "p.html")
        with open(src, "w", encoding="utf-8") as f:
            f.write(html)
        subprocess.run(
            [CHROME, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=2",
             "--default-background-color=00000000", f"--window-size={width},{height}",
             f"--screenshot={path}", "file://" + src],
            check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60,
        )


def main() -> None:
    os.makedirs(OUT, exist_ok=True)
    made = []
    for name, t in THEMES.items():
        p = os.path.join(OUT, f"logo-{name}.svg")
        with open(p, "w", encoding="utf-8") as f:
            f.write(logo(t) + "\n")
        made.append(p)
        p = os.path.join(OUT, f"hero-{name}.png")
        render(page(t, HERO, 1000), p, 1000, hero_height())
        made.append(p)
        p = os.path.join(OUT, f"telegram-{name}.png")
        render(page(t, phone(t), 440), p, 440, 471)
        made.append(p)
        for lang, en in (("zh", False), ("en", True)):
            p = os.path.join(OUT, f"flow-{lang}-{name}.svg")
            with open(p, "w", encoding="utf-8") as f:
                f.write(flow(t, en) + "\n")
            made.append(p)
    for p in made:
        with open(p, "rb") as f:
            print(hashlib.sha256(f.read()).hexdigest(), os.path.relpath(p, ROOT))


if __name__ == "__main__":
    main()
