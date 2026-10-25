package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
)

func TestTelegramVerboseDeliveryMatrix(t *testing.T) {
	for _, mode := range []string{"", "on", "off"} {
		for _, route := range []string{"reply", "retained"} {
			t.Run(mode+"/"+route, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "fixture")
					bot.SetVerboseSource(func() string { return mode })
					var texts, methods []string
					files := 0
					path := filepath.Join(t.TempDir(), "result.txt")
					if err := os.WriteFile(path, []byte("synthetic file"), 0600); err != nil {
						t.Fatal(err)
					}
					attachments := []core.OutboundAttachment{{Kind: "attachment", Name: "result.txt", Path: path, MaxBytes: 1024}}
					bot.client.Transport = telegramRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/sendDocument") {
							files++
							if _, err := io.Copy(io.Discard, r.Body); err != nil {
								t.Fatal(err)
							}
							return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":92}}`))}, nil
						}
						var payload map[string]any
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						if text, ok := payload["text"].(string); ok {
							if payload["chat_id"] != "7" {
								t.Fatal("wrong destination")
							}
							texts = append(texts, text)
							methods = append(methods, r.URL.Path)
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":91}}`))}, nil
					})
					stream := func(emit core.Emit) {
						emit(core.Event{Kind: core.EventStatus, Text: "private diagnostic"})
						for range 100 {
							emit(core.Event{Kind: core.EventDelta, Text: "progress "})
						}
						synctest.Wait()
						time.Sleep(time.Second)
						synctest.Wait()
						want := 0
						if mode != "off" {
							want = 1
						}
						if len(texts) != want {
							t.Fatalf("progress sends=%d want %d", len(texts), want)
						}
						if want == 1 && texts[0] != strings.TrimSpace(strings.Repeat("progress ", 100)) {
							t.Fatal("injected progress heading", texts)
						}
						emit(core.Event{Kind: core.EventDelta, Text: "**Spy** is model-authored."})
						time.Sleep(time.Second)
						synctest.Wait()
						if len(texts) != want*2 || want == 1 && texts[1] != strings.Repeat("progress ", 100)+"<b>Spy</b> is model-authored." {
							t.Fatal("progress edit changed model text", texts)
						}
						final := "Final **answer**."
						emit(core.Event{Kind: core.EventFinal, Text: "progress\n" + final, FinalText: &final, Done: true, Attachments: attachments})
					}
					if route == "reply" {
						bot.handle(t.Context(), func(_ context.Context, _ core.Message, emit core.Emit) error {
							stream(emit)
							emit(core.Event{Kind: core.EventFinal, Text: "duplicate", Done: true})
							return nil
						}, &telegramMessage{MessageID: 10, Text: "/fixture", From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}})
					} else {
						stream(func(e core.Event) {
							if err := bot.DeliverEvent(t.Context(), "TG-7", "receipt", e); err != nil {
								t.Fatal(err)
							}
						})
					}
					want := 1
					if mode != "off" {
						want = 3
					}
					if files != 1 {
						t.Fatalf("files=%d", files)
					}
					if len(texts) != want || texts[len(texts)-1] != "Final <b>answer</b>." {
						t.Fatal(texts)
					}
					if mode != "off" && (!strings.HasSuffix(methods[1], "/editMessageText") || !strings.HasSuffix(methods[2], "/editMessageText")) {
						t.Fatal("final duplicated preview", methods)
					}
				})
			})
		}
	}
}

func TestTelegramVerboseLiveToggleRevocationAndFailedPreview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mode atomic.Value
		mode.Store("on")
		var allowed atomic.Value
		allowed.Store([]string{"7"})
		bot := New(config.Telegram{AllowedUsers: allowed.Load().([]string)}, "fixture")
		bot.SetVerboseSource(func() string { return mode.Load().(string) })
		bot.SetAllowedUsersSource(func() []string { return allowed.Load().([]string) })
		var texts []string
		fail := true
		bot.client.Transport = telegramRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			if fail {
				return nil, errors.New("synthetic preview failure")
			}
			texts = append(texts, p["text"].(string))
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":9}}`))}, nil
		})
		emit := func(text string) {
			if err := bot.DeliverEvent(t.Context(), "TG-7", "delta", core.Event{Kind: core.EventDelta, Text: text}); err != nil {
				t.Fatal(err)
			}
		}
		tick := func() { synctest.Wait(); time.Sleep(time.Second); synctest.Wait() }
		emit("failed")
		tick()
		fail = false
		mode.Store("off")
		emit("hidden")
		tick()
		mode.Store("on")
		emit("visible")
		tick()
		if len(texts) != 1 || strings.Contains(texts[0], "hidden") || strings.Contains(texts[0], "failed") {
			t.Fatal(texts)
		}
		allowed.Store([]string(nil))
		emitErr := bot.DeliverEvent(t.Context(), "TG-7", "end", core.Event{Kind: core.EventFinal, Text: "denied", Done: true})
		if emitErr == nil {
			t.Fatal("revoked final allowed")
		}
		tick()
		if len(texts) != 1 {
			t.Fatal("revoked progress sent")
		}
		bot.progress.Stop("TG-7")
	})
}

func TestTelegramVerboseLongFinalSurvivesEditFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(config.Telegram{AllowedUsers: []string{"7"}}, "fixture")
		b.SetVerboseSource(func() string { return "on" })
		var sent []string
		b.client.Transport = telegramRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/editMessageText") {
				return nil, errors.New("synthetic edit rejection")
			}
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			sent = append(sent, p["text"].(string))
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`))}, nil
		})
		b.DeliverEvent(t.Context(), "TG-7", "p", core.Event{Kind: core.EventDelta, Text: "progress"})
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		final := strings.Repeat("x", 10000)
		if err := b.DeliverEvent(t.Context(), "TG-7", "f", core.Event{Kind: core.EventFinal, Text: "progress\n" + final, FinalText: &final, Done: true}); err != nil {
			t.Fatal(err)
		}
		if len(sent) < 3 || strings.Join(sent[1:], "") != final {
			t.Fatal("final lost, repeated, or not chunked")
		}
		for _, text := range sent {
			if len(text) > 4096 {
				t.Fatal("oversized native message")
			}
		}
	})
}

func TestTelegramVerboseContinuationDoesNotReplaySuppressedDeltas(t *testing.T) {
	for _, route := range []string{"reply", "retained"} {
		for _, suffix := range []string{"", " Visible suffix."} {
			t.Run(route+suffix, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var mode atomic.Value
					mode.Store("off")
					var texts []string

					b := New(config.Telegram{AllowedUsers: []string{"7"}}, "fixture")
					b.SetVerboseSource(func() string { return mode.Load().(string) })
					b.client.Transport = telegramRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						var payload map[string]any
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						if text, ok := payload["text"].(string); ok {
							texts = append(texts, text)
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":9}}`))}, nil
					})

					stream := func(emit core.Emit) {
						tick := func() { synctest.Wait(); time.Sleep(time.Second); synctest.Wait() }
						emit(core.Event{Kind: core.EventDelta, Text: "HIDDEN commentary"})
						tick()
						mode.Store("on")
						item := "HIDDEN commentary" + suffix
						emit(core.Event{Kind: core.EventFinal, Text: item, FinalText: &item, Done: true, Continues: true})
						tick()
						if strings.Contains(strings.Join(texts, "\n"), "HIDDEN") {
							t.Fatal("hidden progress replayed", texts)
						}
						if suffix == "" && len(texts) != 0 || suffix != "" && (len(texts) != 1 || !strings.Contains(texts[0], strings.TrimSpace(suffix))) {
							t.Fatal("new suffix lost or duplicated", texts)
						}
						final := "Next turn final."
						emit(core.Event{Kind: core.EventFinal, Text: final, FinalText: &final, Done: true})
					}

					if route == "reply" {
						b.handle(t.Context(), func(_ context.Context, _ core.Message, emit core.Emit) error { stream(emit); return nil }, &telegramMessage{MessageID: 10, Text: "/fixture", From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}})
					} else {
						stream(func(e core.Event) {
							if err := b.DeliverEvent(t.Context(), "TG-7", "receipt", e); err != nil {
								t.Fatal(err)
							}
						})
					}

					if strings.Count(strings.Join(texts, "\n"), "Next turn final.") != 1 {
						t.Fatal("final lost or duplicated", texts)
					}
				})
			})
		}
	}
}
