package core

import "testing"

func TestRemoteCommandRecognition(t *testing.T) {
	for _, command := range SlashCommands() {
		for _, channel := range []string{"telegram", "whatsapp"} {
			if !IsCommand(channel, command.Value) {
				t.Fatalf("catalog command not recognized: %s", command.Value)
			}
		}
	}
	for _, test := range []struct {
		channel, text, name, args string
		known                     bool
	}{
		{"telegram", " /STATUS@spy value ", "status", "value", true},
		{"whatsapp", "/STATUS@spy", "status@spy", "", false},
		{"telegram", "/start payload", "start", "payload", true},
		{"whatsapp", "/start", "start", "", false},
		{"telegram", "/unknown", "unknown", "", false},
		{"telegram", "quote /restart", "quote", "/restart", false},
		{"telegram", "[Generated voice transcription]\n/restart", "[generated", "voice transcription]\n/restart", false},
		{"telegram", "/", "", "", true},
		{"telegram", "/@spy", "", "", false},
		{"telegram", "/todo request", "todo", "request", true},
		{"whatsapp", "/logs", "logs", "", true},
	} {
		name, args := ParseCommand(test.channel, test.text)
		if name != test.name || args != test.args || IsCommand(test.channel, test.text) != test.known {
			t.Errorf("%+v: got %q %q", test, name, args)
		}
	}
}
