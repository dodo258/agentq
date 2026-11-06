package fixture

import "testing"

func TestCents(t *testing.T) {
	for _, c := range []struct {
		yuan float64
		want int
	}{{1, 100}, {19.99, 1999}, {0.07, 7}, {1.1, 110}} {
		if got := Cents(c.yuan); got != c.want {
			t.Errorf("Cents(%v) = %d，要 %d", c.yuan, got, c.want)
		}
	}
}
