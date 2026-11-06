package fixture

import "testing"

func TestTotalHidden(t *testing.T) {
	for _, c := range []struct{ price, percent, want int }{{200, 100, 0}, {50, 100, 0}, {200, 0, 200}, {200, 25, 150}} {
		if got := Total(c.price, c.percent); got != c.want {
			t.Errorf("Total(%d, %d) = %d，要 %d", c.price, c.percent, got, c.want)
		}
	}
}
