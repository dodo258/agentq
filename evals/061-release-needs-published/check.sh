#!/bin/sh
# 11-24：.agentq/release v0.4.0 第一步就因 unbound variable 停了，EXIT trap 的 rm 又把退出码变成 0，agentq 照记「发成」、
# 推了「发布了」、给官网项目写了「对齐新版本」，公开仓其实没有 v0.4.0 的 tag 和 Release。
# 要的是：脚本退出 0 之后核对公开仓真有这个 tag 和已发布的 Release，没有就不算发成；都有才记 done、写官网待做。
root="$EVAL_WORK/projects"; bin="$EVAL_WORK/bin"
mkdir -p "$bin" "$HOME/.agentq"
# 假 gh：查 tag、Release 看 $EVAL_WORK/tags、$EVAL_WORK/releases 里有没有，没有像真 gh 那样回 404
cat > "$bin/gh" <<GH
#!/bin/sh
for a; do
  v=\${a##*/}
  case "\$a" in
    */git/ref*/tags/*) grep -qx "\$v" "$EVAL_WORK/tags" && { printf '{"ref":"refs/tags/%s"}\n' "\$v"; exit 0; }; break ;;
    */releases/tags/*) grep -qx "\$v" "$EVAL_WORK/releases" && { printf '{"tag_name":"%s","draft":false}\n' "\$v"; exit 0; }; break ;;
    v[0-9]*.*) grep -qx "\$v" "$EVAL_WORK/releases" && { printf '{"tagName":"%s","isDraft":false}\n' "\$v"; exit 0; }; echo 'release not found' >&2; exit 1 ;;
  esac
done
echo 'gh: Not Found (HTTP 404)' >&2
exit 1
GH
chmod 755 "$bin/gh"
PATH="$bin:$PATH"; export PATH
printf '{"projects_root":"%s","paused":true,"patrol_enabled":false,"notify_enabled":true,"self_project":"demo","site_project":"site","release_repos":{"demo":"o/demo"}}\n' "$root" > "$HOME/.agentq/config.json"

mkrepo() { (cd "$1" && git init -q -b main && git add -A && git commit -q -m init); }
for n in none tag-only both; do
  rm -rf "$root" "$HOME/.agentq/release" "$HOME/.agentq/notify.json"
  mkdir -p "$root/demo/docs" "$root/demo/.agentq" "$root/site/docs"
  printf '# 待办\n\n## 待做\n\n## 已完成\n' | tee "$root/demo/docs/BACKLOG.md" > "$root/site/docs/BACKLOG.md"
  printf '# 项目\n\n- 自动发版：条件满足时发\n' > "$root/demo/AGENTS.md"
  printf '# 更新记录\n\n## 未发布\n\n- 新功能甲\n' > "$root/demo/CHANGELOG.md"
  printf '#!/bin/sh\nexit 0\n' > "$root/demo/.agentq/release"
  chmod 755 "$root/demo/.agentq/release"
  mkrepo "$root/demo" && mkrepo "$root/site" || { echo "建项目失败"; exit 2; }
  : > "$EVAL_WORK/tags"; : > "$EVAL_WORK/releases"
  [ "$n" = none ] || echo v0.4.0 > "$EVAL_WORK/tags"
  [ "$n" != both ] || echo v0.4.0 > "$EVAL_WORK/releases"
  out=$("$AGENTQ_BIN" _release demo v0.4.0 2>&1)
  rec="$HOME/.agentq/release/demo.json"
  [ -f "$rec" ] || { echo "没留下发版记录：${out}" | tail -3; exit 2; }
  done_=no; grep -q '"state": *"done"' "$rec" && done_=yes
  site=no; grep -q '对齐 demo 新版本 v0.4.0' "$root/site/docs/BACKLOG.md" && site=yes
  want=no; [ "$n" = both ] && want=yes
  if [ "$done_" != "$want" ] || [ "$site" != "$want" ]; then
    echo "公开仓 ${n}：记成发成 ${done_}、写了官网待做 ${site}，都要是 ${want}：${out}" | tail -4
    exit 1
  fi
  if [ "$want" = no ] && grep -q '发布了 demo v0.4.0' "$HOME/.agentq/notify.json" 2>/dev/null; then
    echo "公开仓 ${n}：没发成却推了「发布了」"
    exit 1
  fi
done
