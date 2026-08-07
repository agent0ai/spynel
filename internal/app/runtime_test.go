package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRuntimeWriterCapturesLinesAndPublishesLatestCounts(t *testing.T) {
	runtime := NewRuntime()
	if _, err := runtime.Write([]byte("Spynel sta")); err != nil {
		t.Fatal(err)
	}
	if len(runtime.Logs()) != 0 {
		t.Fatal("partial line was published before completion")
	}
	if _, err := runtime.Write([]byte("rted\nsecond entry\n")); err != nil {
		t.Fatal(err)
	}
	runtime.Log("third entry")

	logs := runtime.Logs()
	if len(logs) != 3 || logs[0].Text != "Spynel started" || logs[2].Text != "third entry" {
		t.Fatalf("logs = %#v", logs)
	}
	if output := formatLogs(logs); !strings.Contains(output, "3 entries") || !strings.Contains(output, "Spynel started") {
		t.Fatalf("formatted logs = %s", output)
	}
	select {
	case status := <-runtime.Updates():
		if status.Logs != 3 || status.Jobs != 0 {
			t.Fatalf("latest status = %#v", status)
		}
	default:
		t.Fatal("runtime did not publish counts")
	}
}

func TestRuntimeClearLogsDropsEntriesAndPartialOutput(t *testing.T) {
	runtime := NewRuntime()
	runtime.Log("first")
	if _, err := runtime.Write([]byte("unfinished")); err != nil {
		t.Fatal(err)
	}
	if count := runtime.ClearLogs(); count != 1 {
		t.Fatalf("cleared count = %d, want 1", count)
	}
	if len(runtime.Logs()) != 0 || runtime.Status().Logs != 0 {
		t.Fatalf("logs remain after clear: %#v", runtime.Logs())
	}
	if _, err := runtime.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	logs := runtime.Logs()
	if len(logs) != 1 || logs[0].Text != "new" {
		t.Fatalf("partial output survived clear: %#v", logs)
	}
	select {
	case status := <-runtime.Updates():
		if status.Logs != 1 {
			t.Fatalf("latest status = %#v", status)
		}
	default:
		t.Fatal("runtime did not publish latest count")
	}
}

func TestRuntimeReusesSessionJobAndKeepsNumericOrder(t *testing.T) {
	runtime := NewRuntime()
	first := runtime.BeginJob("one", "tui", "local", "first job")
	if duplicate := runtime.BeginJob("one", "tui", "local", "duplicate"); duplicate != first {
		t.Fatalf("duplicate session job ID = %d, want %d", duplicate, first)
	}
	second := runtime.BeginJob("two", "telegram", "42", "second job")
	if first != 1 || second != 2 {
		t.Fatalf("job IDs = %d, %d", first, second)
	}
	jobs := runtime.Jobs()
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 2 {
		t.Fatalf("jobs = %#v", jobs)
	}
	runtime.EndJob(first)
	if status := runtime.Status(); status.Jobs != 1 {
		t.Fatalf("status after completion = %#v", status)
	}
}

func TestRuntimeLogIsBoundedAndKeepsNewestEntriesInOrder(t *testing.T) {
	runtime := NewRuntime()
	for index := 0; index < maxLogEntries+7; index++ {
		runtime.Log(fmt.Sprintf("entry-%04d", index))
	}
	logs := runtime.Logs()
	if len(logs) != maxLogEntries || logs[0].Text != "entry-0007" || logs[len(logs)-1].Text != fmt.Sprintf("entry-%04d", maxLogEntries+6) {
		t.Fatalf("bounded logs = %d entries, first %q, last %q", len(logs), logs[0].Text, logs[len(logs)-1].Text)
	}
	runtime.Log(strings.Repeat("x", maxLogEntryRunes+100))
	logs = runtime.Logs()
	if got := len([]rune(logs[len(logs)-1].Text)); got != maxLogEntryRunes || !strings.HasPrefix(logs[len(logs)-1].Text, "…") {
		t.Fatalf("bounded log entry has %d runes: %.20q", got, logs[len(logs)-1].Text)
	}
}

func TestSanitizeLogTextRemovesTerminalCommandsAndUnsafeControls(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "CSI styles", input: "\x1b[2m\x1b[31merror\x1b[0m", want: "error"},
		{name: "OSC hyperlink", input: "\x1b]8;;https://example.com\x1b\\click\x1b]8;;\x1b\\", want: "click"},
		{name: "C1 CSI", input: "\u009b1;32mgreen\u009b0m", want: "green"},
		{name: "raw byte C1 CSI", input: string([]byte{0x9b, '3', '1', 'm', 'r', 'e', 'd', 0x9b, '0', 'm'}), want: "red"},
		{name: "control strings", input: "left\x1bPprivate\x1b\\right\x1b]title\aright", want: "leftrightright"},
		{name: "BEL inside DCS", input: "left\x1bPprivate\a[31mpayload\x1b\\right", want: "leftright"},
		{name: "Unicode and lines", input: "Ошибка 🧪\r\nsecond\tline\x00\b", want: "Ошибка 🧪\nsecond    line"},
		{name: "incomplete CSI", input: "visible\x1b[31", want: "visible"},
		{name: "CSI aborted by newline", input: "before\x1b[31\nafter", want: "before\nafter"},
		{name: "incomplete OSC", input: "visible\x1b]unfinished", want: "visible"},
		{name: "cancelled CSI", input: "before\x1b[31\x18after", want: "beforeafter"},
		{name: "lone escape", input: "visible\x1b", want: "visible"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeLogText(test.input); got != test.want {
				t.Fatalf("sanitizeLogText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestRuntimeWriterSanitizesRawByteC1AtRenderTime(t *testing.T) {
	runtime := NewRuntime()
	raw := []byte{0x9b, '3', '1', 'm', 'f', 'a', 'i', 'l', 'u', 'r', 'e', 0x9b, '0', 'm', '\n'}
	if _, err := runtime.Write(raw); err != nil {
		t.Fatal(err)
	}
	logs := runtime.Logs()
	if len(logs) != 1 || logs[0].Text != string(raw[:len(raw)-1]) {
		t.Fatalf("writer changed the captured source log: %#v", logs)
	}
	output := formatLogs(logs)
	if !strings.Contains(output, "failure") || strings.Contains(output, "31m") || strings.ContainsRune(output, utf8.RuneError) {
		t.Fatalf("rendered raw C1 formatting unsafely: %q", output)
	}
}

func TestFormatLogViewsSanitizeAtSharedRenderingBoundary(t *testing.T) {
	entries := make([]LogEntry, 21)
	for index := range entries {
		entries[index] = LogEntry{At: time.Unix(int64(index), 0), Text: fmt.Sprintf("ordinary-%02d", index)}
	}
	entries[0].Text = "\x1b[31mold failure\x1b[0m"
	entries[20].Text = "\x1b[2mnew failure\x1b[0m\n原因 🧪\x00"

	outputs := map[string]string{
		"ordinary": formatLogs(entries),
		"page":     formatLogPage(entries, 2),
		"range":    formatLogPages(entries, 1, 2),
		"search":   formatLogSearch(entries, "failure"),
	}
	for name, output := range outputs {
		if strings.ContainsRune(output, '\x1b') || strings.Contains(output, "[31m") || strings.Contains(output, "[2m") || strings.Contains(output, "[0m") || strings.ContainsRune(output, '\x00') {
			t.Errorf("%s output retained terminal formatting or controls: %q", name, output)
		}
	}
	if output := outputs["ordinary"]; !strings.Contains(output, "new failure\n  原因 🧪") {
		t.Fatalf("ordinary page lost readable multiline Unicode: %q", output)
	}
	if output := outputs["page"]; !strings.Contains(output, "old failure") {
		t.Fatalf("older page lost sanitized content: %q", output)
	}
	if output := outputs["range"]; !strings.Contains(output, "old failure") || !strings.Contains(output, "new failure") {
		t.Fatalf("page range lost sanitized entries: %q", output)
	}
	if output := outputs["search"]; !strings.Contains(output, "2 matches") || !strings.Contains(output, "new failure\n  原因 🧪") {
		t.Fatalf("search lost sanitized matches or line breaks: %q", output)
	}
	if entries[0].Text != "\x1b[31mold failure\x1b[0m" {
		t.Fatalf("rendering mutated the source entry: %q", entries[0].Text)
	}
}

func TestFormatLogPagesRendersRangeInNewestFirstPageOrder(t *testing.T) {
	entries := make([]LogEntry, 45)
	for index := range entries {
		entries[index].Text = fmt.Sprintf("entry-%02d", index+1)
	}

	output := formatLogPages(entries, 1, 2)
	if !strings.Contains(output, "45 entries. Pages 1-2 of 3, showing 40.") || strings.Contains(output, "entry-05") || strings.Count(output, "# Log") != 1 {
		t.Fatalf("range output = %s", output)
	}
	if strings.Index(output, "entry-45") > strings.Index(output, "entry-06") {
		t.Fatalf("older page appeared before newest page: %s", output)
	}
}
