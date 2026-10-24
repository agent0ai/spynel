package whatsapp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/workspace"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestWhatsAppVerboseDeliveryMatrix(t *testing.T) {
	for _, mode := range []string{"auto", "on", "off"} {
		for _, route := range []string{"reply", "retained"} {
			t.Run(mode+"/"+route, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, t.TempDir()+"/wa.db")
					c.ctx = t.Context()
					c.SetVerboseSource(func() string { return mode })
					var texts []string
					edits, files := 0, 0
					path := filepath.Join(t.TempDir(), "result.txt")
					if err := os.WriteFile(path, []byte("synthetic file"), 0600); err != nil {
						t.Fatal(err)
					}
					attachments := []core.OutboundAttachment{{Kind: "attachment", Name: "result.txt", Path: path, MediaType: "text/plain", MaxBytes: 1024}}
					c.upload = func(_ context.Context, r io.Reader, _ io.ReadWriteSeeker, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
						_, err := io.Copy(io.Discard, r)
						return whatsmeow.UploadResponse{}, err
					}
					c.deliver = func(_ context.Context, chat types.JID, m *waE2E.Message) (whatsmeow.SendResponse, error) {
						if chat.User != "15551234567" {
							t.Fatal("wrong destination")
						}
						if m.GetDocumentMessage() != nil {
							files++
							return whatsmeow.SendResponse{ID: "file"}, nil
						}
						text := m.GetConversation()
						if edit := m.GetEditedMessage().GetMessage().GetProtocolMessage(); edit != nil {
							edits++
							text = edit.GetEditedMessage().GetConversation()
						}
						texts = append(texts, text)
						return whatsmeow.SendResponse{ID: "preview"}, nil
					}
					c.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error { return nil }
					stream := func(emit core.Emit) {
						emit(core.Event{Kind: core.EventStatus, Text: "private diagnostic"})
						for range 100 {
							emit(core.Event{Kind: core.EventDelta, Text: "progress "})
						}
						synctest.Wait()
						time.Sleep(time.Second)
						synctest.Wait()
						want := 0
						if mode == "on" {
							want = 1
						}
						if len(texts) != want {
							t.Fatal(texts)
						}
						final := "Final **answer**."
						emit(core.Event{Kind: core.EventFinal, Text: "progress\n" + final, FinalText: &final, Done: true, Attachments: attachments})
					}
					if route == "reply" {
						c.handler = func(_ context.Context, _ core.Message, emit core.Emit) error {
							stream(emit)
							emit(core.Event{Kind: core.EventFinal, Text: "duplicate", Done: true})
							return nil
						}
						c.handle(time.Now(), types.NewJID("15551234567", types.DefaultUserServer), "15551234567", "synthetic")
					} else {
						stream(func(e core.Event) {
							if err := c.DeliverEvent(t.Context(), "WA-15551234567", "receipt", e); err != nil {
								t.Fatal(err)
							}
						})
					}
					want := 1
					if mode == "on" {
						want = 2
					}
					if files != 1 {
						t.Fatalf("files=%d", files)
					}
					if len(texts) != want || texts[len(texts)-1] != "Final *answer*." {
						t.Fatal(texts)
					}
					if mode == "on" && edits != 1 {
						t.Fatal("final duplicated preview")
					}
				})
			})
		}
	}
}

func TestWhatsAppVerboseLiveToggleAndRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mode atomic.Value
		mode.Store("on")
		var allowed atomic.Value
		allowed.Store([]string{"15551234567"})
		c := New(config.WhatsApp{AllowedNumbers: allowed.Load().([]string)}, t.TempDir()+"/wa.db")
		c.SetVerboseSource(func() string { return mode.Load().(string) })
		c.SetAllowedNumbersSource(func() []string { return allowed.Load().([]string) })
		var texts []string
		c.deliver = func(_ context.Context, _ types.JID, m *waE2E.Message) (whatsmeow.SendResponse, error) {
			text := m.GetConversation()
			if edit := m.GetEditedMessage().GetMessage().GetProtocolMessage(); edit != nil {
				text = edit.GetEditedMessage().GetConversation()
			}
			texts = append(texts, text)
			return whatsmeow.SendResponse{ID: "preview"}, nil
		}
		emit := func(text string) {
			if err := c.DeliverEvent(t.Context(), "WA-15551234567", "delta", core.Event{Kind: core.EventDelta, Text: text}); err != nil {
				t.Fatal(err)
			}
		}
		tick := func() { synctest.Wait(); time.Sleep(time.Second); synctest.Wait() }
		emit("before")
		tick()
		mode.Store("off")
		emit("hidden")
		tick()
		mode.Store("on")
		emit("after")
		tick()
		if len(texts) != 2 || strings.Contains(texts[1], "hidden") || strings.Contains(texts[1], "before") {
			t.Fatal(texts)
		}
		emit("pending")
		allowed.Store([]string(nil))
		tick()
		if len(texts) != 2 {
			t.Fatal("revoked progress sent")
		}
		if err := c.DeliverEvent(t.Context(), "WA-15551234567", "end", core.Event{Kind: core.EventFinal, Text: "denied", Done: true}); err == nil {
			t.Fatal("revoked final allowed")
		}
		c.progress.Stop("WA-15551234567")
	})
}

func TestWhatsAppVerboseLongFinalSurvivesEditFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(config.WhatsApp{AllowedNumbers: []string{"15551234567"}}, t.TempDir()+"/wa.db")
		c.SetVerboseSource(func() string { return "on" })
		var sent []string
		c.deliver = func(_ context.Context, _ types.JID, m *waE2E.Message) (whatsmeow.SendResponse, error) {
			if m.GetEditedMessage() != nil {
				return whatsmeow.SendResponse{}, context.Canceled
			}
			sent = append(sent, m.GetConversation())
			return whatsmeow.SendResponse{ID: "preview"}, nil
		}
		c.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error { return nil }
		c.DeliverEvent(t.Context(), "WA-15551234567", "p", core.Event{Kind: core.EventDelta, Text: "progress"})
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		final := strings.Repeat("x", 65000)
		if err := c.DeliverEvent(t.Context(), "WA-15551234567", "f", core.Event{Kind: core.EventFinal, Text: "progress\n" + final, FinalText: &final, Done: true}); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 3 || strings.Join(sent[1:], "") != final {
			t.Fatal("final lost, repeated, or not chunked")
		}
		for _, text := range sent {
			if len(text) > 60000 {
				t.Fatal("oversized native message")
			}
		}
	})
}

// This controllable provider keeps the application turn alive across commands.
type verboseHarness struct {
	emit              core.Emit
	sends, interrupts int
}

func (h *verboseHarness) Start(context.Context) error { return nil }
func (h *verboseHarness) Close() error                { return nil }
func (h *verboseHarness) ResetSession(string) error   { return nil }
func (h *verboseHarness) ThreadID(string) string      { return "fixture-thread" }
func (h *verboseHarness) IsActive(string) bool        { return h.emit != nil }
func (h *verboseHarness) Interrupt(context.Context, string) (bool, error) {
	h.interrupts++
	return false, nil
}
func (h *verboseHarness) Send(_ context.Context, _, _ string, emit core.Emit) (string, bool, error) {
	h.emit = emit
	h.sends++
	return "fixture-thread", false, nil
}

func TestWhatsAppVerboseConnectedApplicationTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		if err := workspace.Init(root, false); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(config.PathForRoot(root))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Extensions.Enabled = false
		cfg.Channels.Verbose = "off"
		cfg.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
		provider := &verboseHarness{}
		service := app.New(cfg, provider)
		defer service.Close()
		c := New(cfg.Channels.WhatsApp, filepath.Join(root, "wa.db"))
		c.ctx = t.Context()
		c.handler = service.Handle
		c.SetVerboseSource(func() string { return service.Settings.Snapshot().Channels.Verbose })
		c.presence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error { return nil }
		var texts []string
		c.deliver = func(_ context.Context, _ types.JID, m *waE2E.Message) (whatsmeow.SendResponse, error) {
			text := m.GetConversation()
			if edit := m.GetEditedMessage().GetMessage().GetProtocolMessage(); edit != nil {
				text = edit.GetEditedMessage().GetConversation()
			}
			texts = append(texts, text)
			return whatsmeow.SendResponse{ID: "preview"}, nil
		}
		chat := types.NewJID("15551234567", types.DefaultUserServer)
		input := func(text string) { c.handle(time.Now(), chat, chat.User, text) }
		tick := func() { synctest.Wait(); time.Sleep(time.Second); synctest.Wait() }
		input("Start the synthetic turn")
		if provider.emit == nil {
			t.Fatal("provider not admitted")
		}
		provider.emit(core.Event{Kind: core.EventDelta, Text: "HIDDEN-ONE\n"})
		tick()
		if len(texts) != 0 {
			t.Fatal(texts)
		}
		input("/verbose on")
		provider.emit(core.Event{Kind: core.EventDelta, Text: "visible progress\n"})
		tick()
		input("/verbose off")
		provider.emit(core.Event{Kind: core.EventDelta, Text: "HIDDEN-TWO\n"})
		tick()
		input("/verbose on")
		provider.emit(core.Event{Kind: core.EventDelta, Text: "new progress\n"})
		tick()
		final := "Connected final."
		provider.emit(core.Event{Kind: core.EventFinal, Text: "HIDDEN-ONE\nvisible progress\nHIDDEN-TWO\nnew progress\n" + final, FinalText: &final, Done: true})
		provider.emit = nil
		if provider.sends != 1 || provider.interrupts != 0 {
			t.Fatal("verbosity changed provider lifecycle")
		}
		joined := strings.Join(texts, "\n")
		if strings.Contains(joined, "HIDDEN") || !strings.Contains(joined, "visible progress") || !strings.Contains(joined, "new progress") || strings.Count(joined, final) != 1 {
			t.Fatal(joined)
		}
		loaded, err := config.Load(cfg.Path)
		if err != nil || loaded.Channels.Verbose != "on" {
			t.Fatal("mode not persisted", err)
		}
		entries, _, err := service.History.Entries("whatsapp", "WA-15551234567")
		if err != nil {
			t.Fatal(err)
		}
		retained := false
		for _, e := range entries {
			if strings.Contains(e.Content, "HIDDEN-ONE") {
				retained = true
			}
		}
		if !retained {
			t.Fatal("presentation filtering removed history")
		}
	})
}
