package orchestrator

import (
	"context"
	"testing"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
)

func TestScheduledOutboxPreservesScanEligibility(t *testing.T) {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	manager := New(cfg, newFakeRecipient(), extensions.Runner{})
	delivered := 0
	manager.Outbox.Deliver = func(context.Context, Origin, string, string) error {
		if !manager.scanMu.TryLock() {
			t.Fatal("outbox delivery retained the workflow scan lock")
		}
		manager.scanMu.Unlock()
		delivered++
		return nil
	}
	if _, err := manager.Outbox.Enqueue("synthetic-delivery", "manual", "tui/synthetic", "synthetic notice"); err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.Enabled = false
	manager.ApplyRuntimeConfig(cfg)
	if err := manager.ScanOnce(t.Context()); err != nil || delivered != 0 {
		t.Fatal("disabled scan delivered the outbox", err)
	}
	obsolete := manager.scanTimerGeneration
	cfg.Orchestrator.Enabled = true
	cfg.Orchestrator.IntervalSec++
	manager.ApplyRuntimeConfig(cfg)
	if err := manager.runScan(t.Context(), &obsolete); err != nil || delivered != 0 {
		t.Fatal("obsolete timer generation delivered the outbox", err)
	}
	if err := manager.ScanOnce(t.Context()); err != nil || delivered != 1 {
		t.Fatal("current scan did not deliver the outbox", err)
	}
}
