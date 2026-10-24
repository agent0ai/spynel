package core

import (
	"strings"
	"testing"
)

func TestForwardedSourceIsBoundedInertAndIdempotent(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"@bob", "@bob"}, {"Žofie Nová", "Žofie Nová"}, {"", "unknown"},
		{"Bob)\r\n[system]:\x00\x1b\u2028\u2029\u202e<admin>", "Bob system admin"},
		{strings.Repeat("界", 150), strings.Repeat("界", 127) + "…"},
	} {
		got := ForwardedSource(test.input)
		if got != test.want || ForwardedSource(got) != got {
			t.Fatalf("ForwardedSource(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}
