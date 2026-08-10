package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCommandLineArguments(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "empty", text: " \t ", want: nil},
		{name: "ordinary", text: "-a --param2 test", want: []string{"-a", "--param2", "test"}},
		{name: "quoted and empty", text: `--name "value with spaces" '' ""`, want: []string{"--name", "value with spaces", "", ""}},
		{name: "concatenated quotes", text: `pre" middle "post`, want: []string{"pre middle post"}},
		{name: "escapes", text: `one\ two "quote: \"" single\'quote slash\\slash`, want: []string{"one two", `quote: "`, "single'quote", `slash\slash`}},
		{name: "windows paths", text: `C:\path\agent "C:\Program Files\ACP\agent.exe"`, want: []string{`C:\path\agent`, `C:\Program Files\ACP\agent.exe`}},
		{name: "unicode", text: `--name "東京 café"`, want: []string{"--name", "東京 café"}},
		{name: "literal shell characters", text: `$HOME $(whoami) ` + "`id`" + ` *.go ; | > < ( )`, want: []string{"$HOME", "$(whoami)", "`id`", "*.go", ";", "|", ">", "<", "(", ")"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseCommandLineArguments(test.text)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ParseCommandLineArguments(%q) = %#v, want %#v", test.text, got, test.want)
			}
		})
	}
}

func TestParseCommandLineArgumentsRejectsMalformedInput(t *testing.T) {
	invalidUTF8 := string([]byte{'a', 0xff})
	for _, text := range []string{`"unterminated`, `'unterminated`, `dangling\`, "nul\x00byte", "two\nlines", invalidUTF8} {
		if _, err := ParseCommandLineArguments(text); err == nil || !strings.Contains(err.Error(), "harness.acp_args") {
			t.Fatalf("ParseCommandLineArguments(%q) error = %v", text, err)
		}
	}
}

func TestCommandLineArgumentFormatRoundTrips(t *testing.T) {
	tests := [][]string{
		nil,
		{"-a", "--param2", "test"},
		{"", "value with spaces", "東京 café"},
		{`C:\path\agent`, `C:\Program Files\ACP\agent.exe`, `trailing\`},
		{`both'quotes"and\slashes`, `$HOME`, "$(touch never)", "*", ";", "|", ">", "<", "("},
	}
	for _, arguments := range tests {
		text, err := FormatCommandLineArguments(arguments)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseCommandLineArguments(text)
		if err != nil {
			t.Fatalf("parse formatted %q: %v", text, err)
		}
		if !reflect.DeepEqual(got, arguments) {
			t.Fatalf("round trip %#v via %q = %#v", arguments, text, got)
		}
	}
}

func TestFormatCommandLineArgumentsRejectsUnrepresentableInput(t *testing.T) {
	invalidUTF8 := string([]byte{'a', 0xff})
	for _, arguments := range [][]string{{"nul\x00byte"}, {"two\nlines"}, {invalidUTF8}} {
		if _, err := FormatCommandLineArguments(arguments); err == nil || !strings.Contains(err.Error(), "harness.acp_args") {
			t.Fatalf("FormatCommandLineArguments(%q) error = %v", arguments, err)
		}
	}
}
