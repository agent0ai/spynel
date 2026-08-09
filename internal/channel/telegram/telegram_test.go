package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/media"
)

type fixedTranscriber struct{ text string }

type telegramRoundTripFunc func(*http.Request) (*http.Response, error)

func (f telegramRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (t fixedTranscriber) Transcribe(context.Context, string) (string, error) { return t.text, nil }

type blockingTelegramTranscriber struct {
	entered chan struct{}
	release <-chan struct{}
}

func (t blockingTelegramTranscriber) Transcribe(ctx context.Context, _ string) (string, error) {
	close(t.entered)
	select {
	case <-t.release:
		return "voice words", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestSplitHonorsTelegramCharacterLimit(t *testing.T) {
	text := ""
	for i := 0; i < 5000; i++ {
		text += "界"
	}
	chunks := split(text, 4096)
	if len(chunks) != 2 || len([]rune(chunks[0])) > 4096 || len([]rune(chunks[1])) > 4096 {
		t.Fatalf("unexpected chunks: %d (%d, %d)", len(chunks), len([]rune(chunks[0])), len([]rune(chunks[1])))
	}
}

func TestTelegramRealShapeReplyCarriesIDAndCaption(t *testing.T) {
	var update telegramUpdate
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":303,"from":{"id":7},"chat":{"id":7,"type":"private"},"date":10,"text":"reply","reply_to_message":{"message_id":91,"from":{"id":8},"chat":{"id":7,"type":"private"},"date":9,"caption":"  caption\nwith   space  "}}}`), &update); err != nil {
		t.Fatal(err)
	}
	var got core.Message
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.handle(context.Background(), func(_ context.Context, message core.Message, _ core.Emit) error {
		got = message
		return nil
	}, update.Message)
	if got.ReplyTo != "91 caption with space" {
		t.Fatalf("Telegram reply_to = %q", got.ReplyTo)
	}
	update.Message.ReplyToMessage.Text = ""
	update.Message.ReplyToMessage.Caption = ""
	if got := telegramReplyTo(update.Message); got != "91" {
		t.Fatalf("Telegram ID-only reply_to = %q", got)
	}
	update.Message.ReplyToMessage = nil
	if got := telegramReplyTo(update.Message); got != "" {
		t.Fatalf("Telegram non-reply reply_to = %q", got)
	}
}

func TestSendAttachmentUsesNativeTelegramMediaMethods(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "report.txt")
	if err := os.WriteFile(path, []byte("report body"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, got *http.Request) {
		if err := got.ParseMultipartForm(1024); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		request <- got
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	if err := bot.sendAttachment(context.Background(), "42", core.OutboundAttachment{
		Kind: "attachment", Name: "report.txt", Path: path, MediaType: "text/plain", MaxBytes: 1024,
	}, 8); err != nil {
		t.Fatal(err)
	}
	got := <-request
	if got.URL.Path != "/bottest/sendDocument" || got.FormValue("chat_id") != "42" || got.FormValue("reply_parameters") != `{"message_id":8}` {
		t.Fatalf("request path = %q, form = %#v", got.URL.Path, got.MultipartForm.Value)
	}
	file, header, err := got.FormFile("document")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if header.Filename != "report.txt" {
		t.Fatalf("filename = %q", header.Filename)
	}
}

func TestLongPollingRoutesMessageAndSendsReply(t *testing.T) {
	var polls atomic.Int32
	sent := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/getUpdates":
			if polls.Add(1) == 1 {
				_, _ = writer.Write([]byte(`{"ok":true,"result":[{"update_id":4,"message":{"message_id":8,"from":{"id":7,"username":"trusted"},"chat":{"id":42},"date":1,"text":"/status"}}]}`))
				return
			}
			<-request.Context().Done()
		case "/bottest/sendChatAction":
			_, _ = writer.Write([]byte(`{"ok":true}`))
		case "/bottest/sendMessage":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			sent <- payload
			_, _ = writer.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 1}, "test")
	bot.baseURL = server.URL
	statuses := make(chan channel.ConnectionStatus, 4)
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses <- status })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(_ context.Context, message core.Message, emit core.Emit) error {
			if message.Channel != "telegram" || message.Conversation != "TG-7" || message.Sender != "@trusted" || message.Text != "/status" {
				t.Errorf("unexpected message %#v", message)
			}
			emit(core.Event{Kind: core.EventFinal, Text: "**ready** with `code`", Done: true})
			return nil
		})
	}()
	select {
	case payload := <-sent:
		if payload["chat_id"] != "42" || payload["text"] != "<b>ready</b> with <code>code</code>" || payload["parse_mode"] != "HTML" {
			t.Fatalf("unexpected Telegram reply %#v", payload)
		}
		cancel()
	case <-ctx.Done():
		t.Fatal("timed out waiting for Telegram reply")
	}
	select {
	case status := <-statuses:
		if status.State != channel.ConnectionConnected || status.Name != "telegram" || status.Identity != "@spynel_test_bot" || status.Link != "https://t.me/spynel_test_bot" {
			t.Fatalf("unexpected Telegram status %#v", status)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Telegram connection status")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Telegram poller did not stop")
	}
}

func TestWorkflowSlashCommandIsRoutedToSharedHandler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	var got core.Message
	bot.processUpdate(context.Background(), func(_ context.Context, message core.Message, emit core.Emit) error {
		got = message
		emit(core.Event{Kind: core.EventFinal, Done: true})
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		MessageID: 9,
		From:      telegramUser{ID: 7, Username: "trusted"},
		Chat:      telegramChat{ID: 7, Type: "private"},
		Date:      10,
		Text:      "/goals review --detail",
	}})

	if got.Channel != "telegram" || got.Conversation != "TG-7" || got.Text != "/goals review --detail" {
		t.Fatalf("routed message = %#v", got)
	}
}

func TestTelegramSendsOnlyLastTerminalResponse(t *testing.T) {
	sent := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			_, _ = writer.Write([]byte(`{"ok":true}`))
		case "/bottest/sendMessage":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			sent <- payload["text"].(string)
			_, _ = writer.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		lastResponse := "last response"
		emit(core.Event{Kind: core.EventDelta, Text: "streamed progress"})
		emit(core.Event{Kind: core.EventStatus, Text: "transport handoff", Done: true})
		emit(core.Event{Kind: core.EventFinal, Text: "intermediate response", Done: true, Continues: true})
		emit(core.Event{Kind: core.EventFinal, Text: "progress update\nlast response", FinalText: &lastResponse, Done: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})

	select {
	case got := <-sent:
		if got != "last response" {
			t.Fatalf("Telegram sent %q, want only the last response", got)
		}
	default:
		t.Fatal("Telegram did not send the last response")
	}
	select {
	case extra := <-sent:
		t.Fatalf("Telegram sent an intermediate response: %q", extra)
	default:
	}
}

func TestTelegramFormatsErrorsAsOrdinaryUnindentedResponses(t *testing.T) {
	sent := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			_, _ = writer.Write([]byte(`{"ok":true}`))
		case "/bottest/sendMessage":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			sent <- payload["text"].(string)
			_, _ = writer.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	partial := "partial"
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventError, Text: "first line\nsecond line", FinalText: &partial, Done: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
		return errors.New("handler failed")
	}, &telegramMessage{MessageID: 9, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 2, Text: "hello"})

	for _, want := range []string{"Error first line\nsecond line", "Error handler failed"} {
		select {
		case got := <-sent:
			if got != want {
				t.Fatalf("Telegram error reply = %q, want %q", got, want)
			}
		default:
			t.Fatalf("Telegram did not send error reply %q", want)
		}
	}
}

func TestTelegramConnectionStatusRejectsUnsafeBotUsername(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.me.Username = "unsafe](https://example.com)"
	var got channel.ConnectionStatus
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })
	bot.reportStatus(channel.ConnectionConnected, "")
	if got.Identity != "" || got.Link != "" {
		t.Fatalf("unsafe Telegram username reached connection status: %#v", got)
	}
}

func TestMissingTokenReportsConnectionError(t *testing.T) {
	bot := New(config.Telegram{}, "")
	var got channel.ConnectionStatus
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })

	if err := bot.Run(context.Background(), nil); err == nil {
		t.Fatal("Run() succeeded without a token")
	}
	if got.Name != "telegram" || got.State != channel.ConnectionError || got.Detail == "" {
		t.Fatalf("connection status = %#v", got)
	}
}

func TestEmptyWhitelistReportsConnectionErrorAndRejectsUsers(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"  "}}, "test")
	var got channel.ConnectionStatus
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })
	if err := bot.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "allowed_users") {
		t.Fatalf("Run() accepted an empty whitelist: %v", err)
	}
	if got.State != channel.ConnectionError || !strings.Contains(got.Detail, "allowed_users") {
		t.Fatalf("connection status = %#v", got)
	}
	if bot.allowed(telegramUser{ID: 7, Username: "trusted"}) {
		t.Fatal("empty whitelist accepted a Telegram user")
	}
}

func TestInvalidRuntimeAllowListsHaveZeroPollingOrWebhookSideEffects(t *testing.T) {
	invalid := [][]string{nil, {}, {"  "}, {"@"}, {"..."}, {"bad user"}}
	for _, mode := range []string{"polling", "webhook"} {
		for index, allowed := range invalid {
			t.Run(fmt.Sprintf("%s-%d", mode, index), func(t *testing.T) {
				cfg := config.Telegram{Mode: mode, AllowedUsers: allowed, WebhookListen: "127.0.0.1:0", WebhookURL: "https://public.example", WebhookSecret: "secret"}
				bot := New(cfg, "token")
				providerCalls, listenerBinds := 0, 0
				bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
					providerCalls++
					return nil, errors.New("unexpected provider call")
				})
				bot.listen = func(string, string) (net.Listener, error) {
					listenerBinds++
					return nil, errors.New("unexpected listener bind")
				}
				var statuses []channel.ConnectionStatus
				bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses = append(statuses, status) })
				if err := bot.Run(context.Background(), nil); !errors.Is(err, errTelegramRuntimeAuthorization) {
					t.Fatalf("Run() error = %v", err)
				}
				if providerCalls != 0 || listenerBinds != 0 {
					t.Fatalf("invalid runtime attempted provider=%d listener=%d", providerCalls, listenerBinds)
				}
				if len(statuses) != 1 || statuses[0].State != channel.ConnectionError {
					t.Fatalf("statuses = %#v", statuses)
				}
			})
		}
	}
}

func TestNextInboundUpdateRevalidatesLiveAllowListBeforeSideEffects(t *testing.T) {
	allowed := []string{"7"}
	identityPath := filepath.Join(t.TempDir(), "identities.json")
	bot := NewWithIdentityStore(config.Telegram{AllowedUsers: allowed}, "token", identityPath)
	bot.SetAllowedUsersSource(func() []string { return allowed })
	if err := bot.identity.RecordVerifiedPrivate(7, 7, "trusted"); err != nil {
		t.Fatal(err)
	}
	identityBefore, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	providerCalls, handlerCalls := 0, 0
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected provider call")
	})
	var status channel.ConnectionStatus
	bot.SetStatusReporter(func(next channel.ConnectionStatus) { status = next })
	allowed = []string{"..."}
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		handlerCalls++
		return nil
	}, telegramUpdate{Message: &telegramMessage{MessageID: 1, From: telegramUser{ID: 7, Username: "trusted"}, Chat: telegramChat{ID: 7, Type: "private"}, Text: "hello"}})
	if providerCalls != 0 || handlerCalls != 0 {
		t.Fatalf("revoked update attempted provider=%d handler=%d", providerCalls, handlerCalls)
	}
	identityAfter, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(identityAfter) != string(identityBefore) {
		t.Fatal("revoked update mutated persisted Telegram identity state")
	}
	if status.State != channel.ConnectionError || !strings.Contains(status.Detail, "allowed_users") {
		t.Fatalf("status = %#v", status)
	}
}

func TestLiveAllowListReplacementRejectsPreviouslyAllowedTelegramSender(t *testing.T) {
	allowed := []string{"7"}
	bot := New(config.Telegram{AllowedUsers: allowed}, "token")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	allowed = []string{"8"}
	handlerCalls := 0
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		handlerCalls++
		return nil
	}, telegramUpdate{Message: &telegramMessage{MessageID: 1, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Text: "hello"}})
	if handlerCalls != 0 {
		t.Fatal("replaced live Telegram allow-list retained the old sender")
	}
}

func TestGroupWelcomeDoesNotRequireAddressingTheBot(t *testing.T) {
	sent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/bottest/sendMessage" {
			http.NotFound(writer, request)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(request.Body).Decode(&payload)
		sent <- payload["text"].(string)
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{GroupMode: "mention", WelcomeEnabled: true, WelcomeMessage: "Hello, {name}!", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	called := false
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		called = true
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: -42, Type: "group"},
		NewChatMembers: []telegramUser{{ID: 9, FirstName: "Ada"}},
	}})
	select {
	case welcome := <-sent:
		if welcome != "Hello, Ada!" {
			t.Fatalf("welcome = %q", welcome)
		}
	case <-time.After(time.Second):
		t.Fatal("group welcome was blocked by mention-only message policy")
	}
	if called {
		t.Fatal("membership update was dispatched to the harness")
	}
}

func TestTelegramMarkdownChunksRemainWithinLimit(t *testing.T) {
	text := strings.Repeat("**bold & safe**\n", 1000)
	chunks := telegramChunks(text, 4096)
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > 4096 || strings.Contains(chunk, "**") {
			t.Fatalf("invalid formatted chunk of length %d: %.80q", len([]rune(chunk)), chunk)
		}
	}
}

func TestTelegramVoiceIsStoredAndTranscribed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getFile":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"file_path":"voice/file.ogg"}}`))
		case "/file/bottest/voice/file.ogg":
			_, _ = writer.Write([]byte("voice bytes"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	store := &media.Store{Directory: filepath.Join(t.TempDir(), "attachments"), MaxBytes: 1024}
	bot.SetMedia(store, fixedTranscriber{text: "hello from audio"})
	text, err := bot.messageText(context.Background(), &telegramMessage{Voice: &telegramMedia{FileID: "voice-id", FileUniqueID: "unique"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[Attachment voice-unique.ogg]") || !strings.Contains(text, "[Generated voice transcription") || !strings.Contains(text, "hello from audio") {
		t.Fatalf("message text = %q", text)
	}
	data, err := os.ReadFile(filepath.Join(store.Directory, "voice-unique.ogg"))
	if err != nil || string(data) != "voice bytes" {
		t.Fatalf("stored voice = %q, %v", string(data), err)
	}
}

func TestTelegramTypingRefreshesFromArrivalThroughVoiceTranscriptionAndAgentTurn(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			actions.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/getFile":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"file_path":"voice/file.ogg"}}`))
		case "/file/bottest/voice/file.ogg":
			_, _ = writer.Write([]byte("voice bytes"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	transcriptionEntered := make(chan struct{})
	transcriptionRelease := make(chan struct{})
	agentEntered := make(chan struct{})
	agentRelease := make(chan struct{})
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	bot.SetMedia(&media.Store{Directory: filepath.Join(t.TempDir(), "attachments"), MaxBytes: 1024}, blockingTelegramTranscriber{
		entered: transcriptionEntered, release: transcriptionRelease,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		bot.handle(ctx, func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventActivity, Active: true})
			close(agentEntered)
			<-agentRelease
			emit(core.Event{Kind: core.EventActivity})
			emit(core.Event{Kind: core.EventFinal, Done: true})
			return nil
		}, &telegramMessage{
			MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1,
			Voice: &telegramMedia{FileID: "voice-id", FileUniqueID: "unique"},
		})
	}()
	waitClosed(t, transcriptionEntered, "Telegram transcription")
	waitAtomicCount(t, &actions, 2, "Telegram typing during transcription")
	close(transcriptionRelease)
	waitClosed(t, agentEntered, "Telegram agent turn")
	beforeAgentRefresh := actions.Load()
	waitAtomicCount(t, &actions, beforeAgentRefresh+1, "Telegram typing during agent turn")
	close(agentRelease)
	waitClosed(t, done, "Telegram message completion")
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after the final event: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramFrameworkOnlyResponseDoesNotStartTyping(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventFinal, Text: "local result", Done: true, Local: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "/status"})

	time.Sleep(20 * time.Millisecond)
	if got := actions.Load(); got != 0 {
		t.Fatalf("framework-only response emitted %d typing actions", got)
	}
}

func TestTelegramHandlerReturnWithoutTerminalStopsArrivalActivity(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})

	time.Sleep(20 * time.Millisecond)
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after handler return: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramPanicUnwindStopsAgentActivity(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Telegram handler panic was not propagated")
			}
		}()
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventActivity, Active: true})
			waitAtomicCount(t, &actions, 1, "Telegram typing before panic")
			panic("provider panic")
		}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	}()

	time.Sleep(20 * time.Millisecond)
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after panic unwind: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramTypingStopsBeforeFinalDelivery(t *testing.T) {
	var actions atomic.Int32
	deliveryEntered := make(chan struct{})
	deliveryRelease := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			actions.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/sendMessage":
			close(deliveryEntered)
			<-deliveryRelease
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventActivity, Active: true})
			waitAtomicCount(t, &actions, 1, "Telegram typing before final delivery")
			emit(core.Event{Kind: core.EventFinal, Text: "intermediate", Done: true, Continues: true})
			emit(core.Event{Kind: core.EventActivity})
			emit(core.Event{Kind: core.EventFinal, Text: "complete", Done: true})
			return nil
		}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	}()
	waitClosed(t, deliveryEntered, "Telegram final delivery")
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		close(deliveryRelease)
		t.Fatalf("Telegram typing continued during final delivery: %d -> %d", stoppedAt, got)
	}
	close(deliveryRelease)
	waitClosed(t, done, "Telegram final delivery completion")
	stoppedAt = actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after delivery: %d -> %d", stoppedAt, got)
	}
}

func waitAtomicCount(t *testing.T, value *atomic.Int32, minimum int32, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value.Load() >= minimum {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (got %d, want at least %d)", description, value.Load(), minimum)
}

func waitClosed(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestWebhookModeVerifiesSecretAndRoutesUpdate(t *testing.T) {
	var webhookURL string
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/setWebhook":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			webhookURL, _ = payload["url"].(string)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer api.Close()
	bot := New(config.Telegram{Mode: "webhook", WebhookURL: "https://public.example", WebhookListen: "127.0.0.1:0", WebhookSecret: "secret", GroupMode: "mention", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = api.URL
	statuses := make(chan channel.ConnectionStatus, 4)
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses <- status })
	messages := make(chan core.Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(_ context.Context, message core.Message, _ core.Emit) error {
			messages <- message
			return nil
		})
	}()
	var detail string
	select {
	case status := <-statuses:
		if status.State != channel.ConnectionConnected {
			t.Fatalf("webhook status = %#v", status)
		}
		detail = status.Detail
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook listener")
	}
	parsed, err := url.Parse(webhookURL)
	if err != nil || parsed.Path == "" {
		t.Fatalf("registered webhook = %q, %v", webhookURL, err)
	}
	marker := " via "
	index := strings.LastIndex(detail, marker)
	if index < 0 {
		t.Fatalf("webhook status detail = %q", detail)
	}
	if strings.Contains(detail, parsed.Path) || strings.Contains(detail, webhookURL) {
		t.Fatalf("webhook status exposed its private public URL: %q", detail)
	}
	localURL := "http://" + detail[index+len(marker):] + parsed.Path
	post := func(secret string) int {
		request, _ := http.NewRequest(http.MethodPost, localURL, strings.NewReader(`{"update_id":1,"message":{"message_id":2,"from":{"id":7,"username":"trusted"},"chat":{"id":42,"type":"private"},"date":1,"text":"hello"}}`))
		request.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := post("wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong-secret status = %d", status)
	}
	if status := post("secret"); status != http.StatusOK {
		t.Fatalf("valid webhook status = %d", status)
	}
	select {
	case message := <-messages:
		if message.Text != "hello" || message.Conversation != "TG-7" {
			t.Fatalf("webhook message = %#v", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook update was not routed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook bot did not stop")
	}
}

func TestWebhookAuthorizationLossStopsListenerAndDeletesWebhook(t *testing.T) {
	allowed := []string{"7"}
	var webhookURL string
	var deletes atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/setWebhook":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			webhookURL, _ = payload["url"].(string)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/deleteWebhook":
			deletes.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer api.Close()
	bot := New(config.Telegram{Mode: "webhook", WebhookURL: "https://public.example", WebhookListen: "127.0.0.1:0", WebhookSecret: "secret", AllowedUsers: allowed}, "test")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	bot.baseURL = api.URL
	statuses := make(chan channel.ConnectionStatus, 4)
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses <- status })
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(context.Background(), func(context.Context, core.Message, core.Emit) error { return nil })
	}()
	var localURL string
	select {
	case status := <-statuses:
		if status.State != channel.ConnectionConnected {
			t.Fatalf("webhook status = %#v", status)
		}
		parsed, err := url.Parse(webhookURL)
		if err != nil || parsed.Path == "" {
			t.Fatalf("registered webhook = %q, %v", webhookURL, err)
		}
		const marker = " via "
		index := strings.LastIndex(status.Detail, marker)
		if index < 0 {
			t.Fatalf("webhook status detail = %q", status.Detail)
		}
		localURL = "http://" + status.Detail[index+len(marker):] + parsed.Path
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook listener")
	}
	allowed = nil
	request, _ := http.NewRequest(http.MethodPost, localURL, strings.NewReader(`{"update_id":1,"message":{"message_id":2,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"blocked"}}`))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("revoked webhook status = %d", response.StatusCode)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errTelegramRuntimeAuthorization) {
			t.Fatalf("webhook Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked webhook listener did not stop")
	}
	if deletes.Load() != 1 {
		t.Fatalf("revoked webhook cleanup calls = %d, want 1", deletes.Load())
	}
	if _, err := http.DefaultClient.Get(localURL); err == nil {
		t.Fatal("revoked webhook listener still accepts connections")
	}
}
