claim agent/dark-mode
cat >> "$wt/style.css" <<'CSS'

@media (prefers-color-scheme: dark) {
  body { background: #141414; color: #ececec; }
  li { border-bottom-color: #2c2c2c; }
}
CSS
ready feature "暗色模式"
result "做好了。\n\n分支 agent/dark-mode 可以合并了。验收：系统切到深色，打开 index.html。"
