//go:build linux || darwin

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

type conversationFollowupHarness struct {
	*supervisorHarness
	err      error
	complete bool
	sends    int
}

// Complete after Supervisor's activity observation, at real adapter admission.
type completingConversationCodex struct {
	*Codex
	completeBeforeSend bool
}

func (c *completingConversationCodex) complete() {
	if !c.completeBeforeSend {
		return
	}
	c.mu.Lock()
	state := c.active[c.session["chat:cli:completion"]]
	c.mu.Unlock()
	if state != nil {
		data, _ := json.Marshal(map[string]any{"threadId": state.threadID, "turn": map[string]any{"id": state.turnID, "status": "completed"}})
		c.handleNotification(wireMessage{Method: "turn/completed", Params: data})
	}
}

func (c *completingConversationCodex) SendWithInference(ctx context.Context, key, prompt string, selection InferenceSelection, emit core.Emit) (string, bool, error) {
	c.complete()
	return c.Codex.SendWithInference(ctx, key, prompt, selection, emit)
}

func (c *completingConversationCodex) SteerConversation(ctx context.Context, key, prompt string, emit core.Emit) (string, error) {
	c.complete()
	return c.Codex.SteerConversation(ctx, key, prompt, emit)
}

func TestWorkerConversationFollowupCompletionAtAdapterAdmission(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "codex-interrupt")
	codex, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := codex.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	target := &completingConversationCodex{Codex: codex}
	supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
	supervisor.current, supervisor.ctx = target, ctx
	reserved, restored := 0, 0
	host := &workerHost{supervisor: supervisor, info: WorkerInfo{ID: "completion-worker"}, directory: root, next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
	host.conversation = func(_, _ string, c WorkerConversation, receipt string, sendErr error) error {
		if sendErr == nil {
			reserved++
			return nil
		}
		var rejected *SendNotAdmittedError
		if !errors.As(sendErr, &rejected) || !reflect.DeepEqual(c.Sources, []string{"beta"}) || receipt != "completion-worker:1:followup:beta" {
			t.Fatal("completion did not restore only its proven rejected batch")
		}
		for _, event := range host.events(WorkerRequest{Sequence: 1}).Events {
			if event.Event.Done && !event.RequestOnly {
				t.Fatal("completion published before source restoration")
			}
		}
		restored++
		return nil
	}
	c := &WorkerConversation{Channel: "cli", Conversation: "completion", ExecutionID: "execution", SourceMessageID: "alpha", Sources: []string{"alpha"}}
	request := WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:completion", Prompt: "first", Conversation: c}
	if reply := host.apply(request); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	defer host.executions[1].file.Close()
	target.completeBeforeSend = true
	batch := *c
	batch.SourceMessageID, batch.Sources = "beta", []string{"beta"}
	request.Action, request.Prompt, request.Conversation = "followup", "second", &batch
	reply := host.apply(request)
	starts, steers := 0, 0
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Method == "turn/start" {
			starts++
		}
		if record.Method == "turn/steer" {
			steers++
		}
	}
	e := host.executions[1]
	if starts != 1 || steers != 0 || codex.IsActive(request.Key) || reply.Error == "" {
		t.Fatalf("strict follow-up admitted after completion: starts=%d, steers=%d, provider_active=%t, worker_done=%t, reply_error=%q", starts, steers, codex.IsActive(request.Key), e.Done, reply.Error)
	}
	if !e.Done || !e.Followups[0].Restored || reserved != 2 || restored != 1 {
		t.Fatal("completion lost its terminal or exact source restoration")
	}
	terminals := 0
	for _, event := range host.events(WorkerRequest{Sequence: 1}).Events {
		if event.Event.Done && !event.RequestOnly {
			terminals++
			if !reflect.DeepEqual(event.Conversation.Sources, []string{"alpha"}) {
				t.Fatal("terminal covered a rejected follow-up source")
			}
		}
	}
	host.apply(request)
	if terminals != 1 || reserved != 2 || restored != 1 || codex.IsActive(request.Key) {
		t.Fatal("retry repeated admission, restoration or provider delivery")
	}
}

func (h *conversationFollowupHarness) SteerConversation(ctx context.Context, key, prompt string, emit core.Emit) (string, error) {
	threadID, _, err := h.SendWithModel(ctx, key, prompt, "", emit)
	return threadID, err
}

func TestWorkerConversationFollowupNativeAdapters(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newWorkerFixture(t, scenario.name, scenario.mode)
			t.Logf("worker pid=%d start=%s generation=%s", f.info.PID, f.info.Process.StartTime, f.info.Process.Generation)
			c := &WorkerConversation{Channel: "cli", Conversation: "followup", ExecutionID: "execution", SourceMessageID: "alpha", Sources: []string{"alpha"}}
			request := WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:followup", Prompt: "first", Conversation: c}
			f.mustCall(t, request)
			batch := *c
			batch.SourceMessageID, batch.Sources = "beta", []string{"beta"}
			request.Action, request.Prompt, request.Conversation = "followup", "second", &batch
			f.mustCall(t, request)
			f.replaceOwner(t)
			f.mustCall(t, request)
			state := f.mustCall(t, WorkerRequest{Action: "inspect"})
			if state.NextSequence != 2 || len(state.Executions) != 1 || state.Executions[0].Done {
				t.Fatal("follow-up replaced or closed its execution")
			}
			f.mustCall(t, WorkerRequest{Action: "interrupt", Key: request.Key})
			deadline := time.Now().Add(5 * time.Second)
			for !f.mustCall(t, WorkerRequest{Action: "inspect"}).Executions[0].Done {
				if time.Now().After(deadline) {
					t.Fatal("provider terminal did not settle execution")
				}
				time.Sleep(10 * time.Millisecond)
			}
			page := f.mustCall(t, WorkerRequest{Action: "events", Sequence: 1})
			terminals := 0
			for _, event := range page.Events {
				if event.Event.Done && !event.Event.Continues && !event.RequestOnly {
					terminals++
					if !reflect.DeepEqual(event.Conversation.Sources, []string{"alpha", "beta"}) {
						t.Fatal("terminal lost exact follow-up sources")
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("provider terminal count = %d", terminals)
			}
			last := page.Events[len(page.Events)-1].Cursor
			f.mustCall(t, WorkerRequest{Action: "commit", Sequence: 1, After: last})
			f.mustCall(t, WorkerRequest{Action: "ack", Sequence: 1, After: last})
		})
	}
}

func (h *conversationFollowupHarness) SendWithModel(ctx context.Context, key, prompt, model string, emit core.Emit) (string, bool, error) {
	h.sends++
	if h.sends > 1 {
		if h.complete {
			h.finish(key)
		}
		if h.err != nil {
			return "synthetic-thread", true, h.err
		}
		h.mu.Lock()
		previous := h.emits[key]
		h.mu.Unlock()
		if previous != nil {
			previous(core.Event{Kind: core.EventStatus, Text: "Response continued on a newer message", Done: true})
		}
	}
	return h.supervisorHarness.SendWithModel(ctx, key, prompt, model, emit)
}

func TestWorkerConversationFollowupAdmission(t *testing.T) {
	for _, scenario := range []string{"steered", "rejected", "uncertain", "source-refused", "concurrent-rejection", "completed-before-delivery", "storage-failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			target := &conversationFollowupHarness{supervisorHarness: &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}}
			if scenario == "rejected" || scenario == "concurrent-rejection" || scenario == "storage-failure" {
				target.err = &SendNotAdmittedError{Err: errors.New("synthetic rejection")}
			} else if scenario == "uncertain" {
				target.err = errors.New("synthetic acknowledgement loss after effect")
			}
			target.complete = scenario == "concurrent-rejection"
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, info: WorkerInfo{ID: "synthetic-worker"}, directory: root, next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
			t.Cleanup(func() {
				for _, e := range host.executions {
					_ = e.file.Close()
				}
			})
			reserved, restored := 0, 0
			host.conversation = func(_, _ string, c WorkerConversation, receipt string, sendErr error) error {
				if sendErr == nil {
					reserved++
					if reserved == 2 && scenario == "source-refused" {
						return errors.New("private-storage-path-and-content")
					}
					if reserved == 2 && scenario == "completed-before-delivery" {
						target.finish("chat:cli:owned")
					}
					return nil
				}
				if !reflect.DeepEqual(c.Sources, []string{"beta"}) || receipt == "synthetic-worker:1:admission" {
					t.Fatal("restoration changed the attempted source batch or reused the initial receipt")
				}
				for _, event := range host.events(WorkerRequest{Sequence: 1}).Events {
					if event.Event.Done && !event.RequestOnly {
						t.Fatal("terminal published before source restoration")
					}
				}
				restored++
				if scenario == "storage-failure" {
					return errors.New("private storage failure")
				}
				return nil
			}
			initial := &WorkerConversation{Channel: "cli", Conversation: "owned", SourceMessageID: "alpha", ExecutionID: "execution", Sources: []string{"alpha"}}
			if reply := host.apply(WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:owned", Prompt: "first", Conversation: initial}); reply.Error != "" {
				t.Fatal(reply.Error)
			}
			batch := *initial
			batch.SourceMessageID, batch.Sources = "beta", []string{"beta"}
			request := WorkerRequest{Action: "followup", Sequence: 1, Key: "chat:cli:owned", Prompt: "second", Message: "second", Conversation: &batch}
			if scenario == "steered" {
				for _, change := range []func(*WorkerRequest, *WorkerConversation){
					func(r *WorkerRequest, _ *WorkerConversation) { r.Sequence++ },
					func(r *WorkerRequest, _ *WorkerConversation) { r.Key = "chat:cli:other" },
					func(_ *WorkerRequest, c *WorkerConversation) { c.ExecutionID = "other" },
					func(_ *WorkerRequest, c *WorkerConversation) { c.Recovery = true },
					func(_ *WorkerRequest, c *WorkerConversation) {
						c.SourceMessageID, c.Sources = "alpha", []string{"alpha"}
					},
				} {
					invalid, invalidBatch := request, batch
					invalid.Conversation = &invalidBatch
					change(&invalid, &invalidBatch)
					if host.apply(invalid).Error == "" || reserved != 1 || target.sends != 1 {
						t.Fatal("invalid follow-up had admission effects")
					}
				}
			}
			reply := host.apply(request)
			wantSends := 2
			if scenario == "completed-before-delivery" || scenario == "source-refused" {
				wantSends = 1
			}
			if reserved != 2 || target.sends != wantSends {
				t.Fatalf("follow-up did not reserve and send once: %d/%d, %s", reserved, target.sends, reply.Error)
			}
			e := host.executions[1]
			if scenario == "storage-failure" {
				if host.failure == nil || e.Done || reply.Error != "restore worker follow-up admission failed" {
					t.Fatal("failed restoration did not stop without completion")
				}
				return
			}
			if (reply.Error != "") != (target.err != nil || scenario == "completed-before-delivery" || scenario == "source-refused") {
				t.Fatalf("incorrect acknowledgement: %s", reply.Error)
			}
			if scenario == "source-refused" && reply.Error != "worker conversation source reservation refused" {
				t.Fatal("source refusal exposed storage details")
			}
			cursor := e.LastCursor
			host.apply(request)
			if reserved != 2 || target.sends != wantSends || e.LastCursor != cursor {
				t.Fatal("retry repeated admission, provider delivery or journal effects")
			}
			changed := request
			changed.Prompt = "changed"
			if host.apply(changed).Error == "" {
				t.Fatal("changed retry was accepted")
			}
			if scenario != "concurrent-rejection" && scenario != "completed-before-delivery" {
				if e.Done {
					t.Fatal("response handoff or uncertain Send ended the execution")
				}
				target.finish(request.Key)
			}
			want := []string{"alpha", "beta"}
			if restored > 0 || scenario == "source-refused" {
				want = []string{"alpha"}
			}
			terminals := 0
			for _, event := range host.events(WorkerRequest{Sequence: 1}).Events {
				if event.Event.Done && !event.Event.Continues && !event.RequestOnly {
					terminals++
					if !reflect.DeepEqual(event.Conversation.Sources, want) {
						t.Fatalf("terminal covered wrong sources: %v", event.Conversation.Sources)
					}
				}
			}
			if terminals != 1 || !e.Done {
				t.Fatal("provider terminal was lost or duplicated")
			}
		})
	}
}

func TestWorkerConversationFollowupStorageFailsClosed(t *testing.T) {
	for _, scenario := range []string{"checkpoint", "pending-events"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			target := &supervisorHarness{name: "codex", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, info: WorkerInfo{ID: "synthetic-worker"}, directory: root, next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
			reserved := 0
			host.conversation = func(string, string, WorkerConversation, string, error) error { reserved++; return nil }
			c := &WorkerConversation{Channel: "cli", Conversation: "owned", ExecutionID: "execution", SourceMessageID: "alpha", Sources: []string{"alpha"}}
			request := WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:owned", Prompt: "first", Conversation: c}
			if reply := host.apply(request); reply.Error != "" {
				t.Fatal(reply.Error)
			}
			e := host.executions[1]
			defer e.file.Close()
			cursor := e.LastCursor
			if scenario == "checkpoint" {
				if err := os.Remove(e.file.Name() + ".admission"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(e.file.Name()+".admission", 0700); err != nil {
					t.Fatal(err)
				}
				batch := *c
				batch.SourceMessageID, batch.Sources = "beta", []string{"beta"}
				request.Action, request.Conversation = "followup", &batch
				if reply := host.apply(request); reply.Error != "persist worker follow-up failed" || reserved != 1 {
					t.Fatal("checkpoint failure reserved or sent a batch")
				}
			} else {
				e.conversationSending, host.bytes = true, workerJournalBytes-1
				host.capture(e, core.Event{Kind: core.EventDelta, Text: "synthetic progress"})
			}
			if host.failure == nil || e.Done || e.LastCursor != cursor {
				t.Fatal("storage failure acknowledged unrecorded progress or completion")
			}
		})
	}
}

func TestWorkerConversationFollowupQueuedCompletion(t *testing.T) {
	root := t.TempDir()
	target := &supervisorHarness{name: "acp", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{}}
	supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
	supervisor.current, supervisor.ctx = target, context.Background()
	host := &workerHost{supervisor: supervisor, info: WorkerInfo{ID: "synthetic-worker"}, directory: root, next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{}), conversation: func(string, string, WorkerConversation, string, error) error { return nil }}
	c := &WorkerConversation{Channel: "cli", Conversation: "owned", ExecutionID: "execution", SourceMessageID: "alpha", Sources: []string{"alpha"}}
	request := WorkerRequest{Action: "send", Sequence: 1, Key: "chat:cli:owned", Prompt: "first", Conversation: c}
	if reply := host.apply(request); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	e := host.executions[1]
	defer e.file.Close()
	for _, source := range []string{"beta", "gamma"} {
		batch := *c
		batch.SourceMessageID, batch.Sources = source, []string{source}
		request.Action, request.Conversation, request.Message, request.Prompt = "followup", &batch, source, source
		if reply := host.apply(request); reply.Error != "" {
			t.Fatal(reply.Error)
		}
	}
	target.finish(request.Key)
	waitSupervisorState(t, func() bool { return target.IsActive(request.Key) })
	target.mu.Lock()
	count := len(target.prompts[request.Key])
	target.mu.Unlock()
	if count != 2 {
		t.Fatal("queued follow-ups did not share one continuation")
	}
	target.finish(request.Key)
	page := host.events(WorkerRequest{Sequence: 1})
	terminals := 0
	for _, event := range page.Events {
		if event.Event.Kind == core.EventFinal && event.Event.Continues && event.Conversation.SourceMessageID != "alpha" {
			t.Fatal("queued handoff changed the original response identity")
		}
		if event.Event.Done && !event.Event.Continues && !event.RequestOnly {
			terminals++
			if event.Conversation.SourceMessageID != "gamma" || !reflect.DeepEqual(event.Conversation.Sources, []string{"alpha", "beta", "gamma"}) {
				t.Fatal("queued final lost request identity or exact accumulated sources")
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal count = %d", terminals)
	}
}
