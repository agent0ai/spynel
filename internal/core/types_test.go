package core

import "testing"

func TestSpynelASCIIUsesCanonicalFiveRowWelcomeLogo(t *testing.T) {
	want := `     ████     ████     ███████ ██████  ██    ██ ███    ██ ███████ ██
   ██    ██ ██    ██   ██      ██   ██  ██  ██  ████   ██ ██      ██
  ██  ██  ███  ██  ██  ███████ ██████    ████   ██ ██  ██ █████   ██
   ██    ██ ██    ██        ██ ██         ██    ██  ██ ██ ██      ██
     ████     ████     ███████ ██         ██    ██   ████ ███████ ███████`
	if SpynelASCII != want {
		t.Fatalf("welcome logo changed:\n%s", SpynelASCII)
	}
}
