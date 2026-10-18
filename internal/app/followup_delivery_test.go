package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

// The provider applied the guidance but retained the original response owner
// when its acknowledgement failed.
type uncertainFollowupHarness struct {
	*heldServiceHarness
	effects int
}

func (*uncertainFollowupHarness) FollowUpMode() harness.FollowUpMode { return harness.FollowUpSteer }

func (h *uncertainFollowupHarness) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	if h.IsActive(key) {
		h.effects++
		return h.ThreadID(key), true, errors.New("acknowledgement unavailable after effect")
	}
	return h.heldServiceHarness.SendWithModel(ctx, key, prompt, model, emit)
}

func TestUnknownFollowupDeliveryKeepsExactSourceCoverage(t *testing.T) {
	service, _, _ := newRecoveryTestService(t)
	target := &uncertainFollowupHarness{heldServiceHarness: newHeldServiceHarness()}
	registry := harness.NewRegistry()
	registry.Register("fixture", func(harness.HarnessConfig) (harness.Harness, error) { return target, nil })
	supervisor := harness.NewSupervisor(registry, harness.HarnessConfig{Name: "fixture"})
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	service.Harness = supervisor
	first := core.Message{Channel: "cli", Conversation: "unknown-followup", SourceMessageID: "first", Text: "first"}
	second := core.Message{Channel: first.Channel, Conversation: first.Conversation, SourceMessageID: "second", Text: "guidance"}
	if err := service.Handle(t.Context(), first, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(t.Context(), second, nil); err == nil {
		t.Fatal("unknown follow-up acknowledgement was reported as success")
	}
	target.finish(sessionKey(first))
	snapshot, err := service.History.RecoveryWindow(first.Channel, first.Conversation, time.Time{}, 20)
	if err != nil || snapshot.Covered[first.SourceMessageID] != "terminal_coverage" || snapshot.Covered[second.SourceMessageID] != "terminal_coverage" {
		t.Fatalf("applied follow-up escaped the original terminal coverage: %v, %v", snapshot.Covered, err)
	}
	status := service.scanRecovery(t.Context(), "test")
	target.mu.Lock()
	starts := len(target.prompts[sessionKey(first)])
	target.mu.Unlock()
	if status.ScannedAt.IsZero() || status.Conversations != 1 || status.FailedClosed != 0 || status.Dispatched != 0 || starts != 1 || target.effects != 1 {
		t.Fatalf("applied guidance was recovered again: dispatched=%d starts=%d effects=%d", status.Dispatched, starts, target.effects)
	}
	entries, _, err := service.History.RecoveryEntries(first.Channel, first.Conversation, 20)
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for _, entry := range entries {
		if entry.Role == "assistant" && entry.Outcome == "terminal_assistant" {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("original provider result count = %d, want one", results)
	}
}

type uncertainStartHarness struct {
	*serviceHarness
	effects int
}

func (h *uncertainStartHarness) Send(context.Context, string, string, core.Emit) (string, bool, error) {
	h.effects++
	return "", false, errors.New("acknowledgement unavailable after effect")
}

func TestUnknownInitialDeliveryRemainsPending(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "recovery"}[recovery], func(t *testing.T) {
			service, target, _ := newRecoveryTestService(t)
			unknown := &uncertainStartHarness{serviceHarness: target}
			service.Harness = unknown
			message := core.Message{Channel: "cli", Conversation: "uncertain-start", SourceMessageID: "source", Text: "synthetic"}
			if recovery {
				now := time.Now().UTC()
				entry := history.Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: message.SourceMessageID, Content: message.Text}
				if _, err := service.History.Append(message.Channel, message.Conversation, entry); err != nil {
					t.Fatal(err)
				}
				if service.dispatchRecovery(t.Context(), orchestrator.Origin{Channel: message.Channel, Conversation: message.Conversation}, []recoveryCandidate{{entry: entry}}) {
					t.Fatal("unknown recovery counted as confirmed dispatch")
				}
			} else if err := service.Handle(t.Context(), message, nil); !errors.Is(err, orchestrator.ErrConversationPending) {
				t.Fatal("unknown delivery was not reported as pending", err)
			}
			if !service.conversationInFlight(sessionKey(message)) {
				t.Fatal("unknown delivery released its source reservation")
			}
			job, ok := service.Runtime.JobForSession(sessionKey(message))
			if !ok || job.Execution != JobDegraded {
				t.Fatal("unknown inactive delivery fabricated job completion", job)
			}
			entries, _, err := service.History.RecoveryEntries(message.Channel, message.Conversation, 20)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Role == "assistant" || entry.Role == "error" || len(entry.Covers) != 0 {
					t.Fatal("unknown delivery fabricated a terminal result")
				}
			}
			status := service.scanRecovery(t.Context(), "test")
			if status.Conversations != 1 || status.Dispatched != 0 || unknown.effects != 1 {
				t.Fatal("unknown delivery redispatched", status, unknown.effects)
			}
		})
	}
}
