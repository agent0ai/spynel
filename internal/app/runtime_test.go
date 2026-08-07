package app

import (
	"fmt"
	"strings"
	"testing"
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
