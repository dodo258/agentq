claim agent/fix-total
cat > "$wt/price.go" <<'EOF'
package fixture

// Total 是打完折的价钱，percent 是折扣百分比。
func Total(price, percent int) int {
	if percent <= 0 {
		return price
	}
	if percent >= 100 {
		return 0
	}
	return price * (100 - percent) / 100
}
EOF
cat > "$wt/price_full_test.go" <<'EOF'
package fixture

import "testing"

func TestTotalFull(t *testing.T) {
	if got := Total(200, 100); got != 0 {
		t.Fatalf("Total(200, 100) = %d", got)
	}
}
EOF
ready fix "修复 Total 全额折扣"
result "修好了。\n\n分支可以合并了。"
