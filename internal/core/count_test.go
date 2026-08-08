package core

import "testing"

func TestCompactCount(t *testing.T) {
	tests := []struct {
		count int
		want  string
	}{
		{-1, "0"}, {0, "0"}, {1, "1"}, {999, "999"},
		{1000, "1k"}, {1540, "1.5k"}, {999499, "999k"},
		{999500, "1m"}, {14700000, "15m"}, {999500000, "1b"},
		{int(^uint(0) >> 1), "9.2e"},
	}
	for _, test := range tests {
		if got := CompactCount(test.count); got != test.want {
			t.Errorf("CompactCount(%d) = %q, want %q", test.count, got, test.want)
		}
	}
}
