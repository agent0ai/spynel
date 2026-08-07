package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	markdownfmt "github.com/frdel/spynel/internal/markdown"
	"github.com/frdel/spynel/internal/media"
)

type Bot struct {
	config   config.Telegram
	token    string
	client   *http.Client
	baseURL  string
	report   channel.StatusReporter
	notice   channel.NoticeReporter
	store    *media.Store
	speech   media.Transcriber
	me       telegramUser
	activity *channel.ActivityIndicator[string]
}

func New(cfg config.Telegram, token string) *Bot {
	timeout := time.Duration(cfg.PollTimeoutSec+10) * time.Second
	if timeout < 20*time.Second {
		timeout = 20 * time.Second
	}
	bot := &Bot{config: cfg, token: token, client: &http.Client{Timeout: timeout}, baseURL: "https://api.telegram.org"}
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

func (b *Bot) SetMedia(store *media.Store, speech media.Transcriber) {
	b.store = store
	b.speech = speech
}

func (b *Bot) Run(ctx context.Context, handler channel.Handler) error {
	if b.token == "" {
		err := errors.New("Telegram is enabled but no token is configured")
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	if !hasAllowedUser(b.config.AllowedUsers) {
		err := errors.New("Telegram is enabled but its allowed-users whitelist is empty")
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

func hasAllowedUser(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
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
		}
	}
}

func (b *Bot) runWebhook(ctx context.Context, handler channel.Handler) error {
	listener, err := net.Listen("tcp", b.config.WebhookListen)
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
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if secret := b.config.WebhookSecret; secret != "" && subtle.ConstantTimeCompare([]byte(request.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(secret)) != 1 {
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
	payload := map[string]any{"url": publicURL, "allowed_updates": []string{"message"}}
	if b.config.WebhookSecret != "" {
		payload["secret_token"] = b.config.WebhookSecret
	}
	if err := b.post(ctx, "setWebhook", payload); err != nil {
		_ = server.Close()
		return err
	}
	b.reportStatus(channel.ConnectionConnected, "webhook "+publicURL+" via "+listener.Addr().String())
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		_, _ = b.call(shutdownContext, "deleteWebhook", map[string]any{"drop_pending_updates": false})
		return ctx.Err()
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (b *Bot) processUpdate(ctx context.Context, handler channel.Handler, update telegramUpdate) {
	message := update.Message
	if message == nil || !message.hasContent() || !b.allowed(message.From) {
		return
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
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	finishActivity := b.activity.Start(ctx, chatID)
	text, err := b.messageText(ctx, message)
	if err != nil {
		_ = b.send(context.Background(), chatID, "Spynel attachment error: "+err.Error(), message.MessageID)
		finishActivity()
		return
	}
	if b.config.NotifyMessages && b.notice != nil {
		preview := strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(preview); len(runes) > 120 {
			preview = string(runes[:120]) + "…"
		}
		b.notice(channel.Notice{Channel: b.Name(), Sender: b.sender(message.From), Text: preview})
	}
	emit := func(event core.Event) {
		if !event.Done {
			return
		}
		text := event.Text
		if text == "" && event.Kind == core.EventError {
			text = "The harness turn failed."
		}
		if text != "" {
			_ = b.send(context.Background(), chatID, text, message.MessageID)
		}
		finishActivity()
	}
	err = handler(ctx, core.Message{
		Channel: b.Name(), Conversation: b.conversationID(message), Sender: b.sender(message.From),
		Text:       text,
		ReceivedAt: time.Unix(message.Date, 0).UTC(),
	}, emit)
	if err != nil {
		_ = b.send(context.Background(), chatID, "Spynel error: "+err.Error(), message.MessageID)
		finishActivity()
	}
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
	if len(b.config.AllowedUsers) == 0 {
		return false
	}
	id := strconv.FormatInt(user.ID, 10)
	username := strings.ToLower(strings.TrimPrefix(user.Username, "@"))
	for _, allowed := range b.config.AllowedUsers {
		allowed = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(allowed, "@")))
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
	for _, chunk := range telegramChunks(text, 4096) {
		payload := map[string]any{"chat_id": chatID, "text": chunk, "parse_mode": "HTML", "disable_web_page_preview": true}
		if replyTo > 0 {
			payload["reply_parameters"] = map[string]any{"message_id": replyTo}
		}
		if err := b.post(ctx, "sendMessage", payload); err != nil {
			return err
		}
		replyTo = 0
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
