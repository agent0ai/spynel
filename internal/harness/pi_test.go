package harness

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestPiRPCStreamsSettlesAndResumes(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	sessionsPath := filepath.Join(root, "sessions.json")
	config := HarnessConfig{
		Command: command, Cwd: root, Model: "fixture/model-a", Effort: "high",
		Sandbox: "read-only", SessionsFile: sessionsPath,
	}
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []core.Event
	deltaSeen := make(chan struct{}, 1)
	done := make(chan core.Event, 1)
	threadID, steered, err := pi.Send(ctx, "chat", "test prompt", func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Kind == core.EventDelta && strings.Contains(event.Text, "world") {
			select {
			case deltaSeen <- struct{}{}:
			default:
			}
		}
		if event.Done {
			done <- event
		}
	})
	if err != nil || steered || threadID != "pi-session" {
		t.Fatalf("Send() = %q, %t, %v", threadID, steered, err)
	}
	select {
	case <-deltaSeen:
	case <-ctx.Done():
		t.Fatal("timed out waiting for Pi delta")
	}
	select {
	case event := <-done:
		t.Fatalf("Pi treated agent_end as terminal before agent_settled: %#v", event)
	case <-time.After(30 * time.Millisecond):
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for Pi agent_settled")
	}
	if final.Kind != core.EventFinal || final.Text != "hello world" || final.FinalText == nil || *final.FinalText != "hello world" || pi.IsActive("chat") {
		t.Fatalf("Pi final = %#v, active %t", final, pi.IsActive("chat"))
	}
	models, err := pi.Models(ctx)
	if err != nil || len(models) != 1 || models[0].ID != "fixture/model-a" || models[0].DefaultEffort != "medium" {
		t.Fatalf("Pi models = %#v, %v", models, err)
	}
	if err := pi.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPi(config)
	if err != nil || restarted.ThreadID("chat") != "pi-session" {
		t.Fatalf("persisted Pi session = %q, %v", restarted.ThreadID("chat"), err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	restartedDone := make(chan struct{}, 1)
	if threadID, steered, err := restarted.Send(ctx, "chat", "continued", func(event core.Event) {
		if event.Done {
			restartedDone <- struct{}{}
		}
	}); err != nil || steered || threadID != "pi-session" {
		t.Fatalf("resumed Pi Send() = %q, %t, %v", threadID, steered, err)
	}
	select {
	case <-restartedDone:
	case <-ctx.Done():
		t.Fatal("timed out waiting for resumed Pi turn")
	}
	_ = restarted.Close()

	var rpcInvocations, resumed int
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "invocation" || containsArgument(record.Args, "--version") {
			continue
		}
		rpcInvocations++
		arguments := strings.Join(record.Args, " ")
		if !strings.Contains(arguments, "--mode rpc") || record.Cwd != root || record.Executable != command {
			t.Fatalf("portable Pi invocation = %#v", record)
		}
		if containsArgument(record.Args, "--session") {
			resumed++
		}
	}
	if rpcInvocations != 3 || resumed != 1 {
		t.Fatalf("Pi RPC invocations = %d, resumed = %d", rpcInvocations, resumed)
	}
}

func TestPiAuthoritativeMessageAddsOnlyMissingStreamSuffix(t *testing.T) {
	turn := &piTurn{}
	turn.appendText("session", "partial ")
	turn.finishMessage("session", "partial response")
	if got := turn.text.String(); got != "partial response" {
		t.Fatalf("reconciled Pi message = %q", got)
	}
	if turn.lastMessage != "partial response" {
		t.Fatalf("Pi final assistant item = %q", turn.lastMessage)
	}
}

func TestPiSteersActiveTurnAndReleasesPreviousEmitter(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-steer")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	first := make(chan core.Event, 16)
	if _, steered, err := pi.Send(ctx, "chat", "first", func(event core.Event) { first <- event }); err != nil || steered {
		t.Fatalf("first Pi send = steered %t, %v", steered, err)
	}
	for {
		select {
		case event := <-first:
			if event.Kind == core.EventDelta && event.Text == "first" {
				goto firstSeen
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for first Pi delta")
		}
	}

firstSeen:
	second := make(chan core.Event, 16)
	if threadID, steered, err := pi.Send(ctx, "chat", "second", func(event core.Event) { second <- event }); err != nil || !steered || threadID != "pi-session" {
		t.Fatalf("steered Pi send = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	for !final.Done {
		select {
		case event := <-second:
			if event.Done {
				final = event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for steered Pi result")
		}
	}
	if final.Kind != core.EventFinal || final.Text != "first second" {
		t.Fatalf("steered Pi final = %#v", final)
	}
	released := false
	for len(first) > 0 {
		event := <-first
		released = released || event.Kind == core.EventStatus && event.Done
	}
	if !released {
		t.Fatal("previous Pi emitter was not released")
	}
}

func TestPiInterruptUsesRPCAbort(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-interrupt")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	events := make(chan core.Event, 16)
	if _, _, err := pi.Send(ctx, "chat", "work", func(event core.Event) { events <- event }); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event.Kind == core.EventDelta {
				goto active
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for active Pi turn")
		}
	}

active:
	if stopped, err := pi.Interrupt(ctx, "chat"); err != nil || !stopped {
		t.Fatalf("Pi interrupt = %t, %v", stopped, err)
	}
	for {
		select {
		case event := <-events:
			if event.Done {
				if event.Kind != core.EventFinal {
					t.Fatalf("Pi interrupt terminal = %#v", event)
				}
				goto done
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for interrupted Pi turn")
		}
	}

done:
	foundAbort := false
	for _, record := range readFixtureRecords(t, logPath) {
		foundAbort = foundAbort || record.Method == "abort"
	}
	if !foundAbort {
		t.Fatal("Pi fixture did not receive abort")
	}
}
