package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
)

func TestConcurrentMessageIdentityDispatchesOnce(t *testing.T) {
	s, target, _ := newRecoveryTestService(t)
	message := core.Message{Channel: "cli", Conversation: "retry", Text: "hello", SourceMessageID: "retry-1"}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errorsSeen <- s.Handle(context.Background(), message, nil) }()
	}
	wg.Wait()
	close(errorsSeen)
	accepted := 0
	for err := range errorsSeen {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrDuplicateMessage) {
			t.Fatal(err)
		}
	}
	if accepted != 1 || len(target.prompts["chat:cli:retry"]) != 1 {
		t.Fatalf("admissions=%d provider sends=%d", accepted, len(target.prompts["chat:cli:retry"]))
	}
	for i := 0; i < 64; i++ {
		msg := message
		msg.SourceMessageID = string(rune('A' + i))
		if _, err := s.admitMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	message.SourceMessageID = "overflow"
	if _, err := s.admitMessage(message); !errors.Is(err, ErrMessageAdmissionBusy) {
		t.Fatalf("bound: %v", err)
	}
}

func TestLocalMessageValidationAndOperationalLogPrivacy(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", "a b", "name_", strings.Repeat("a", 121)} {
		if ValidateConversationName(name) == nil {
			t.Fatalf("unsafe name %q", name)
		}
	}
	message := core.Message{Channel: "telegram", Conversation: "adapter", Text: "hello"}
	if ValidateLocalMessage(message) == nil {
		t.Fatal("local API allowed remote impersonation")
	}
	message.Channel = "cli"
	message.Text = string([]byte{0xff})
	if ValidateLocalMessage(message) == nil {
		t.Fatal("invalid UTF-8 admitted")
	}
	runtime := NewRuntime()
	var output bytes.Buffer
	runtime.SetOperationalOutput(&output)
	runtime.LogEvent("info", "harness", "turn_started", "private prompt and source identity; token=secret")
	runtime.LogEvent("error", "harness", "start_failed", "sensitive provider payload")
	runtime.LogEvent("warning", "tui", "private-diagnostic-id", "private diagnostic text")
	if !strings.Contains(output.String(), "harness turn_started") || !strings.Contains(output.String(), "harness start_failed") || strings.Contains(output.String(), "private") || strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "payload") {
		t.Fatalf("headless projection: %q", output.String())
	}
	before := output.Len()
	runtime.SetOperationalOutput(nil)
	runtime.LogEvent("info", "runtime", "primary_started", "TUI")
	if output.Len() != before {
		t.Fatal("TUI output was corrupted")
	}
}
