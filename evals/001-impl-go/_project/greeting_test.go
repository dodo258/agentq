package fixture

import "testing"

func TestGreeting(t *testing.T) {
	if got := Greeting(); got != "Hello, agentq." {
		t.Fatalf("Greeting() = %q", got)
	}
}
