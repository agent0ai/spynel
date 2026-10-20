package harness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestSupervisorCancelledSessionOperationDoesNotReachProvider(t *testing.T) {
	for _, scenario := range []struct {
		action string
		mode   FollowUpMode
	}{
		{"send", FollowUpSteer}, {"control", FollowUpSteer}, {"interrupt", FollowUpSteer},
		{"send", FollowUpQueue}, {"control", FollowUpQueue},
	} {
		action := scenario.action
		for _, contended := range []bool{false, true} {
			t.Run(action+"/"+string(scenario.mode)+map[bool]string{false: "/already-cancelled", true: "/contended-deadline"}[contended], func(t *testing.T) {
				target := &supervisorHarness{name: "codex", followUp: scenario.mode, active: map[string]bool{}, emits: map[string]core.Emit{}}
				registry := NewRegistry()
				registry.Register("codex", func(HarnessConfig) (Harness, error) { return target, nil })
				supervisor := NewSupervisor(registry, HarnessConfig{Name: "codex"})
				if err := supervisor.Start(t.Context()); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = supervisor.Close() })
				if _, _, err := supervisor.Send(t.Context(), "job", "original", nil); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				release := func() {}
				if contended {
					operation := supervisor.controlOperation("job")
					operation.Lock()
					release = sync.OnceFunc(operation.Unlock)
					defer release()
				} else {
					cancel()
				}
				reservations := 0
				request := ControlRequest{ID: "cancelled-control", Prompt: "guidance", ReserveProviderTurn: func() bool { reservations++; return true }}
				result := make(chan error, 1)
				go func() {
					var err error
					switch action {
					case "send":
						_, _, err = supervisor.SendConversation(ctx, "job", "follow-up", "follow-up", nil)
					case "control":
						_, err = supervisor.SendControl(ctx, "job", request)
					case "interrupt":
						_, err = supervisor.Interrupt(ctx, "job")
					}
					result <- err
				}()
				select {
				case err := <-result:
					if !errors.Is(err, ctx.Err()) || err == nil {
						t.Errorf("cancelled %s returned %v, want %v", action, err, ctx.Err())
					}
				case <-time.After(time.Second):
					t.Error("session operation ignored cancellation while waiting for admission")
					release()
					<-result
				}
				release()
				if reservations != 0 || !supervisor.IsActive("job") || !target.IsActive("job") {
					t.Fatal("cancelled waiter changed the original provider execution")
				}
				supervisor.mu.RLock()
				changed := supervisor.controlGeneration["job"] != 0 || len(supervisor.pending["job"]) != 0 || len(supervisor.seenControl["job"]) != 0 || supervisor.controls["job"] != nil
				supervisor.mu.RUnlock()
				target.mu.Lock()
				prompts := len(target.prompts["job"])
				target.mu.Unlock()
				if changed || prompts != 1 {
					t.Fatal("cancelled waiter left admission, queue, control or provider effects")
				}
				if action == "control" {
					admitted, err := supervisor.SendControl(t.Context(), "job", request)
					wantReservations := 1
					if scenario.mode == FollowUpQueue {
						wantReservations = 0
					}
					if err != nil || admitted.Duplicate || admitted.Queued != (scenario.mode == FollowUpQueue) || reservations != wantReservations {
						t.Fatalf("cancelled waiter consumed retry authority: %+v, %v, reservations=%d", admitted, err, reservations)
					}
				}
			})
		}
	}
}

func TestSupervisorNativeSessionWaitCancellationPreservesTurn(t *testing.T) {
	t.Setenv("SPYNEL_TEST_CODEX_STALL_CONTROL", "1")
	command, root, logPath := portableHarnessFixture(t, "codex-worker")
	supervisor := NewSupervisor(NewBuiltinRegistry(), HarnessConfig{Name: "codex", Command: command, Cwd: root, Sandbox: "read-only", ApprovalPolicy: "never"})
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := supervisor.Close(); err != nil {
			t.Error("native provider cleanup:", err)
		}
	})
	events := make(chan core.Event, 1024)
	thread, _, err := supervisor.SendConversation(t.Context(), "job", "original", "original", func(event core.Event) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	target := supervisor.current.(*Codex)
	holderCtx, releaseHolder := context.WithCancel(t.Context())
	holderDone := make(chan struct{})
	var holderErr error
	request := ControlRequest{ID: "native-stalled-control", Prompt: "guidance"}
	go func() {
		_, holderErr = supervisor.SendControl(holderCtx, "job", request)
		close(holderDone)
	}()
	t.Cleanup(func() { releaseHolder(); <-holderDone })
	deadline := time.After(3 * time.Second)
	for {
		seen := false
		for _, record := range readFixtureRecords(t, logPath) {
			seen = seen || record.Method == "turn/steer"
		}
		if seen {
			break
		}
		select {
		case <-deadline:
			t.Fatal("native control did not hold session admission")
		case <-time.After(10 * time.Millisecond):
		}
	}
	for _, action := range []string{"send", "control", "interrupt"} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		result := make(chan error, 1)
		go func() {
			var err error
			switch action {
			case "send":
				_, _, err = supervisor.SendConversation(ctx, "job", "expired", "expired", nil)
			case "control":
				_, err = supervisor.SendControl(ctx, "job", ControlRequest{ID: "native-expired-control", Prompt: "expired"})
			case "interrupt":
				_, err = supervisor.Interrupt(ctx, "job")
			}
			result <- err
		}()
		select {
		case err := <-result:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("native %s waiter returned %v", action, err)
			}
		case <-time.After(time.Second):
			t.Error("native session waiter ignored its deadline")
			releaseHolder()
			<-result
		}
		cancel()
	}
	if !supervisor.IsActive("job") || !target.IsActive("job") || supervisor.ThreadID("job") != thread {
		t.Fatal("cancelled waiters ended or replaced the original turn")
	}
progress:
	for {
		select {
		case event := <-events:
			if event.Done {
				t.Fatal("cancelled waiter released the original emitter")
			}
			if event.Kind == core.EventDelta {
				break progress
			}
		case <-time.After(time.Second):
			t.Fatal("native provider stopped publishing progress")
		}
	}
	releaseHolder()
	select {
	case <-holderDone:
		if !errors.Is(holderErr, context.Canceled) {
			t.Fatal("stalled native control did not detach honestly:", holderErr)
		}
	case <-time.After(time.Second):
		t.Fatal("native control did not release admission")
	}
	if retry, err := supervisor.SendControl(t.Context(), "job", request); err != nil || !retry.Duplicate {
		t.Fatal("uncertain admitted control lost deduplication:", retry, err)
	}
	starts, steers, interrupts, processes := 0, 0, 0, 0
	for _, record := range readFixtureRecords(t, logPath) {
		switch record.Method {
		case "turn/start":
			starts++
		case "turn/steer":
			steers++
		case "turn/interrupt":
			interrupts++
		}
		if record.Kind == "invocation" {
			processes++
			t.Logf("retained native synthetic provider pid=%d", record.PID)
		}
	}
	if starts != 1 || steers != 1 || interrupts != 0 || processes != 1 {
		t.Fatalf("cancelled waiters reached provider: starts=%d steers=%d interrupts=%d processes=%d", starts, steers, interrupts, processes)
	}
	if stopped, err := supervisor.Interrupt(t.Context(), "job"); err != nil || !stopped {
		t.Fatal("explicit cancellation after admission release failed:", stopped, err)
	}
}
