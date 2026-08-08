package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"go.mau.fi/whatsmeow"
	waCompanionReg "go.mau.fi/whatsmeow/proto/waCompanionReg"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	markdownfmt "github.com/agent0ai/spynel/internal/markdown"
	"github.com/agent0ai/spynel/internal/media"
)

type Client struct {
	config    config.WhatsApp
	dbPath    string
	handler   channel.Handler
	client    *whatsmeow.Client
	ctx       context.Context
	report    channel.StatusReporter
	pairing   channel.PairingReporter
	log       io.Writer
	store     *media.Store
	speech    media.Transcriber
	incoming  chan incomingMessage
	download  func(context.Context, whatsmeow.DownloadableMessage, *os.File) error
	upload    func(context.Context, io.Reader, io.ReadWriteSeeker, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	deliver   func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error)
	deliverID func(context.Context, types.JID, *waE2E.Message, types.MessageID) (whatsmeow.SendResponse, error)
	presence  func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error
	activity  *channel.ActivityIndicator[types.JID]

	mu      sync.Mutex
	sentIDs map[types.MessageID]time.Time

	pairingMu     sync.RWMutex
	pairingActive bool
	pairingReady  bool
	pairingQR     string
	pairingRetry  chan struct{}
	pairingDelay  time.Duration
	pairPhone     func(context.Context, string) (string, error)
}

type incomingMessage struct {
	received time.Time
	chat     types.JID
	sender   string
	message  *waE2E.Message
	id       types.MessageID
}

const whatsAppDeviceName = "Spynel"

func New(cfg config.WhatsApp, database string) *Client {
	client := &Client{
		config: cfg, dbPath: database, sentIDs: map[types.MessageID]time.Time{}, log: os.Stderr,
		incoming: make(chan incomingMessage, 64), pairingRetry: make(chan struct{}, 1), pairingDelay: 2 * time.Second,
	}
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

func (c *Client) Deliver(ctx context.Context, conversation, eventID, text string) (channel.DeliveryReceipt, error) {
	var chat types.JID
	if strings.HasPrefix(conversation, "WA-group-") {
		if !c.config.AllowGroups {
			return channel.DeliveryReceipt{}, errors.New("WhatsApp group delivery is disabled")
		}
		chat = types.NewJID(strings.TrimPrefix(conversation, "WA-group-"), types.GroupServer)
	} else if strings.HasPrefix(conversation, "WA-") {
		number := config.NormalizeWhatsAppNumber(strings.TrimPrefix(conversation, "WA-"))
		authorized := false
		for _, allowed := range c.config.AllowedNumbers {
			authorized = authorized || config.NormalizeWhatsAppNumber(allowed) == number
		}
		if number == "" || !authorized {
			return channel.DeliveryReceipt{}, errors.New("WhatsApp origin is not in allowed_numbers")
		}
		chat = types.NewJID(number, types.DefaultUserServer)
	} else {
		return channel.DeliveryReceipt{}, errors.New("invalid WhatsApp conversation origin")
	}
	ids, err := c.sendEvent(ctx, chat, eventID, text)
	return channel.DeliveryReceipt{MessageIDs: ids}, err
}

func stableWhatsAppMessageID(eventID string, index int) types.MessageID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", eventID, index)))
	return types.MessageID("3EB0" + strings.ToUpper(hex.EncodeToString(sum[:9])))
}

func (c *Client) sendEvent(ctx context.Context, chat types.JID, eventID, text string) ([]string, error) {
	if c.client == nil && c.deliver == nil && c.deliverID == nil {
		return nil, errors.New("WhatsApp is not connected")
	}
	var ids []string
	for index, chunk := range split(markdownfmt.WhatsApp(text), 60000) {
		id := stableWhatsAppMessageID(eventID, index)
		var response whatsmeow.SendResponse
		var err error
		if c.deliverID != nil {
			response, err = c.deliverID(ctx, chat, &waE2E.Message{Conversation: proto.String(chunk)}, id)
		} else if c.deliver != nil {
			response, err = c.deliver(ctx, chat, &waE2E.Message{Conversation: proto.String(chunk)})
		} else {
			response, err = c.client.SendMessage(ctx, chat, &waE2E.Message{Conversation: proto.String(chunk)}, whatsmeow.SendRequestExtra{ID: id})
		}
		if err != nil {
			return nil, err
		}
		nativeID := response.ID
		if nativeID == "" {
			nativeID = id
		}
		c.recordSent(nativeID)
		ids = append(ids, string(nativeID))
	}
	return ids, nil
}

func (c *Client) Run(ctx context.Context, handler channel.Handler) error {
	if !config.HasAllowedWhatsAppNumber(c.config.AllowedNumbers) {
		err := errors.New("WhatsApp is enabled but its allowed-numbers whitelist is empty")
		c.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
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
	configurePairingIdentity(c.client)
	c.pairingMu.Lock()
	c.pairPhone = func(ctx context.Context, phone string) (string, error) {
		return c.client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	}
	c.pairingMu.Unlock()
	c.upload = func(ctx context.Context, source io.Reader, temporary io.ReadWriteSeeker, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return c.client.UploadReader(ctx, source, temporary, mediaType)
	}
	c.deliver = func(ctx context.Context, chat types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		return c.client.SendMessage(ctx, chat, message)
	}
	c.client.AddEventHandler(c.onEvent)
	if c.client.Store.ID == nil {
		if err := c.runPairing(ctx); err != nil {
			return err
		}
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
			if c.client.IsConnected() && c.isPaired() {
				c.reportStatus(channel.ConnectionConnected, "")
			} else {
				c.reportStatus(channel.ConnectionError, "disconnected")
			}
		}
	}
}

// configurePairingIdentity sets the companion metadata that WhatsApp records
// when a new device is linked. The phone-code endpoint has separate server-
// validated display-name restrictions and is intentionally configured below.
func configurePairingIdentity(client *whatsmeow.Client) {
	store.DeviceProps.Os = proto.String(whatsAppDeviceName)
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	client.QRClientType = whatsmeow.PairClientElectron
}

// RetryPairing replaces the current unpaired websocket and its expired QR
// codes. The signal is buffered so a retry pressed while the session is
// winding down is still observed by the terminal-state wait.
func (c *Client) RetryPairing() error {
	c.pairingMu.RLock()
	active := c.pairingActive
	c.pairingMu.RUnlock()
	if !active {
		return errors.New("WhatsApp is not waiting for pairing")
	}
	select {
	case c.pairingRetry <- struct{}{}:
	default:
	}
	return nil
}

// PairPhone generates WhatsApp's official eight-character alternative to QR
// scanning for the active pairing websocket.
func (c *Client) PairPhone(ctx context.Context, phone string) (string, error) {
	c.pairingMu.RLock()
	active, ready, rendered, pair := c.pairingActive, c.pairingReady, c.pairingQR, c.pairPhone
	c.pairingMu.RUnlock()
	if !active {
		return "", errors.New("WhatsApp is not waiting for pairing")
	}
	if !ready || pair == nil {
		return "", errors.New("WhatsApp pairing is not ready yet; wait for Show QR, then try again")
	}
	code, err := pair(ctx, phone)
	if err != nil {
		return "", err
	}
	c.reportPairing(channel.PairingEvent{Name: c.Name(), State: "phone-code", Code: code, Rendered: rendered, Detail: "Enter this code in WhatsApp: " + code})
	return code, nil
}

func (c *Client) runPairing(ctx context.Context) error {
	c.setPairingState(true, false)
	defer c.setPairingState(false, false)
	for c.client.Store.ID == nil {
		c.reportStatus(channel.ConnectionConnecting, "waiting for pairing")
		c.reportPairing(channel.PairingEvent{Name: c.Name(), State: "starting", Detail: "Starting a fresh WhatsApp pairing session…"})
		qrChannel, err := c.client.GetQRChannel(ctx)
		if err != nil {
			if waitErr := c.waitForPairingRestart(ctx, "error", "WhatsApp pairing could not start: "+err.Error()); waitErr != nil {
				return waitErr
			}
			continue
		}
		if err := c.client.Connect(); err != nil {
			if waitErr := c.waitForPairingRestart(ctx, "error", "WhatsApp pairing could not connect: "+err.Error()); waitErr != nil {
				return waitErr
			}
			continue
		}

		restart := false
		for !restart {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-c.pairingRetry:
				c.setPairingState(true, false)
				c.client.Disconnect()
				restart = true
			case event, ok := <-qrChannel:
				if !ok {
					if err := c.waitForPairingRestart(ctx, "error", "WhatsApp pairing ended before the device was linked"); err != nil {
						return err
					}
					restart = true
					continue
				}
				switch event.Event {
				case whatsmeow.QRChannelEventCode:
					var rendered strings.Builder
					qrterminal.GenerateHalfBlock(event.Code, qrterminal.L, &rendered)
					qr := strings.TrimSpace(rendered.String())
					c.setPairingQR(qr)
					c.reportPairing(channel.PairingEvent{Name: c.Name(), State: "code", Code: event.Code, Rendered: qr, Detail: "WhatsApp is ready to pair"})
					fmt.Fprintln(c.log, "WhatsApp is waiting for pairing; open /whatsapp in the TUI")
				case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
					c.setPairingState(true, false)
					c.reportPairing(channel.PairingEvent{Name: c.Name(), State: event.Event, Detail: "Confirm WhatsApp pairing on your phone"})
				case whatsmeow.QRChannelEventError:
					detail := "WhatsApp pairing failed"
					if event.Error != nil {
						detail += ": " + event.Error.Error()
					}
					if err := c.waitForPairingRestart(ctx, "error", detail); err != nil {
						return err
					}
					restart = true
				case "success":
					c.setPairingState(true, false)
					c.reportStatus(channel.ConnectionConnected, "")
					c.reportPairing(channel.PairingEvent{Name: c.Name(), State: "connected", Detail: "WhatsApp connected"})
					return nil
				default:
					if err := c.waitForPairingRestart(ctx, event.Event, "WhatsApp pairing: "+event.Event); err != nil {
						return err
					}
					restart = true
				}
			}
		}
	}
	return nil
}

func (c *Client) waitForPairingRestart(ctx context.Context, state, detail string) error {
	c.setPairingState(true, false)
	retryingDetail := detail + " — retrying automatically…"
	c.reportPairing(channel.PairingEvent{Name: c.Name(), State: state, Detail: retryingDetail})
	fmt.Fprintln(c.log, retryingDetail)
	delay := c.pairingDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.pairingRetry:
	case <-timer.C:
		// Do not carry a simultaneous manual signal into the newly created
		// session and immediately restart it a second time.
		select {
		case <-c.pairingRetry:
		default:
		}
	}
	if c.client != nil {
		c.client.Disconnect()
	}
	return nil
}

func (c *Client) setPairingState(active, ready bool) {
	c.pairingMu.Lock()
	c.pairingActive = active
	c.pairingReady = ready
	if !ready {
		c.pairingQR = ""
	}
	c.pairingMu.Unlock()
}

func (c *Client) setPairingQR(rendered string) {
	c.pairingMu.Lock()
	c.pairingActive = true
	c.pairingReady = true
	c.pairingQR = rendered
	c.pairingMu.Unlock()
}

func (c *Client) onEvent(raw any) {
	switch event := raw.(type) {
	case *events.Connected:
		if !c.isPaired() {
			c.reportStatus(channel.ConnectionConnecting, "waiting for pairing")
			return
		}
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
	sender := preferredPhoneUser(event.Info.Sender, event.Info.SenderAlt)
	allowedSenders := []string{event.Info.Sender.User, event.Info.SenderAlt.User}
	if c.config.Mode == "self-chat" && c.client != nil && c.client.Store != nil && c.client.Store.ID != nil {
		// Messages sent from the primary phone may arrive under the account's
		// opaque LID without SenderAlt. Authorize and key them by the paired
		// phone-number identity rather than requiring users to whitelist a LID.
		sender = c.client.Store.ID.User
		allowedSenders = append(allowedSenders, sender, event.Info.RecipientAlt.User)
	}
	if !c.allowed(allowedSenders...) {
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

func preferredPhoneUser(addresses ...types.JID) string {
	fallback := ""
	for _, address := range addresses {
		if address.IsEmpty() {
			continue
		}
		if fallback == "" {
			fallback = address.User
		}
		switch address.Server {
		case types.DefaultUserServer, types.LegacyUserServer, types.HostedServer:
			return address.User
		}
	}
	return fallback
}

func (c *Client) isPaired() bool {
	return c.client != nil && c.client.Store != nil && c.client.Store.ID != nil
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
			contextInfo := messageContext(incoming.message)
			replyTo := ""
			if contextInfo != nil {
				replyTo = contextInfo.GetStanzaID()
			}
			c.handleWithNativeActivity(incoming.received, incoming.chat, incoming.sender, text, string(incoming.id), replyTo, finishActivity)
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
		status := channel.ConnectionStatus{Name: c.Name(), State: state, Detail: detail}
		if state == channel.ConnectionConnected {
			status.Identity, status.Link = c.pairedAccountIdentity()
		}
		c.report(status)
	}
}

func (c *Client) pairedAccountIdentity() (string, string) {
	if !c.isPaired() {
		return "", ""
	}
	address := c.client.Store.ID.ToNonAD()
	switch address.Server {
	case types.DefaultUserServer, types.LegacyUserServer, types.HostedServer:
	default:
		return "", ""
	}
	number := config.NormalizeWhatsAppNumber(address.User)
	if number == "" {
		return "", ""
	}
	return "+" + number, "https://wa.me/" + number
}

func (c *Client) handle(received time.Time, chat types.JID, sender, text string) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	c.handleWithActivity(received, chat, sender, text, c.activity.Start(ctx, chat))
}

func (c *Client) handleWithActivity(received time.Time, chat types.JID, sender, text string, finishActivity func()) {
	c.handleWithNativeActivity(received, chat, sender, text, "", "", finishActivity)
}

func (c *Client) handleWithNativeActivity(received time.Time, chat types.JID, sender, text, nativeMessageID, nativeReplyToID string, finishActivity func()) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	emit := func(event core.Event) {
		if !event.Done || event.Continues || (event.Kind != core.EventFinal && event.Kind != core.EventError) {
			return
		}
		defer finishActivity()
		text := event.Text
		if event.FinalText != nil {
			text = *event.FinalText
		}
		if text != "" {
			_ = c.send(ctx, chat, text)
		}
		for _, attachment := range event.Attachments {
			if err := c.sendAttachment(ctx, chat, attachment); err != nil {
				_ = c.send(ctx, chat, "Spynel attachment delivery error: "+err.Error())
			}
		}
	}
	err := c.handler(ctx, core.Message{
		Channel: c.Name(), Conversation: whatsappConversation(chat, sender), Sender: sender,
		Text: text, NativeMessageID: nativeMessageID, NativeReplyToID: nativeReplyToID,
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
	number := config.NormalizeWhatsAppNumber(sender)
	if number == "" {
		number = config.NormalizeWhatsAppNumber(chat.User)
	}
	return "WA-" + number
}

func (c *Client) send(ctx context.Context, chat types.JID, text string) error {
	if c.client == nil && c.deliver == nil {
		return errors.New("WhatsApp is not connected")
	}
	for _, chunk := range split(markdownfmt.WhatsApp(text), 60000) {
		response, err := c.sendMessage(ctx, chat, &waE2E.Message{Conversation: proto.String(chunk)})
		if err != nil {
			return err
		}
		c.recordSent(response.ID)
	}
	return nil
}

func (c *Client) sendAttachment(ctx context.Context, chat types.JID, attachment core.OutboundAttachment) error {
	if (c.client == nil && c.upload == nil) || (c.client == nil && c.deliver == nil) {
		return errors.New("WhatsApp is not connected")
	}
	file, err := media.OpenOutbound(attachment)
	if err != nil {
		return err
	}
	defer file.Close()
	mediaType := whatsmeow.MediaDocument
	if attachment.Kind == "photo" {
		mediaType = whatsmeow.MediaImage
	}
	var uploaded whatsmeow.UploadResponse
	if c.upload != nil {
		uploaded, err = c.upload(ctx, file, nil, mediaType)
	} else {
		uploaded, err = c.client.UploadReader(ctx, file, nil, mediaType)
	}
	if err != nil {
		return err
	}
	var message *waE2E.Message
	if attachment.Kind == "photo" {
		message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: &uploaded.URL, DirectPath: &uploaded.DirectPath, MediaKey: uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256, FileSHA256: uploaded.FileSHA256, FileLength: &uploaded.FileLength,
			Mimetype: proto.String(attachment.MediaType),
		}}
	} else {
		message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: &uploaded.URL, DirectPath: &uploaded.DirectPath, MediaKey: uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256, FileSHA256: uploaded.FileSHA256, FileLength: &uploaded.FileLength,
			Mimetype: proto.String(attachment.MediaType), FileName: proto.String(attachment.Name), Title: proto.String(attachment.Name),
		}}
	}
	response, err := c.sendMessage(ctx, chat, message)
	if err != nil {
		return err
	}
	c.recordSent(response.ID)
	return nil
}

func (c *Client) sendMessage(ctx context.Context, chat types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
	if c.deliver != nil {
		return c.deliver(ctx, chat, message)
	}
	return c.client.SendMessage(ctx, chat, message)
}

func (c *Client) recordSent(id types.MessageID) {
	c.mu.Lock()
	c.sentIDs[id] = time.Now()
	c.cleanupSentLocked()
	c.mu.Unlock()
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
	if !config.HasAllowedWhatsAppNumber(c.config.AllowedNumbers) {
		return false
	}
	for _, sender := range senders {
		number := config.NormalizeWhatsAppNumber(sender)
		for _, allowed := range c.config.AllowedNumbers {
			if number != "" && number == config.NormalizeWhatsAppNumber(allowed) {
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
