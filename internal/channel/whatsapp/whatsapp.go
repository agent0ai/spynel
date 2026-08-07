package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/core"
	markdownfmt "github.com/frdel/spynel/internal/markdown"
	"github.com/frdel/spynel/internal/media"
)

type Client struct {
	config   config.WhatsApp
	dbPath   string
	handler  channel.Handler
	client   *whatsmeow.Client
	ctx      context.Context
	report   channel.StatusReporter
	pairing  channel.PairingReporter
	log      io.Writer
	store    *media.Store
	speech   media.Transcriber
	incoming chan incomingMessage
	download func(context.Context, whatsmeow.DownloadableMessage, *os.File) error
	presence func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error
	activity *channel.ActivityIndicator[types.JID]

	mu      sync.Mutex
	sentIDs map[types.MessageID]time.Time
}

type incomingMessage struct {
	received time.Time
	chat     types.JID
	sender   string
	message  *waE2E.Message
	id       types.MessageID
}

var nonNumber = regexp.MustCompile(`\D+`)

func New(cfg config.WhatsApp, database string) *Client {
	client := &Client{config: cfg, dbPath: database, sentIDs: map[types.MessageID]time.Time{}, log: os.Stderr, incoming: make(chan incomingMessage, 64)}
	client.activity = newWhatsAppActivity(client, 4*time.Second)
	return client
}

func newWhatsAppActivity(client *Client, interval time.Duration) *channel.ActivityIndicator[types.JID] {
	return channel.NewActivityIndicator(interval, func(ctx context.Context, chat types.JID, active bool) error {
		state := types.ChatPresencePaused
		if active {
			state = types.ChatPresenceComposing
		}
		return client.sendChatPresence(ctx, chat, state)
	})
}

func (c *Client) Name() string { return "whatsapp" }

func (c *Client) SetStatusReporter(report channel.StatusReporter) { c.report = report }

func (c *Client) SetPairingReporter(report channel.PairingReporter) { c.pairing = report }

func (c *Client) SetLogWriter(writer io.Writer) {
	if writer != nil {
		c.log = writer
	}
}

func (c *Client) SetMedia(store *media.Store, speech media.Transcriber) {
	c.store = store
	c.speech = speech
}

func (c *Client) Run(ctx context.Context, handler channel.Handler) error {
	c.handler = handler
	c.ctx = ctx
	go c.messageWorker(ctx)
	if err := os.MkdirAll(filepath.Dir(c.dbPath), 0o700); err != nil {
		return err
	}
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+filepath.ToSlash(c.dbPath)+"?_foreign_keys=on", nil)
	if err != nil {
		return fmt.Errorf("open WhatsApp session store: %w", err)
	}
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return err
	}
	c.client = whatsmeow.NewClient(device, nil)
	c.client.AddEventHandler(c.onEvent)
	if c.client.Store.ID == nil {
		qrChannel, err := c.client.GetQRChannel(ctx)
		if err != nil {
			return err
		}
		if err := c.client.Connect(); err != nil {
			return err
		}
		go func() {
			for event := range qrChannel {
				if event.Event == "code" {
					var rendered strings.Builder
					qrterminal.GenerateHalfBlock(event.Code, qrterminal.L, &rendered)
					c.reportPairing(channel.PairingEvent{Name: c.Name(), State: "code", Code: event.Code, Rendered: strings.TrimSpace(rendered.String()), Detail: "Scan in WhatsApp → Linked devices"})
					fmt.Fprintln(c.log, "WhatsApp is waiting for QR pairing; open /whatsapp in the TUI")
				} else {
					fmt.Fprintln(c.log, "WhatsApp pairing:", event.Event)
					c.reportPairing(channel.PairingEvent{Name: c.Name(), State: event.Event, Detail: "WhatsApp pairing: " + event.Event})
				}
			}
		}()
	} else {
		if err := c.client.Connect(); err != nil {
			return err
		}
		c.reportStatus(channel.ConnectionConnected, "")
	}
	defer c.client.Disconnect()
	interval := time.Duration(c.config.PollIntervalSec) * time.Second
	if interval < 2*time.Second {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.client.IsConnected() {
				c.reportStatus(channel.ConnectionConnected, "")
			} else {
				c.reportStatus(channel.ConnectionError, "disconnected")
			}
		}
	}
}

func (c *Client) onEvent(raw any) {
	switch event := raw.(type) {
	case *events.Connected:
		c.reportStatus(channel.ConnectionConnected, "")
		c.reportPairing(channel.PairingEvent{Name: c.Name(), State: "connected", Detail: "WhatsApp connected"})
		return
	case *events.Disconnected:
		c.reportStatus(channel.ConnectionError, "disconnected")
		return
	case *events.LoggedOut:
		c.reportStatus(channel.ConnectionError, fmt.Sprintf("logged out (%d)", event.Reason))
		return
	case *events.StreamError:
		c.reportStatus(channel.ConnectionError, "stream error: "+event.Code)
		return
	}
	event, ok := raw.(*events.Message)
	if !ok || event.Message == nil || c.wasSent(event.Info.ID) {
		return
	}
	if event.Info.IsGroup {
		if !c.config.AllowGroups || !c.groupAddressed(event.Message) {
			return
		}
	}
	if c.config.Mode == "self-chat" {
		if !event.Info.IsFromMe || event.Info.IsGroup {
			return
		}
		chat := event.Info.Chat.ToNonAD()
		phoneID := c.client.Store.ID.ToNonAD()
		lid := c.client.Store.LID.ToNonAD()
		if chat != phoneID && (lid.IsEmpty() || chat != lid) {
			return
		}
	} else if event.Info.IsFromMe {
		return
	}
	sender := event.Info.Sender.User
	if !event.Info.SenderAlt.IsEmpty() {
		sender = event.Info.SenderAlt.User
	}
	if !c.allowed(event.Info.Sender.User, event.Info.SenderAlt.User) {
		return
	}
	if strings.TrimSpace(messageText(event)) == "" && !hasMedia(event.Message) {
		return
	}
	chat := event.Info.Chat
	select {
	case c.incoming <- incomingMessage{received: event.Info.Timestamp, chat: chat, sender: sender, message: event.Message, id: event.Info.ID}:
	default:
		fmt.Fprintln(c.log, "WhatsApp incoming queue is full; dropping message", event.Info.ID)
	}
}

func (c *Client) groupAddressed(message *waE2E.Message) bool {
	contextInfo := messageContext(message)
	if contextInfo == nil || c.client == nil || c.client.Store == nil || c.client.Store.ID == nil {
		return false
	}
	identities := map[string]bool{c.client.Store.ID.User: true}
	if !c.client.Store.LID.IsEmpty() {
		identities[c.client.Store.LID.User] = true
	}
	for _, raw := range contextInfo.GetMentionedJID() {
		if jid, err := types.ParseJID(raw); err == nil && identities[jid.User] {
			return true
		}
	}
	if contextInfo.GetStanzaID() != "" {
		if jid, err := types.ParseJID(contextInfo.GetParticipant()); err == nil && identities[jid.User] {
			return true
		}
	}
	return false
}

func messageContext(message *waE2E.Message) *waE2E.ContextInfo {
	if value := message.GetExtendedTextMessage(); value != nil {
		return value.GetContextInfo()
	}
	if value := message.GetImageMessage(); value != nil {
		return value.GetContextInfo()
	}
	if value := message.GetVideoMessage(); value != nil {
		return value.GetContextInfo()
	}
	if value := message.GetAudioMessage(); value != nil {
		return value.GetContextInfo()
	}
	if value := message.GetDocumentMessage(); value != nil {
		return value.GetContextInfo()
	}
	return nil
}

func (c *Client) reportPairing(event channel.PairingEvent) {
	if c.pairing != nil {
		c.pairing(event)
	}
}

func (c *Client) messageWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case incoming := <-c.incoming:
			finishActivity := c.activity.Start(ctx, incoming.chat)
			text, err := c.prepareMessage(ctx, incoming)
			if err != nil {
				_ = c.send(ctx, incoming.chat, "Spynel attachment error: "+err.Error())
				finishActivity()
				continue
			}
			c.handleWithActivity(incoming.received, incoming.chat, incoming.sender, text, finishActivity)
		}
	}
}

func (c *Client) prepareMessage(ctx context.Context, incoming incomingMessage) (string, error) {
	parts := []string{messageBody(incoming.message)}
	downloadable, name, voice, size := downloadableMedia(incoming.message, incoming.id)
	if downloadable != nil {
		if c.store == nil {
			return "", errors.New("attachment storage is not configured")
		}
		if c.store.MaxBytes > 0 && size > uint64(c.store.MaxBytes) {
			return "", fmt.Errorf("attachment exceeds the %d byte limit", c.store.MaxBytes)
		}
		attachment, err := c.store.Create(ctx, name, func(file *os.File) error {
			if c.download != nil {
				return c.download(ctx, downloadable, file)
			}
			return c.client.DownloadToFile(ctx, downloadable, file)
		})
		if err != nil {
			return "", err
		}
		parts = append(parts, attachment.Token())
		if voice && c.speech == nil {
			parts = append(parts, "[Voice transcription is disabled; inspect the attached audio manually]")
		}
		if voice && c.speech != nil {
			transcript, err := c.speech.Transcribe(ctx, attachment.Path)
			if err != nil {
				parts = append(parts, "[Voice transcription failed — inspect the attached audio manually: "+err.Error()+"]")
				return joinParts(parts), nil
			}
			parts = append(parts, "[Generated voice transcription — may contain errors]\n"+strings.TrimSpace(transcript))
		}
	}
	return joinParts(parts), nil
}

func (c *Client) reportStatus(state channel.ConnectionState, detail string) {
	if c.report != nil {
		c.report(channel.ConnectionStatus{Name: c.Name(), State: state, Detail: detail})
	}
}

func (c *Client) handle(received time.Time, chat types.JID, sender, text string) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	c.handleWithActivity(received, chat, sender, text, c.activity.Start(ctx, chat))
}

func (c *Client) handleWithActivity(received time.Time, chat types.JID, sender, text string, finishActivity func()) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	emit := func(event core.Event) {
		if !event.Done {
			return
		}
		if event.Text != "" {
			_ = c.send(ctx, chat, event.Text)
		}
		finishActivity()
	}
	err := c.handler(ctx, core.Message{
		Channel: c.Name(), Conversation: whatsappConversation(chat, sender), Sender: sender,
		Text:       text,
		ReceivedAt: received.UTC(),
	}, emit)
	if err != nil {
		_ = c.send(ctx, chat, "Spynel error: "+err.Error())
		finishActivity()
	}
}

func (c *Client) sendChatPresence(ctx context.Context, chat types.JID, state types.ChatPresence) error {
	if c.presence != nil {
		return c.presence(ctx, chat, state, types.ChatPresenceMediaText)
	}
	if c.client == nil {
		return errors.New("WhatsApp is not connected")
	}
	return c.client.SendChatPresence(ctx, chat, state, types.ChatPresenceMediaText)
}

func whatsappConversation(chat types.JID, sender string) string {
	chat = chat.ToNonAD()
	if chat.Server == types.GroupServer {
		return "WA-group-" + chat.User
	}
	number := nonNumber.ReplaceAllString(sender, "")
	if number == "" {
		number = nonNumber.ReplaceAllString(chat.User, "")
	}
	return "WA-" + number
}

func (c *Client) send(ctx context.Context, chat types.JID, text string) error {
	if c.client == nil {
		return errors.New("WhatsApp is not connected")
	}
	for _, chunk := range split(markdownfmt.WhatsApp(text), 60000) {
		response, err := c.client.SendMessage(ctx, chat, &waE2E.Message{Conversation: proto.String(chunk)})
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.sentIDs[response.ID] = time.Now()
		c.cleanupSentLocked()
		c.mu.Unlock()
	}
	return nil
}

func (c *Client) wasSent(id types.MessageID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sentIDs[id]
	if ok {
		delete(c.sentIDs, id)
	}
	c.cleanupSentLocked()
	return ok
}

func (c *Client) cleanupSentLocked() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, sentAt := range c.sentIDs {
		if sentAt.Before(cutoff) {
			delete(c.sentIDs, id)
		}
	}
}

func (c *Client) allowed(senders ...string) bool {
	if len(c.config.AllowedNumbers) == 0 {
		return true
	}
	for _, sender := range senders {
		number := nonNumber.ReplaceAllString(sender, "")
		for _, allowed := range c.config.AllowedNumbers {
			if number != "" && number == nonNumber.ReplaceAllString(allowed, "") {
				return true
			}
		}
	}
	return false
}

func messageText(event *events.Message) string {
	return messageBody(event.Message)
}

func messageBody(message *waE2E.Message) string {
	if text := message.GetConversation(); text != "" {
		return text
	}
	if extended := message.GetExtendedTextMessage(); extended != nil {
		return extended.GetText()
	}
	if image := message.GetImageMessage(); image != nil {
		return image.GetCaption()
	}
	if video := message.GetVideoMessage(); video != nil {
		return video.GetCaption()
	}
	if document := message.GetDocumentMessage(); document != nil {
		if caption := document.GetCaption(); caption != "" {
			return caption
		}
		return document.GetFileName()
	}
	return ""
}

func hasMedia(message *waE2E.Message) bool {
	return message.GetImageMessage() != nil || message.GetVideoMessage() != nil || message.GetAudioMessage() != nil || message.GetDocumentMessage() != nil || message.GetStickerMessage() != nil
}

func downloadableMedia(message *waE2E.Message, id types.MessageID) (whatsmeow.DownloadableMessage, string, bool, uint64) {
	if document := message.GetDocumentMessage(); document != nil {
		return document, firstName(document.GetFileName(), "document-"+string(id)+mediaExtension(document.GetMimetype())), false, document.GetFileLength()
	}
	if image := message.GetImageMessage(); image != nil {
		return image, "image-" + string(id) + mediaExtension(image.GetMimetype()), false, image.GetFileLength()
	}
	if video := message.GetVideoMessage(); video != nil {
		return video, "video-" + string(id) + mediaExtension(video.GetMimetype()), false, video.GetFileLength()
	}
	if audio := message.GetAudioMessage(); audio != nil {
		return audio, "audio-" + string(id) + mediaExtension(audio.GetMimetype()), audio.GetPTT(), audio.GetFileLength()
	}
	if sticker := message.GetStickerMessage(); sticker != nil {
		return sticker, "sticker-" + string(id) + mediaExtension(sticker.GetMimetype()), false, sticker.GetFileLength()
	}
	return nil, "", false, 0
}

func mediaExtension(mimeType string) string {
	extensions, err := mime.ExtensionsByType(strings.TrimSpace(mimeType))
	if err == nil && len(extensions) > 0 {
		return extensions[0]
	}
	return ".bin"
}

func firstName(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "attachment"
}

func joinParts(parts []string) string {
	result := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return strings.Join(result, "\n\n")
}

func split(text string, limit int) []string {
	runes := []rune(text)
	var chunks []string
	for len(runes) > limit {
		chunks = append(chunks, string(runes[:limit]))
		runes = runes[limit:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
