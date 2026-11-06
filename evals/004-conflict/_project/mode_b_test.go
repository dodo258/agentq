package fixture

import "testing"

func TestModeIsSafe(t *testing.T) {
	if Mode() != "safe" {
		t.Fatalf("Mode() = %q，要 safe", Mode())
	}
}
