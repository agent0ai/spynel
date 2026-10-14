package harness

import (
	"context"
	"errors"
	"github.com/agent0ai/spynel/internal/core"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderOwnershipFencesAnotherContenderUntilOrderlyClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "harness-owner")
	registry := NewRegistry()
	starts := 0
	registry.Register("fixture", func(HarnessConfig) (Harness, error) {
		starts++
		return &supervisorHarness{active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
	})
	first := NewSupervisor(registry, HarnessConfig{Name: "fixture"})
	intents := 0
	first.SetProcessStartGuard(path, func() error { intents++; return nil })
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if intents != 1 {
		t.Fatal("provider started without recovery suppression")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ownership marker is not private")
	}
	// The contender has no local execution state from the prior process. Neither
	// stale election nor dead owner evidence authorizes deleting this marker.
	second := NewSupervisor(registry, HarnessConfig{Name: "fixture"})
	second.SetProcessStartGuard(path, func() error { t.Fatal("blocked contender reached native admission"); return nil })
	if err := second.Start(context.Background()); err == nil {
		t.Fatal("orphan ownership was bypassed")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("contender cleared prior ownership")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third := NewSupervisor(registry, HarnessConfig{Name: "fixture"})
	third.SetProcessStartGuard(path, nil)
	if err := third.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	if starts != 3 {
		t.Fatal("unexpected factory calls")
	}
}

func TestProviderGuardFailurePreventsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	registry := NewRegistry()
	target := &supervisorHarness{startErr: errors.New("provider Start must not run")}
	registry.Register("fixture", func(HarnessConfig) (Harness, error) { return target, nil })
	s := NewSupervisor(registry, HarnessConfig{Name: "fixture"})
	s.SetProcessStartGuard(path, func() error { return errors.New("parent unavailable") })
	if err := s.Start(context.Background()); err == nil || err.Error() != "parent unavailable" {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderOwnershipNeverClearsChangedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	owner := processOwnership{path: path}
	if err := owner.acquire(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.release(); err == nil {
		t.Fatal("changed owner was cleared")
	}
}

type uncertainCloseHarness struct{ supervisorHarness }

func (*uncertainCloseHarness) Close() error { return errors.New("synthetic provider join failure") }

func TestUncertainProviderCloseRetainsOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	registry := NewRegistry()
	registry.Register("fixture", func(HarnessConfig) (Harness, error) { return &uncertainCloseHarness{}, nil })
	s := NewSupervisor(registry, HarnessConfig{Name: "fixture"})
	s.SetProcessStartGuard(path, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err == nil {
		t.Fatal("uncertain shutdown was accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain ownership was cleared")
	}
	contender := processOwnership{path: path}
	if err := contender.acquire(); err == nil {
		t.Fatal("uncertain effects admitted another owner")
	}
}
