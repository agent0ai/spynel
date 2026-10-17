package tui

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/workspace"
	tea "github.com/charmbracelet/bubbletea"
)

type notificationWorkflowHarness struct {
	harness.Harness
	prompt string
	active bool
}

func (h *notificationWorkflowHarness) IsActive(string) bool   { return h.active }
func (h *notificationWorkflowHarness) ThreadID(string) string { return "" }
func (h *notificationWorkflowHarness) Send(_ context.Context, _, prompt string, emit core.Emit) (string, bool, error) {
	h.prompt = prompt
	emit(core.Event{Kind: core.EventFinal, Text: "next answer", Done: true})
	return "controlled-thread", false, nil
}

func notificationWorkflowService(t *testing.T) (*app.Service, *notificationWorkflowHarness) {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	target := &notificationWorkflowHarness{}
	service := app.New(cfg, target)
	t.Cleanup(service.Runtime.Close)
	return service, target
}

func awaitNotificationMessage(t *testing.T, command tea.Cmd) tea.Msg {
	t.Helper()
	output := make(chan tea.Msg, 1)
	go func() { output <- command() }()
	select {
	case message := <-output:
		return message
	case <-time.After(2 * time.Second):
		t.Fatal("notification watcher did not advance")
		return nil
	}
}

type notificationSelectionRouter struct{ calls int }

func (r *notificationSelectionRouter) Deliver(context.Context, string, string, string, string, []core.OutboundAttachment) error {
	r.calls++
	return nil
}

func TestNewScreenPublishesSelectionBeforeNotificationFallback(t *testing.T) {
	service, _ := notificationWorkflowService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.AllowedUsers = []string{"7"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, origin := range [][2]string{{"tui", "original"}, {"telegram", "TG-7"}} {
		if _, err := service.History.Ensure(origin[0], origin[1]); err != nil {
			t.Fatal(err)
		}
	}
	router := &notificationSelectionRouter{}
	service.DeliveryControl = router
	if err := service.RegisterLiveTUI("client", "original", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	m := notificationTestModel()
	m.ctx, m.conversation = ctx, "original"
	m.liveConversation = &liveConversationTracker{conversation: "original"}
	m.historyDirectory = service.Config.StatePath("history")
	var screen *core.Screen
	if err := service.Handle(ctx, core.Message{Channel: "tui", InstanceID: "client", Conversation: "original", Text: "/new"}, func(event core.Event) {
		if event.Screen != nil {
			screen = event.Screen
			m.openScreen(*screen)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if screen == nil || m.conversation == "original" || m.liveConversation.Get() != m.conversation {
		t.Fatal("screen did not switch")
	}
	// The owner must already see the switch, before any periodic renewal.
	if _, err := service.NotifyWithFallback(ctx, "tui/original", "telegram/TG-7", "result after switch"); err != nil {
		t.Fatal(err)
	}
	if err := service.Orchestrator.Outbox.Process(ctx); err != nil {
		t.Fatal(err)
	}
	remote, _, err := service.History.RecentEntries("telegram", "TG-7", 20, 12000)
	if err != nil || router.calls != 1 || len(remote) != 1 || remote[0].Content != "result after switch" {
		t.Fatalf("fallback sends=%d remote=%#v err=%v", router.calls, remote, err)
	}
	local, _, err := service.History.RecentEntries("tui", "original", 20, 12000)
	if err != nil || len(local) != 1 || local[0].Content != "/new" {
		t.Fatalf("unseen notification committed to original: %#v err=%v", local, err)
	}
}

func TestNotificationWorkflowSwitchAndResumeKeepDisplayHistoryAndNextChatTogether(t *testing.T) {
	for _, mode := range []string{"new", "resume"} {
		t.Run(mode, func(t *testing.T) {
			service, target := notificationWorkflowService(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := service.History.Append("tui", "original", history.Entry{Role: "user", Content: "original question"}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Notify(ctx, "tui/original", "prior notification"); err != nil {
				t.Fatal(err)
			}
			entries, _, offset, err := service.History.RecentEntriesSnapshot("tui", "original", 500, 500000)
			if err != nil {
				t.Fatal(err)
			}
			m := notificationTestModel()
			m.ctx, m.conversation = ctx, "original"
			m.transcript = transcriptFromHistory(entries)
			m.historyDirectory = service.Config.StatePath("history")
			m.watchConversationNotifications(offset)
			staleWait := m.waitEvent()
			oldStream := m.notifications
			var screen *core.Screen
			if mode == "new" {
				err = service.Handle(ctx, core.Message{Channel: "tui", InstanceID: "fixture", Conversation: "original", Text: "/new"}, func(event core.Event) {
					if event.Screen != nil {
						screen = event.Screen
					}
				})
			} else {
				action := "resume:" + base64.RawURLEncoding.EncodeToString([]byte("tui\x00original"))
				screen, err = service.ScreenActionForInstance(ctx, "fixture", "resume", action, nil)
			}
			if err != nil || screen == nil || screen.Conversation == "original" {
				t.Fatalf("switch screen=%#v err=%v", screen, err)
			}
			if mode == "resume" && screen.HistoryOffset <= 0 {
				t.Fatal("resume lost the transcript snapshot boundary")
			}
			// Both writes precede delivery of the screen, exercising the handoff gap.
			if _, err := service.Notify(ctx, "tui/original", "original-only result"); err != nil {
				t.Fatal(err)
			}
			stale := awaitNotificationMessage(t, staleWait)
			if _, err := service.Notify(ctx, "tui/"+screen.Conversation, "selected result"); err != nil {
				t.Fatal(err)
			}
			m.openScreen(*screen)
			next, _ := m.Update(stale)
			m = next.(model)
			next, _ = m.Update(awaitNotificationMessage(t, m.waitEvent()))
			m = next.(model)
			displayed := ""
			for _, entry := range m.transcript {
				displayed += entry.text + "\n"
			}
			if strings.Contains(displayed, "original-only result") || strings.Count(displayed, "selected result") != 1 {
				t.Fatalf("wrong switched transcript: %q", displayed)
			}
			if mode == "resume" && strings.Count(displayed, "prior notification") != 1 {
				t.Fatal("resume duplicated or lost prior notification")
			}
			select {
			case _, ok := <-oldStream:
				if ok {
					t.Fatal("old watcher retained an event after switching")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("old watcher did not stop")
			}
			for range 2 {
				if err := service.Orchestrator.Outbox.Process(ctx); err != nil {
					t.Fatal(err)
				}
			}
			selected, _, err := service.History.RecentEntries("tui", screen.Conversation, 500, 500000)
			if err != nil {
				t.Fatal(err)
			}
			saved := ""
			for _, entry := range selected {
				saved += entry.Content + "\n"
			}
			if strings.Count(saved, "selected result") != 1 || strings.Contains(saved, "original-only result") {
				t.Fatalf("wrong durable destination: %q", saved)
			}
			if err := service.Handle(ctx, core.Message{Channel: "tui", Conversation: screen.Conversation, Sender: "local", Text: "what happened?"}, func(core.Event) {}); err != nil {
				t.Fatal(err)
			}
			if strings.Count(target.prompt, "selected result") != 1 || strings.Contains(target.prompt, "original-only result") {
				t.Fatal("next chat did not receive only its own notification")
			}
			select {
			case duplicate := <-m.notifications:
				t.Fatalf("notification replayed: %#v", duplicate)
			case <-time.After(220 * time.Millisecond):
			}
		})
	}
}

func TestNotificationAcknowledgementStaysWithItsConversationAcrossSwitch(t *testing.T) {
	service, target := notificationWorkflowService(t)
	target.active = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, conversation := range []string{"original", "selected"} {
		if _, err := service.History.Ensure("tui", conversation); err != nil {
			t.Fatal(err)
		}
	}
	m := notificationTestModel()
	m.conversation = "original"
	m.working, m.streaming, m.responseText = true, "Hello ", "Hello "
	m.ackNotification = func(conversation, id string, after int) error {
		return service.AckNotification("tui/"+conversation, id, after)
	}
	id, err := service.Notify(ctx, "tui/original", "old pending result")
	if err != nil {
		t.Fatal(err)
	}
	m.pendingNotifications = []channel.Notification{{ID: id, Text: "old pending result"}}
	oldAck := m.beginNotificationAck()
	oldRetry := notificationAckRetryMsg{conversation: "original"}
	oldBoundary := notificationBoundaryMsg{sequence: m.deltaSequence, safe: true, after: 6}
	m.openScreen(core.Screen{ID: "welcome", Conversation: "selected"})
	// The captured command can finish after the screen changes, but must write
	// only its old destination and must not settle the new screen's work.
	oldResult := oldAck()
	next, _ := m.Update(oldResult)
	m = next.(model)
	next, _ = m.Update(oldRetry)
	m = next.(model)
	next, _ = m.Update(oldBoundary)
	m = next.(model)
	if len(m.transcript) != 0 || len(m.pendingNotifications) != 0 || m.notificationAckBusy {
		t.Fatal("old acknowledgement changed the selected transcript")
	}
	m.working, m.streaming, m.responseText = true, "New ", "New "
	newID, err := service.Notify(ctx, "tui/selected", "new pending result")
	if err != nil {
		t.Fatal(err)
	}
	m.pendingNotifications = []channel.Notification{{ID: newID, Text: "new pending result"}}
	newAck := m.beginNotificationAck()
	next, _ = m.Update(oldResult)
	m = next.(model)
	if !m.notificationAckBusy {
		t.Fatal("stale acknowledgement completed a new acknowledgement")
	}
	next, _ = m.Update(newAck())
	m = next.(model)
	if len(m.transcript) != 2 || m.transcript[0].text != "New " || m.transcript[1].text != "new pending result" {
		t.Fatalf("selected interleave=%#v", m.transcript)
	}
	for _, conversation := range []string{"original", "selected"} {
		raw, err := os.ReadFile(service.History.Path("tui", conversation))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), `"role":"notification_ack"`) != 1 {
			t.Fatalf("acknowledgement missing/duplicated in %s", conversation)
		}
		otherID := id
		if conversation == "original" {
			otherID = newID
		}
		if strings.Contains(string(raw), otherID) {
			t.Fatal("acknowledgement crossed conversation boundary")
		}
	}
}
