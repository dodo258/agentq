package fixture

import "testing"

func TestModeIsFast(t *testing.T) {
	if Mode() != "fast" {
		t.Fatalf("Mode() = %q，要 fast", Mode())
	}
}
