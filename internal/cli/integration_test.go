package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/workspace"
)

type terminalCLIHarness struct{ *heldCLIHarness }

func (h terminalCLIHarness) Interrupt(ctx context.Context, key string) (bool, error) {
	h.mu.Lock()
	emit := h.emits[key]
	h.mu.Unlock()
	ok, err := h.heldCLIHarness.Interrupt(ctx, key)
	if ok {
		emit(core.Event{Kind: core.EventError, Text: "synthetic interruption", Done: true})
	}
	return ok, err
}

func TestCLIIntegrationFollowupCancellationAndProtocolOutput(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Extensions.Enabled = false
	cfg.Orchestrator.Enabled = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	target := terminalCLIHarness{newHeldCLIHarness()}
	service := app.New(cfg, target)
	owner, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	token, _ := owner.NewToken()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := owner.TryAcquire(listener.Addr().String(), token); err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	service.SetPrimaryInstanceID(owner.ID())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&localapi.Server{Service: service, Token: token}).Serve(ctx, listener) }()
	defer func() { cancel(); <-done; _ = service.Close(); _ = owner.Release(token) }()
	var first, follow bytes.Buffer
	firstDone, followDone := make(chan error, 1), make(chan error, 1)
	go func() {
		firstDone <- runMessageMode(cfg.Path, "adapter", "first", "test", messageRunOptions{JSON: true, RequestID: "first-1", Output: &first})
	}()
	waitCLISends(t, target.heldCLIHarness, 1)
	go func() {
		followDone <- runMessageMode(cfg.Path, "adapter", "followup", "test", messageRunOptions{JSON: true, FollowupOnly: true, RequestID: "follow-1", Output: &follow})
	}()
	waitCLISends(t, target.heldCLIHarness, 2)
	if err := waitCLIResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	target.finish("chat:cli:adapter", "followup result")
	if err := waitCLIResult(t, followDone); err != nil {
		t.Fatal(err)
	}
	for _, output := range []*bytes.Buffer{&first, &follow} {
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		for decoder.More() {
			var e core.Event
			if err := decoder.Decode(&e); err != nil || e.RequestID == "" {
				t.Fatalf("NDJSON correlation: %#v %v", e, err)
			}
		}
	}
	if !strings.Contains(follow.String(), "followup result") {
		t.Fatal("followup terminal missing")
	}
	var retry bytes.Buffer
	if err := runMessageMode(cfg.Path, "adapter", "first", "test", messageRunOptions{JSON: true, RequestID: "first-1", Output: &retry}); err == nil || !strings.Contains(retry.String(), "duplicate") {
		t.Fatalf("retry output: %q %v", retry.String(), err)
	}
	var stopped bytes.Buffer
	go func() {
		firstDone <- runMessageMode(cfg.Path, "adapter", "cancel this", "test", messageRunOptions{JSON: true, RequestID: "cancel-1", Output: &stopped})
	}()
	waitCLISends(t, target.heldCLIHarness, 3)
	var command bytes.Buffer
	if err := runFrameworkMessageMode(cfg.Path, "adapter", "/stop", "test", messageRunOptions{Output: &command}); err != nil {
		t.Fatal(err)
	}
	if err := waitCLIResult(t, firstDone); err == nil || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("JSON failure leaked response into stderr: %v", err)
	}
	if !strings.Contains(stopped.String(), "synthetic interruption") {
		t.Fatal("terminal error missing from protocol output")
	}
	if err := runMessageMode(cfg.Path, "adapter", "too late", "test", messageRunOptions{FollowupOnly: true, Output: &command}); err == nil {
		t.Fatal("inactive followup admitted")
	}
	lease, err := owner.Current()
	if err != nil || lease.InstanceID != owner.ID() {
		t.Fatal("CLI displaced the primary")
	}
}

func waitCLISends(t *testing.T, target *heldCLIHarness, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		target.mu.Lock()
		n := len(target.prompts["chat:cli:adapter"])
		target.mu.Unlock()
		if n >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("CLI dispatch timeout")
}

func waitCLIResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("CLI result timeout")
		return nil
	}
}

func TestJSONContinuationWaitsForActualTerminal(t *testing.T) {
	var output bytes.Buffer
	err := runMessageWithOutput(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventFinal, Text: "intermediate", Done: true, Continues: true})
		emit(core.Event{Kind: core.EventFinal, Text: "terminal", Done: true})
		return nil
	}, "adapter", "hello", messageRunOptions{JSON: true, Output: &output})
	if err != nil || !strings.Contains(output.String(), "terminal") {
		t.Fatalf("continuation truncated: %q %v", output.String(), err)
	}
}
