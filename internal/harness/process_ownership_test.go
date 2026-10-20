package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestProviderInputRenewsWriteDeadline(t *testing.T) {
	reader, file, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	input := &providerInput{file}
	defer input.Close()
	want := []byte("synthetic 世界\n")
	got := make([]byte, len(want)*2)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(reader, got); done <- err }()
	for range 2 {
		if err := file.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		if n, err := input.Write(want); err != nil || n != len(want) {
			t.Fatalf("new frame inherited an expired deadline: n=%d error=%v", n, err)
		}
	}
	if err := <-done; err != nil || !bytes.Equal(got, bytes.Repeat(want, 2)) {
		t.Fatalf("input changed complete UTF-8 frames: %q error=%v", got, err)
	}
}

func TestProviderInputDeadlineClosesPartialFrame(t *testing.T) {
	reader, file, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	input := &providerInput{file}
	defer input.Close()
	data := bytes.Repeat([]byte("x"), 2*1024*1024)
	started := time.Now()
	n, err := input.Write(data)
	if !errors.Is(err, os.ErrDeadlineExceeded) || n >= len(data) || time.Since(started) > 7*time.Second {
		t.Fatalf("blocked pipe was not bounded: n=%d error=%v elapsed=%s", n, err, time.Since(started))
	}
	if _, err := input.Write([]byte("another frame\n")); err == nil {
		t.Fatalf("failed input admitted another frame: %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data[:n]) {
		t.Fatalf("failed input did not end at its partial frame: length=%d error=%v", len(got), err)
	}
}

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
		t.Fatal("provider started without its admission guard")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ownership marker is not private")
	}
	// A contender must not release another live process's lock.
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

func TestProviderOwnershipUsesStableLockNotFileContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	owner := processOwnership{path: path}
	if err := owner.acquire(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	contender := processOwnership{path: path}
	if err := contender.acquire(); !errors.Is(err, errProcessOwnershipBusy) {
		t.Fatalf("file contents bypassed live lock: %v", err)
	}
	if err := owner.release(); err != nil {
		t.Fatal(err)
	}
	if err := contender.acquire(); err != nil {
		t.Fatalf("unused marker blocked recovery: %v", err)
	}
	defer contender.release()
	if data, err := os.ReadFile(path); err != nil || string(data) != "changed\n" {
		t.Fatalf("ownership changed the stable lock file: %v", err)
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

func TestUncertainFailedStartCannotReadmit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	registry := NewRegistry()
	registry.Register("uncertain", func(HarnessConfig) (Harness, error) {
		return &uncertainCloseHarness{supervisorHarness{startErr: errors.New("synthetic startup failure")}}, nil
	})
	registry.Register("healthy", func(HarnessConfig) (Harness, error) { return &supervisorHarness{}, nil })
	s := NewSupervisor(registry, HarnessConfig{Name: "uncertain"})
	admissions := 0
	s.SetProcessStartGuard(path, func() error { admissions++; return nil })
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("synthetic startup unexpectedly succeeded")
	}
	if err := s.Reconfigure(HarnessConfig{Name: "healthy"}); err == nil {
		t.Fatal("uncertain failed startup admitted a replacement")
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("uncertain failed startup was retried")
	}
	if admissions != 1 {
		t.Fatalf("provider admissions = %d, want 1", admissions)
	}
	if available, _ := s.Available(); available {
		t.Fatal("uncertain provider is available")
	}
	if err := s.Close(); err == nil {
		t.Fatal("uncertain close was accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain marker was cleared")
	}
}

func TestReplacementCloseFailureRefusesAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	registry := NewRegistry()
	registry.Register("uncertain", func(HarnessConfig) (Harness, error) { return &uncertainCloseHarness{}, nil })
	registry.Register("healthy", func(HarnessConfig) (Harness, error) { return &supervisorHarness{}, nil })
	s := NewSupervisor(registry, HarnessConfig{Name: "uncertain"})
	admissions := 0
	s.SetProcessStartGuard(path, func() error { admissions++; return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Reconfigure(HarnessConfig{Name: "healthy"}); err == nil {
			t.Fatal("unconfirmed provider close admitted a replacement")
		}
	}
	if admissions != 1 {
		t.Fatalf("provider admissions = %d, want 1", admissions)
	}
	if available, _ := s.Available(); available {
		t.Fatal("replacement is available after uncertain close")
	}
	if _, _, err := s.Send(context.Background(), "fixture", "synthetic effect", nil); err == nil {
		t.Fatal("dispatch remained available after uncertain close")
	}
	if s.HarnessConfig().Name != "uncertain" {
		t.Fatal("failed replacement changed the saved selection")
	}
	if err := s.Close(); err == nil {
		t.Fatal("uncertain close was accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain marker was cleared")
	}
}

func TestReplacementJoinsPreviousBeforeAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	registry := NewRegistry()
	previous := &supervisorHarness{}
	registry.Register("previous", func(HarnessConfig) (Harness, error) { return previous, nil })
	registry.Register("healthy", func(HarnessConfig) (Harness, error) { return &supervisorHarness{}, nil })
	s := NewSupervisor(registry, HarnessConfig{Name: "previous"})
	admissions := 0
	s.SetProcessStartGuard(path, func() error {
		admissions++
		if admissions > 1 && !previous.closed {
			t.Fatal("replacement admitted effects before prior close")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("replacement released ownership between providers")
		}
		return nil
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconfigure(HarnessConfig{Name: "healthy"}); err != nil {
		t.Fatal(err)
	}
	if admissions != 2 {
		t.Fatalf("provider admissions = %d, want 2", admissions)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	contender := processOwnership{path: path}
	if err := contender.acquire(); err != nil {
		t.Fatalf("orderly replacement retained ownership: %v", err)
	}
	defer contender.release()
}
