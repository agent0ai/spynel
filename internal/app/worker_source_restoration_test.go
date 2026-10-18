package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationRestorationProofAndBatch(t *testing.T) {
	s, _, cfg := newRecoveryTestService(t)
	c := harness.WorkerConversation{Channel: "cli", Conversation: "owned", SourceMessageID: "alpha", ExecutionID: "execution", Sources: []string{"alpha", "beta"}}
	for _, source := range []string{"original", "alpha", "beta"} {
		if _, err := s.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: source, Content: "synthetic"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.History.AdmitConversation(c.Channel, c.Conversation, c.ExecutionID, "older-admission", []string{"original"}, false); err != nil {
		t.Fatal(err)
	}
	key, receipt := "chat:cli:owned", "worker:2:admission"
	if err := ApplyWorkerConversation(cfg.Root, key, c, receipt, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.History.Path(c.Channel, c.Conversation))
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyWorkerConversation(cfg.Root, key, c, receipt, errors.New("uncertain acknowledgement")); err == nil {
		t.Fatal("uncertain delivery restored sources")
	}
	after, err := os.ReadFile(s.History.Path(c.Channel, c.Conversation))
	if err != nil || string(before) != string(after) {
		t.Fatal("refused restoration changed history", err)
	}
	proof := fmt.Errorf("wrapped: %w", &harness.SendNotAdmittedError{Err: errors.New("synthetic rejection")})
	if err := ApplyWorkerConversation(cfg.Root, key, c, receipt, proof); err != nil {
		t.Fatal(err)
	}
	window, err := s.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
	if err != nil || !window.Admitted["original"] || window.Admitted["alpha"] || window.Admitted["beta"] || len(window.Covered) != 0 {
		t.Fatal("restoration changed older work or retained rejected source admission", err)
	}
	// The old rejection cannot clear a later admission, even after Store reopen.
	if err := s.History.AdmitConversation(c.Channel, c.Conversation, "next", "next-admission", c.Sources, false); err != nil {
		t.Fatal(err)
	}
	if err := ApplyWorkerConversation(cfg.Root, key, c, receipt, proof); err != nil {
		t.Fatal(err)
	}
	window, err = s.History.RecoveryWindow(c.Channel, c.Conversation, time.Time{}, 20)
	if err != nil || !window.Admitted["original"] || !window.Admitted["alpha"] || !window.Admitted["beta"] {
		t.Fatal("old restoration cleared later work", err)
	}
}

func TestWorkerConversationRestorationRefusalAndPrivacy(t *testing.T) {
	for _, scenario := range []string{"recovery", "changed-receipt", "unavailable-storage"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, cfg := newRecoveryTestService(t)
			c := harness.WorkerConversation{Channel: "cli", Conversation: "owned", SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
			if _, err := s.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			key, receipt := "chat:cli:owned", "worker:1:admission"
			if err := ApplyWorkerConversation(cfg.Root, key, c, receipt, nil); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "recovery":
				c.Recovery = true
			case "changed-receipt":
				receipt = "different-admission"
			case "unavailable-storage":
				path := s.History.Path(c.Channel, c.Conversation)
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			proof := &harness.SendNotAdmittedError{Err: errors.New("private-provider-detail")}
			err := ApplyWorkerConversation(cfg.Root, key, c, receipt, proof)
			if err == nil || strings.Contains(err.Error(), cfg.Root) || strings.Contains(err.Error(), receipt) || strings.Contains(err.Error(), "private-provider-detail") {
				t.Fatal("restoration accepted invalid state or exposed private storage/provider details")
			}
		})
	}
}
