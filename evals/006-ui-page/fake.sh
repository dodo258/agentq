claim agent/page
cat > "$wt/index.html" <<'EOF'
<!doctype html>
<meta charset="utf-8">
<title>今日待办</title>
<h1>今日待办</h1>
<label><input type="checkbox"> 买菜</label>
<label><input type="checkbox"> 跑步</label>
<label><input type="checkbox"> 读书</label>
EOF
ready feature "待办页面"
result "做好了。\n\n分支可以合并了。"
