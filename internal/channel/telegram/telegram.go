package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	markdownfmt "github.com/agent0ai/spynel/internal/markdown"
	"github.com/agent0ai/spynel/internal/media"
)

type Bot struct {
	config       config.Telegram
	token        string
	client       *http.Client
	baseURL      string
	report       channel.StatusReporter
	notice       channel.NoticeReporter
	store        *media.Store
	speech       media.Transcriber
	me           telegramUser
	activity     *channel.ActivityIndicator[string]
	identity     *IdentityStore
	log          io.Writer
	allowedUsers func() []string
	revoked      atomic.Bool
	authLost     chan struct{}
	authLostOnce sync.Once
	listen       func(string, string) (net.Listener, error)
}

var errTelegramRuntimeAuthorization = errors.New("Telegram runtime authorization is unavailable: allowed_users has no valid user")

func New(cfg config.Telegram, token string) *Bot {
	return NewWithIdentityStore(cfg, token, "")
}

func NewWithIdentityStore(cfg config.Telegram, token, identityPath string) *Bot {
	timeout := time.Duration(cfg.PollTimeoutSec+10) * time.Second
	if timeout < 20*time.Second {
		timeout = 20 * time.Second
	}
	bot := &Bot{config: cfg, token: token, client: &http.Client{Timeout: timeout}, baseURL: "https://api.telegram.org", identity: NewIdentityStore(identityPath), authLost: make(chan struct{}), listen: net.Listen}
	bot.allowedUsers = func() []string { return cfg.AllowedUsers }
	bot.activity = newTelegramActivity(bot, 4*time.Second)
	return bot
}

func newTelegramActivity(bot *Bot, interval time.Duration) *channel.ActivityIndicator[string] {
	return channel.NewActivityIndicator(interval, func(ctx context.Context, chatID string, active bool) error {
		if !active {
			return nil
		}
		return bot.action(ctx, chatID, "typing")
	})
}

func (b *Bot) Name() string { return "telegram" }

func (b *Bot) SetStatusReporter(report channel.StatusReporter) { b.report = report }

func (b *Bot) SetNoticeReporter(report channel.NoticeReporter) { b.notice = report }

func (b *Bot) SetLogWriter(writer io.Writer) { b.log = writer }

func (b *Bot) SetMedia(store *media.Store, speech media.Transcriber) {
	b.store = store
	b.speech = speech
}

// SetAllowedUsersSource installs the live configuration resolver used at
// startup and immediately before every inbound and outbound provider action.
func (b *Bot) SetAllowedUsersSource(source func() []string) { b.allowedUsers = source }

func (b *Bot) liveAllowedUsers() []string {
	if b.allowedUsers == nil {
		return nil
	}
	return b.allowedUsers()
}

func (b *Bot) ValidateRuntimeAuthorization() error {
	if b.revoked.Load() || !config.HasAllowedTelegramUser(b.liveAllowedUsers()) {
		return errTelegramRuntimeAuthorization
	}
	return nil
}

func (b *Bot) RevokeRuntimeAuthorization() {
	b.revoked.Store(true)
	b.signalAuthorizationLoss()
}

func (b *Bot) signalAuthorizationLoss() {
	b.authLostOnce.Do(func() { close(b.authLost) })
}

func (b *Bot) requireRuntimeAuthorization() error {
	if err := b.ValidateRuntimeAuthorization(); err != nil {
		b.reportStatus(channel.ConnectionError, err.Error())
		b.signalAuthorizationLoss()
		return err
	}
	return nil
}

func (b *Bot) Deliver(ctx context.Context, conversation, eventID, text string) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	var chatID string
	if strings.HasPrefix(conversation, "TG-group-") {
		if b.config.GroupMode == "off" {
			return errors.New("Telegram group delivery is disabled")
		}
		chatID = strings.TrimPrefix(conversation, "TG-group-")
	} else if strings.HasPrefix(conversation, "TG-") {
		chatID = strings.TrimPrefix(conversation, "TG-")
		if !b.identity.AuthorizedPrivate(b.liveAllowedUsers(), chatID) {
			return errors.New("Telegram origin is not in allowed_users")
		}
	} else {
		return errors.New("invalid Telegram conversation origin")
	}
	if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
		return errors.New("invalid Telegram chat identifier")
	}
	_, err := b.sendWithIDs(ctx, chatID, text, 0, false)
	return err
}

func (b *Bot) Run(ctx context.Context, handler channel.Handler) error {
	if b.token == "" {
		err := errors.New("Telegram is enabled but no token is configured")
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	if b.config.Mode == "webhook" && strings.TrimSpace(b.config.WebhookSecret) == "" {
		err := errors.New("Telegram webhook mode requires a verification secret")
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	result, err := b.call(ctx, "getMe", map[string]any{})
	if err != nil {
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	_ = json.Unmarshal(result, &b.me)
	if b.store != nil && b.config.AttachmentMaxAgeHours > 0 {
		if _, err := b.store.CleanupOlderThan(time.Duration(b.config.AttachmentMaxAgeHours) * time.Hour); err != nil {
			return fmt.Errorf("clean Telegram attachments: %w", err)
		}
		go b.cleanupAttachments(ctx)
	}
	if b.config.Mode == "webhook" {
		return b.runWebhook(ctx, handler)
	}
	return b.runPolling(ctx, handler)
}

func (b *Bot) cleanupAttachments(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = b.store.CleanupOlderThan(time.Duration(b.config.AttachmentMaxAgeHours) * time.Hour)
		}
	}
}

func (b *Bot) runPolling(ctx context.Context, handler channel.Handler) error {
	if err := b.post(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}); err != nil {
		return err
	}
	b.reportStatus(channel.ConnectionConnected, "")
	offset := int64(0)
	for {
		updates, err := b.updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.reportStatus(channel.ConnectionError, err.Error())
			if errors.Is(err, errTelegramRuntimeAuthorization) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		b.reportStatus(channel.ConnectionConnected, "")
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			b.processUpdate(ctx, handler, update)
			select {
			case <-b.authLost:
				return errTelegramRuntimeAuthorization
			default:
			}
		}
	}
}

func (b *Bot) runWebhook(ctx context.Context, handler channel.Handler) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	listener, err := b.listen("tcp", b.config.WebhookListen)
	if err != nil {
		return fmt.Errorf("listen for Telegram webhook on %s: %w", b.config.WebhookListen, err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(b.token)))
	path := "/spynel/telegram/" + hash[:16]
	updates := make(chan telegramUpdate, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-updates:
				b.processUpdate(ctx, handler, update)
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(writer http.ResponseWriter, request *http.Request) {
		if err := b.requireRuntimeAuthorization(); err != nil {
			http.Error(writer, "channel authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(request.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(b.config.WebhookSecret)) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 10*1024*1024)
		var update telegramUpdate
		if err := json.NewDecoder(request.Body).Decode(&update); err != nil {
			http.Error(writer, "invalid update", http.StatusBadRequest)
			return
		}
		select {
		case updates <- update:
			writer.WriteHeader(http.StatusOK)
		case <-ctx.Done():
			http.Error(writer, "shutting down", http.StatusServiceUnavailable)
		default:
			http.Error(writer, "update queue full", http.StatusServiceUnavailable)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	serveError := make(chan error, 1)
	go func() { serveError <- server.Serve(listener) }()
	publicURL := strings.TrimRight(b.config.WebhookURL, "/") + path
	payload := map[string]any{"url": publicURL, "allowed_updates": []string{"message"}, "secret_token": b.config.WebhookSecret}
	if err := b.post(ctx, "setWebhook", payload); err != nil {
		_ = server.Close()
		return err
	}
	b.reportStatus(channel.ConnectionConnected, "webhook connected via "+listener.Addr().String())
	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case <-b.authLost:
		runErr = errTelegramRuntimeAuthorization
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownContext)
	// Deleting an already-registered webhook is a teardown-only exception to
	// the normal provider boundary: revocation must block all useful traffic,
	// but it must not strand Telegram delivery at a listener that is gone.
	_, _ = b.callProvider(shutdownContext, "deleteWebhook", map[string]any{"drop_pending_updates": false})
	return runErr
}

func (b *Bot) processUpdate(ctx context.Context, handler channel.Handler, update telegramUpdate) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return
	}
	message := update.Message
	if message == nil || !message.hasContent() || !b.allowed(message.From) {
		return
	}
	if message.Chat.Type == "private" && message.Chat.ID == message.From.ID {
		if err := b.requireRuntimeAuthorization(); err != nil {
			return
		}
		if err := b.identity.RecordVerifiedPrivate(message.From.ID, message.From.ID, message.From.Username); err != nil && b.log != nil {
			_, _ = fmt.Fprintln(b.log, "telegram: persist verified private identity:", err)
		}
	}
	if b.config.WelcomeEnabled && len(message.NewChatMembers) > 0 {
		b.welcome(ctx, message)
	}
	if (strings.TrimSpace(message.Text) != "" || strings.TrimSpace(message.Caption) != "" || message.hasMedia()) && b.groupAllowed(message) {
		b.handle(ctx, handler, message)
	}
}

func (b *Bot) reportStatus(state channel.ConnectionState, detail string) {
	if b.report != nil {
		if (state == channel.ConnectionConnecting || state == channel.ConnectionConnected) && b.ValidateRuntimeAuthorization() != nil {
			state = channel.ConnectionError
			detail = errTelegramRuntimeAuthorization.Error()
		}
		status := channel.ConnectionStatus{Name: b.Name(), State: state, Detail: detail}
		if username := telegramUsername(b.me.Username); username != "" {
			status.Identity = "@" + username
			status.Link = "https://t.me/" + username
		}
		b.report(status)
	}
}

func telegramUsername(value string) string {
	username := strings.TrimPrefix(strings.TrimSpace(value), "@")
	if username == "" || strings.ContainsFunc(username, func(character rune) bool {
		return character != '_' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z')
	}) {
		return ""
	}
	return username
}

func (b *Bot) handle(ctx context.Context, handler channel.Handler, message *telegramMessage) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return
	}
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	var activityMu sync.Mutex
	var finishActivity func()
	var agentOwnedActivity bool
	setActivity := func(active bool) {
		activityMu.Lock()
		if active && finishActivity == nil {
			finishActivity = b.activity.Start(ctx, chatID)
			activityMu.Unlock()
			return
		}
		finish := finishActivity
		if !active {
			finishActivity = nil
		}
		activityMu.Unlock()
		if !active && finish != nil {
			finish()
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			setActivity(false)
			panic(recovered)
		}
	}()
	// Ordinary accepted messages own the visible communication turn from
	// arrival, including attachment preparation and transcription. Slash
	// commands wait for the application activity event so framework-only
	// commands never flash typing while agent-backed commands still activate it.
	rawText := strings.TrimSpace(firstNonempty(message.Text, message.Caption))
	if !strings.HasPrefix(rawText, "/") {
		setActivity(true)
	}
	text, err := b.messageText(ctx, message)
	if err != nil {
		_ = b.send(context.Background(), chatID, channel.ErrorResponse("Spynel attachment error: "+err.Error()), message.MessageID)
		setActivity(false)
		return
	}
	if b.config.NotifyMessages && b.notice != nil {
		if err := b.requireRuntimeAuthorization(); err != nil {
			setActivity(false)
			return
		}
		preview := strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(preview); len(runes) > 120 {
			preview = string(runes[:120]) + "…"
		}
		b.notice(channel.Notice{Channel: b.Name(), Sender: b.sender(message.From), Text: preview})
	}
	emit := func(event core.Event) {
		if event.Kind == core.EventActivity {
			if event.Active {
				activityMu.Lock()
				agentOwnedActivity = true
				activityMu.Unlock()
			}
			setActivity(event.Active)
			return
		}
		if !event.Done || event.Continues || (event.Kind != core.EventFinal && event.Kind != core.EventError) {
			return
		}
		// Terminal events are a fallback cleanup boundary for handlers that do
		// not yet emit the explicit activity-off event. Stop before delivery.
		setActivity(false)
		text := event.Text
		if event.Kind == core.EventFinal && event.FinalText != nil {
			text = *event.FinalText
		}
		if event.Kind == core.EventError {
			text = channel.ErrorResponse(text)
		}
		if text != "" {
			_ = b.send(context.Background(), chatID, text, message.MessageID)
			message.MessageID = 0
		}
		for _, attachment := range event.Attachments {
			if err := b.sendAttachment(context.Background(), chatID, attachment, message.MessageID); err != nil {
				_ = b.send(context.Background(), chatID, channel.ErrorResponse("Spynel attachment delivery error: "+err.Error()), message.MessageID)
			}
			message.MessageID = 0
		}
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		setActivity(false)
		return
	}
	err = handler(ctx, core.Message{
		Channel: b.Name(), Conversation: b.conversationID(message), Sender: b.sender(message.From),
		ReplyTo: telegramReplyTo(message), Text: text, ReceivedAt: time.Unix(message.Date, 0).UTC(),
	}, emit)
	if err != nil {
		_ = b.send(context.Background(), chatID, channel.ErrorResponse(err.Error()), message.MessageID)
		setActivity(false)
		return
	}
	activityMu.Lock()
	owned := agentOwnedActivity
	activityMu.Unlock()
	if !owned {
		// A synchronous handler that neither completed nor transferred activity
		// to an asynchronous main-agent turn cannot retain arrival activity.
		setActivity(false)
	}
}

func telegramReplyTo(message *telegramMessage) string {
	if message == nil || message.ReplyToMessage == nil || message.ReplyToMessage.MessageID <= 0 {
		return ""
	}
	replied := message.ReplyToMessage
	return channel.ReplyReference(strconv.FormatInt(replied.MessageID, 10), firstNonempty(replied.Text, replied.Caption))
}

func (b *Bot) conversationID(message *telegramMessage) string {
	if message.Chat.Type == "group" || message.Chat.Type == "supergroup" {
		return "TG-group-" + strconv.FormatInt(message.Chat.ID, 10)
	}
	return "TG-" + strconv.FormatInt(message.From.ID, 10)
}

func (b *Bot) welcome(ctx context.Context, message *telegramMessage) {
	for _, member := range message.NewChatMembers {
		welcome := b.config.WelcomeMessage
		if welcome == "" {
			welcome = "Welcome, {name}!"
		}
		name := firstNonempty(member.FirstName, b.sender(member))
		_ = b.send(ctx, strconv.FormatInt(message.Chat.ID, 10), strings.ReplaceAll(welcome, "{name}", name), message.MessageID)
	}
}

func (b *Bot) messageText(ctx context.Context, message *telegramMessage) (string, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return "", err
	}
	parts := []string{firstNonempty(message.Text, message.Caption)}
	for _, file := range message.files() {
		attachment, err := b.download(ctx, file)
		if err != nil {
			return "", err
		}
		parts = append(parts, attachment.Token())
		if file.Voice && b.speech == nil {
			parts = append(parts, "[Voice transcription is disabled; inspect the attached audio manually]")
		}
		if file.Voice && b.speech != nil {
			transcript, err := b.speech.Transcribe(ctx, attachment.Path)
			if err != nil {
				parts = append(parts, "[Voice transcription failed — inspect the attached audio manually: "+err.Error()+"]")
				continue
			}
			parts = append(parts, "[Generated voice transcription — may contain errors]\n"+strings.TrimSpace(transcript))
		}
	}
	return joinNonempty(parts), nil
}

func (b *Bot) download(ctx context.Context, file telegramFile) (media.Attachment, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return media.Attachment{}, err
	}
	if b.store == nil {
		return media.Attachment{}, errors.New("attachment storage is not configured")
	}
	result, err := b.call(ctx, "getFile", map[string]any{"file_id": file.FileID})
	if err != nil {
		return media.Attachment{}, err
	}
	var remote struct {
		Path string `json:"file_path"`
	}
	if err := json.Unmarshal(result, &remote); err != nil || remote.Path == "" {
		return media.Attachment{}, errors.New("Telegram returned an invalid attachment path")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.baseURL, "/")+"/file/bot"+b.token+"/"+strings.TrimLeft(remote.Path, "/"), nil)
	if err != nil {
		return media.Attachment{}, err
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		return media.Attachment{}, err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return media.Attachment{}, b.redact(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return media.Attachment{}, fmt.Errorf("Telegram attachment download returned HTTP %d", response.StatusCode)
	}
	return b.store.Save(ctx, file.Name, response.Body)
}

func (b *Bot) allowed(user telegramUser) bool {
	allowedUsers := b.liveAllowedUsers()
	if !config.HasAllowedTelegramUser(allowedUsers) {
		return false
	}
	id := strconv.FormatInt(user.ID, 10)
	username := normalizeUsername(user.Username)
	for _, allowed := range allowedUsers {
		allowed = normalizeAllowedUser(allowed)
		if allowed == id || (username != "" && allowed == username) {
			return true
		}
	}
	return false
}

func (b *Bot) sender(user telegramUser) string {
	if user.Username != "" {
		return "@" + user.Username
	}
	return strconv.FormatInt(user.ID, 10)
}

func (b *Bot) groupAllowed(message *telegramMessage) bool {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return true
	}
	switch b.config.GroupMode {
	case "all":
		return true
	case "off":
		return false
	default:
		username := strings.ToLower(strings.TrimPrefix(b.me.Username, "@"))
		body := strings.ToLower(firstNonempty(message.Text, message.Caption))
		mentioned := username != "" && strings.Contains(body, "@"+username)
		replied := message.ReplyToMessage != nil && message.ReplyToMessage.From.ID == b.me.ID
		return mentioned || replied
	}
}

func (b *Bot) updates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("offset", strconv.FormatInt(offset, 10))
	timeout := b.config.PollTimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	query.Set("timeout", strconv.Itoa(timeout))
	query.Set("allowed_updates", `["message"]`)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint("getUpdates")+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return nil, b.redact(err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool             `json:"ok"`
		Result      []telegramUpdate `json:"result"`
		Description string           `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		return nil, fmt.Errorf("Telegram getUpdates: %s", envelope.Description)
	}
	return envelope.Result, nil
}

func (b *Bot) send(ctx context.Context, chatID, text string, replyTo int64) error {
	_, err := b.sendWithIDs(ctx, chatID, text, replyTo, false)
	return err
}

func (b *Bot) sendWithIDs(ctx context.Context, chatID, text string, replyTo int64, requireID bool) ([]string, error) {
	var ids []string
	for _, chunk := range telegramChunks(text, 4096) {
		payload := map[string]any{"chat_id": chatID, "text": chunk, "parse_mode": "HTML", "disable_web_page_preview": true}
		if replyTo > 0 {
			payload["reply_parameters"] = map[string]any{"message_id": replyTo}
		}
		result, err := b.call(ctx, "sendMessage", payload)
		if err != nil {
			return nil, err
		}
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		if json.Unmarshal(result, &sent) != nil || sent.MessageID <= 0 {
			if requireID {
				return nil, errors.New("Telegram sendMessage returned no message identifier")
			}
			continue
		}
		ids = append(ids, strconv.FormatInt(sent.MessageID, 10))
		replyTo = 0
	}
	return ids, nil
}

func (b *Bot) sendAttachment(ctx context.Context, chatID string, attachment core.OutboundAttachment, replyTo int64) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	file, err := media.OpenOutbound(attachment)
	if err != nil {
		return err
	}
	defer file.Close()
	method, field := "sendDocument", "document"
	if attachment.Kind == "photo" {
		method, field = "sendPhoto", "photo"
	}
	reader, pipe := io.Pipe()
	writer := multipart.NewWriter(pipe)
	contentType := writer.FormDataContentType()
	go func() {
		writeErr := writer.WriteField("chat_id", chatID)
		if writeErr == nil && replyTo > 0 {
			writeErr = writer.WriteField("reply_parameters", fmt.Sprintf(`{"message_id":%d}`, replyTo))
		}
		if writeErr == nil {
			var part io.Writer
			part, writeErr = writer.CreateFormFile(field, filepath.Base(attachment.Name))
			if writeErr == nil {
				_, writeErr = io.Copy(part, file)
			}
		}
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = pipe.CloseWithError(writeErr)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", contentType)
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return b.redact(err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		return fmt.Errorf("Telegram %s: %s", method, envelope.Description)
	}
	return nil
}

func telegramChunks(markdownText string, limit int) []string {
	if markdownText == "" || limit <= 0 {
		return nil
	}
	queue := split(markdownText, max(1, limit/2))
	var chunks []string
	for len(queue) > 0 {
		raw := queue[0]
		queue = queue[1:]
		formatted := markdownfmt.TelegramHTML(raw)
		if len([]rune(formatted)) <= limit || len([]rune(raw)) <= 1 {
			chunks = append(chunks, formatted)
			continue
		}
		half := max(1, len([]rune(raw))/2)
		parts := split(raw, half)
		queue = append(parts, queue...)
	}
	return chunks
}

func (b *Bot) action(ctx context.Context, chatID, action string) error {
	return b.post(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action})
}

func (b *Bot) post(ctx context.Context, method string, payload any) error {
	_, err := b.call(ctx, method, payload)
	return err
}

func (b *Bot) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return nil, err
	}
	return b.callProvider(ctx, method, payload)
}

func (b *Bot) callProvider(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := b.client.Do(request)
	if err != nil {
		return nil, b.redact(err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		return nil, fmt.Errorf("Telegram %s: %s", method, envelope.Description)
	}
	return envelope.Result, nil
}

func (b *Bot) endpoint(method string) string {
	return strings.TrimRight(b.baseURL, "/") + "/bot" + b.token + "/" + method
}

func (b *Bot) redact(err error) error {
	return errors.New(strings.ReplaceAll(err.Error(), b.token, "<redacted>"))
}

func split(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	var chunks []string
	for len(runes) > limit {
		cut := limit
		for cut > limit/2 && runes[cut] != '\n' {
			cut--
		}
		if cut <= limit/2 {
			cut = limit
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID      int64             `json:"message_id"`
	From           telegramUser      `json:"from"`
	Chat           telegramChat      `json:"chat"`
	Date           int64             `json:"date"`
	Text           string            `json:"text"`
	Caption        string            `json:"caption"`
	Document       *telegramDocument `json:"document"`
	Photo          []telegramPhoto   `json:"photo"`
	Video          *telegramMedia    `json:"video"`
	Audio          *telegramMedia    `json:"audio"`
	Voice          *telegramMedia    `json:"voice"`
	ReplyToMessage *telegramMessage  `json:"reply_to_message"`
	NewChatMembers []telegramUser    `json:"new_chat_members"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramDocument struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
}

type telegramMedia struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
}

type telegramPhoto struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
}

type telegramFile struct {
	FileID string
	Name   string
	Voice  bool
}

func (m *telegramMessage) hasMedia() bool {
	return m.Document != nil || len(m.Photo) > 0 || m.Video != nil || m.Audio != nil || m.Voice != nil
}

func (m *telegramMessage) hasContent() bool {
	return strings.TrimSpace(m.Text) != "" || strings.TrimSpace(m.Caption) != "" || m.hasMedia() || len(m.NewChatMembers) > 0
}

func (m *telegramMessage) files() []telegramFile {
	var files []telegramFile
	if m.Document != nil {
		files = append(files, telegramFile{FileID: m.Document.FileID, Name: firstNonempty(m.Document.FileName, "document-"+m.Document.FileUniqueID)})
	}
	if len(m.Photo) > 0 {
		photo := m.Photo[len(m.Photo)-1]
		files = append(files, telegramFile{FileID: photo.FileID, Name: "photo-" + photo.FileUniqueID + ".jpg"})
	}
	if m.Video != nil {
		files = append(files, telegramFile{FileID: m.Video.FileID, Name: firstNonempty(m.Video.FileName, "video-"+m.Video.FileUniqueID+".mp4")})
	}
	if m.Audio != nil {
		files = append(files, telegramFile{FileID: m.Audio.FileID, Name: firstNonempty(m.Audio.FileName, "audio-"+m.Audio.FileUniqueID+".mp3")})
	}
	if m.Voice != nil {
		files = append(files, telegramFile{FileID: m.Voice.FileID, Name: "voice-" + m.Voice.FileUniqueID + ".ogg", Voice: true})
	}
	return files
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func joinNonempty(values []string) string {
	result := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return strings.Join(result, "\n\n")
}
