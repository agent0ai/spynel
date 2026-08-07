package shortid

import "testing"

func TestDisplayUsesEightAlphanumericCharacters(t *testing.T) {
	if got := Display("019c9f42-a3b1-7ced-9e10"); got != "019c9f42" {
		t.Fatalf("Display() = %q", got)
	}
	if got := Display("thr_test"); got != "thrtest" {
		t.Fatalf("short Display() = %q", got)
	}
}
