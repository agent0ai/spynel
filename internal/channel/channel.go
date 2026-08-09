package channel

import (
	"context"
	"io"
	"strings"
	"unicode"

	"github.com/agent0ai/spynel/internal/core"
)

const replyPreviewRunes = 100

// ReplyReference formats an opaque transport message ID with an optional,
// whitespace-normalized preview. It never invents context or drops a valid ID.
func ReplyReference(nativeID, preview string) string {
	nativeID = strings.TrimSpace(nativeID)
	if nativeID == "" {
		return ""
	}
	preview = strings.Join(strings.FieldsFunc(preview, unicode.IsSpace), " ")
	runes := []rune(preview)
	if len(runes) > replyPreviewRunes {
		preview = string(runes[:replyPreviewRunes-1]) + "…"
	}
	if preview == "" {
		return nativeID
	}
	return nativeID + " " + preview
}

type Handler func(context.Context, core.Message, core.Emit) error

// ErrorResponse renders a complete user-facing remote-channel failure as an
// ordinary message. Unlike the TUI's aligned transcript rows, remote messages
// keep subsequent lines unindented.
func ErrorResponse(text string) string {
	if text == "" {
		text = "The harness turn failed."
	}
	return "Error " + text
}

type ConnectionState string

const (
	ConnectionUnconfigured ConnectionState = "unconfigured"
	ConnectionConnecting   ConnectionState = "connecting"
	ConnectionConnected    ConnectionState = "connected"
	ConnectionError        ConnectionState = "error"
)

type ConnectionStatus struct {
	Name     string          `json:"name"`
	State    ConnectionState `json:"state"`
	Detail   string          `json:"detail,omitempty"`
	Identity string          `json:"identity,omitempty"`
	Link     string          `json:"link,omitempty"`
}

type StatusReporter func(ConnectionStatus)

type PairingEvent struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Code     string `json:"code,omitempty"`
	Rendered string `json:"rendered,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type PairingReporter func(PairingEvent)

type Notice struct {
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
}

type NoticeReporter func(Notice)

type Notification struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Channel interface {
	Name() string
	Run(context.Context, Handler) error
}

// RuntimeAuthorizer is the fail-closed boundary for channels whose external
// runtime is safe only while a valid live sender allow-list exists. The
// supervisor validates before publishing a connecting state and revokes stale
// instances before cancellation during disable or hot replacement.
type RuntimeAuthorizer interface {
	ValidateRuntimeAuthorization() error
	RevokeRuntimeAuthorization()
}

// ProactiveDeliverer sends a complete assistant message after the inbound
// request lifecycle has ended. Implementations must re-apply transport
// authorization rather than trusting a locally supplied origin string.
type ProactiveDeliverer interface {
	Deliver(context.Context, string, string, string) error
}

type DeliveryRouter interface {
	Deliver(context.Context, string, string, string, string) error
}

type ConnectionReporter interface {
	SetStatusReporter(StatusReporter)
}

type LogWriterSetter interface {
	SetLogWriter(io.Writer)
}

type PairingReporterSetter interface {
	SetPairingReporter(PairingReporter)
}

// PairingController is implemented by a running channel that can refresh an
// interactive pairing session or generate a phone-linking code.
type PairingController interface {
	RetryPairing() error
	PairPhone(context.Context, string) (string, error)
}

// PairingManager routes pairing actions to the currently running channel.
type PairingManager interface {
	RetryPairing(string) error
	PairPhone(context.Context, string, string) (string, error)
}

type NoticeReporterSetter interface {
	SetNoticeReporter(NoticeReporter)
}
