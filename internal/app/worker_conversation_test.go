package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationSettlementSurvivesControllerReplacement(t *testing.T) {
	s, _, cfg := newRecoveryTestService(t)
	metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "retained", ExecutionID: "execution", SourceMessageID: "second", Sources: []string{"first", "second"}}
	for _, source := range metadata.Sources {
		if _, err := s.History.Append("cli", "retained", history.Entry{Role: "user", Content: "synthetic", SourceMessageID: source, AcceptedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	open := func(service *Service) (int, Job) {
		t.Helper()
		id, err := service.Runtime.TryBeginJobWithDetails("chat:cli:retained", "cli", "retained", "synthetic", JobDetails{Kind: "conversation", WorkID: metadata.ExecutionID})
		if err != nil {
			t.Fatal(err)
		}
		job, _ := service.Runtime.Job(id)
		return id, job
	}
	id, original := open(s)
	delivery := harness.WorkerDelivery{WorkerID: strings.Repeat("a", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: 1, Conversation: metadata, Event: core.Event{Kind: core.EventError, Text: "synthetic failure", Done: true}, Streamed: "useful partial"}}
	if err := s.ConsumeConversationDelivery(id, delivery, func(harness.WorkerDelivery) error { return errors.New("delivery unavailable") }); err == nil {
		t.Fatal("failed downstream was acknowledged")
	}
	pending, _ := s.Runtime.Job(id)
	if s.Runtime.Status().Jobs != 1 || pending.Execution != JobDegraded {
		t.Fatal("failed downstream released its job")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = New(cfg, newServiceHarness())
	defer s.Close()
	id, restored := open(s)
	if restored.Number != original.Number || restored.Generation != original.Generation || restored.StableID != original.StableID || restored.ProviderIterations != 1 {
		t.Fatal("replacement changed job identity or counters")
	}
	delivered := 0
	if err := s.ConsumeConversationDelivery(id, delivery, func(actual harness.WorkerDelivery) error {
		if actual.ReceiptID() != delivery.ReceiptID() {
			t.Fatal("downstream lost stable delivery identity")
		}
		delivered++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := s.History.Entries("cli", "retained")
	if err != nil {
		t.Fatal(err)
	}
	partial, result := 0, 0
	for _, entry := range entries {
		if entry.Content == "useful partial" {
			partial++
		}
		if entry.Terminal {
			result++
			if entry.ExecutionID != "execution" || strings.Join(entry.Covers, ",") != "first,second" || entry.ReceiptID != delivery.ReceiptID()+":result" {
				t.Fatal("terminal lost exact coverage or receipt")
			}
		}
	}
	window, err := s.History.RecoveryWindow("cli", "retained", time.Time{}, 2000)
	if err != nil || window.Covered["first"] != "terminal_coverage" || window.Covered["second"] != "terminal_coverage" || partial != 1 || result != 1 || delivered != 1 || s.Runtime.Status().Jobs != 0 {
		t.Fatal("replayed settlement duplicated or lost an effect", err)
	}
	_, output, err := s.Runtime.ArchivedJob(original.Number)
	if err != nil || strings.Count(output, "synthetic failure") != 1 || strings.Contains(output, "worker_delivery") || strings.Contains(output, "useful partial") {
		t.Fatal("archive replay or private projection failed", err)
	}
	info := ""
	if err := s.archivedJobInfoCommand(core.Message{Channel: "cli", Conversation: "inspection"}, original.Number, func(event core.Event) { info += event.Text }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info, "Work ID:") || strings.Contains(info, delivery.WorkerID) || strings.Contains(info, "source") {
		t.Fatal("private conversation correlation leaked through job inspection")
	}
}

func TestWorkerConversationDeliveryFailsClosedAndRetriesStorage(t *testing.T) {
	s, target, _ := newRecoveryTestService(t)
	metadata := &harness.WorkerConversation{Channel: "cli", Conversation: "storage", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
	if _, err := s.History.Append("cli", "storage", history.Entry{Role: "user", SourceMessageID: "source"}); err != nil {
		t.Fatal(err)
	}
	key := "chat:cli:storage"
	id, err := s.Runtime.TryBeginJobWithDetails(key, "cli", "storage", "synthetic", JobDetails{Kind: "conversation", WorkID: "execution"})
	if err != nil {
		t.Fatal(err)
	}
	// A worker callback must use its captured state, never controller activity.
	target.active[key] = true
	delivery := harness.WorkerDelivery{WorkerID: strings.Repeat("b", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: 1, Conversation: metadata, Event: core.Event{Kind: core.EventFinal, Text: "one result", Done: true}}}
	job, _ := s.Runtime.Job(id)
	archive := filepath.Join(s.Config.StatePath("jobs"), job.StableID+".md")
	if err := os.Rename(archive, archive+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeConversationDelivery(id, delivery, nil); err == nil || s.Runtime.Status().Jobs != 1 {
		t.Fatal("failed archive write settled delivery")
	}
	if pending, _ := s.Runtime.Job(id); pending.Execution != JobDegraded {
		t.Fatal("failed archive delivery was not visibly degraded")
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(archive+".saved", archive); err != nil {
		t.Fatal(err)
	}
	changed := *metadata
	changed.Conversation = "other-thread"
	invalid := delivery
	invalid.Conversation = &changed
	if s.ConsumeConversationDelivery(id, invalid, nil) == nil {
		t.Fatal("unknown cross-thread source was accepted")
	}
	if s.ConsumeConversationDelivery(id+1, delivery, nil) == nil {
		t.Fatal("wrong job was accepted")
	}
	if err := s.ConsumeConversationDelivery(id, delivery, nil); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := s.History.Entries("cli", "storage")
	if len(entries) != 2 || len(entries[1].Covers) != 1 || entries[1].ExecutionID != "execution" {
		t.Fatal("retry lost coverage or duplicated history")
	}
}

func TestWorkerConversationFinalPreservesOutputOnProcessingError(t *testing.T) {
	for _, failure := range []string{"after-hook", "outbound-final-item"} {
		t.Run(failure, func(t *testing.T) {
			s, _, _ := newRecoveryTestService(t)
			metadata := &harness.WorkerConversation{Channel: "telegram", Conversation: "TG-7", ExecutionID: "execution", SourceMessageID: "source", Sources: []string{"source"}}
			if _, err := s.History.Append(metadata.Channel, metadata.Conversation, history.Entry{Role: "user", SourceMessageID: "source"}); err != nil {
				t.Fatal(err)
			}
			id, err := s.Runtime.TryBeginJobWithDetails("chat:telegram:TG-7", metadata.Channel, metadata.Conversation, "synthetic", JobDetails{Kind: "conversation", WorkID: metadata.ExecutionID})
			if err != nil {
				t.Fatal(err)
			}
			finalItem := "[Send attachment](<missing>)"
			delivery := harness.WorkerDelivery{WorkerID: strings.Repeat("c", 32), Sequence: 1, WorkerEvent: harness.WorkerEvent{Cursor: 1, Conversation: metadata, Streamed: "useful partial", Event: core.Event{Kind: core.EventFinal, Text: "useful partial", FinalText: &finalItem, Done: true}}}
			if failure == "after-hook" {
				s.Config.Extensions.Enabled = true
				s.Hooks.Directory = filepath.Join(t.TempDir(), "unreadable-extensions")
				if err := os.WriteFile(s.Hooks.Directory, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ConsumeConversationDelivery(id, delivery, nil); err != nil {
				t.Fatal(err)
			}
			entries, _, err := s.History.Entries(metadata.Channel, metadata.Conversation)
			if err != nil || len(entries) != 3 || entries[1].Role != "assistant" || entries[1].Content != "useful partial" || entries[1].ReceiptID != delivery.ReceiptID()+":partial" || entries[2].Role != "error" || !entries[2].Terminal {
				t.Fatal("application processing error lost output or its stable receipt", err)
			}
		})
	}
}
