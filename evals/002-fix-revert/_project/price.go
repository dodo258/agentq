package fixture

// Total 是打完折的价钱，percent 是折扣百分比。
func Total(price, percent int) int {
	if percent <= 0 || percent >= 100 {
		return price
	}
	return price * (100 - percent) / 100
}
