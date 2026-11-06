claim agent/cents
cat > "$wt/money.go" <<'EOF'
package fixture

import "math"

// Cents 把元换成分。
func Cents(yuan float64) int { return int(math.Round(yuan * 100)) }
EOF
ready fix "修复 Cents 的舍入"
result "修好了，测试没动。\n\n分支可以合并了。"
