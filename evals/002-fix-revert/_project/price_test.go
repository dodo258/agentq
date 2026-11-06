package fixture

import "testing"

func TestTotal(t *testing.T) {
	if got := Total(200, 25); got != 150 {
		t.Fatalf("Total(200, 25) = %d", got)
	}
}
