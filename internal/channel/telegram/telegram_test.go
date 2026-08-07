package telegram

import (
	"context"
	"encoding/json"
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
	bot := New(config.Telegram{}, "test")
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

	bot := New(config.Telegram{}, "test")
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

func TestTelegramConnectionStatusRejectsUnsafeBotUsername(t *testing.T) {
	bot := New(config.Telegram{}, "test")
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
	if err := bot.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "whitelist is empty") {
		t.Fatalf("Run() accepted an empty whitelist: %v", err)
	}
	if got.State != channel.ConnectionError || !strings.Contains(got.Detail, "whitelist is empty") {
		t.Fatalf("connection status = %#v", got)
	}
	if bot.allowed(telegramUser{ID: 7, Username: "trusted"}) {
		t.Fatal("empty whitelist accepted a Telegram user")
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
	bot := New(config.Telegram{}, "test")
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

func TestTelegramTypingRefreshesThroughVoiceTranscriptionAndAgentTurn(t *testing.T) {
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
	bot := New(config.Telegram{}, "test")
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
			close(agentEntered)
			<-agentRelease
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

func TestTelegramTypingContinuesUntilFinalDeliveryCompletes(t *testing.T) {
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

	bot := New(config.Telegram{}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventStatus, Done: true})
			emit(core.Event{Kind: core.EventFinal, Text: "intermediate", Done: true, Continues: true})
			emit(core.Event{Kind: core.EventFinal, Text: "complete", Done: true})
			return nil
		}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	}()
	waitClosed(t, deliveryEntered, "Telegram final delivery")
	beforeDeliveryRefresh := actions.Load()
	waitAtomicCount(t, &actions, beforeDeliveryRefresh+1, "Telegram typing during final delivery")
	close(deliveryRelease)
	waitClosed(t, done, "Telegram final delivery completion")
	stoppedAt := actions.Load()
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
