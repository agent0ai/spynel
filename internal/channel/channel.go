package channel

import (
	"context"
	"io"

	"github.com/agent0ai/spynel/internal/core"
)

type Handler func(context.Context, core.Message, core.Emit) error

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

// DeliveryReceipt contains opaque native IDs for every delivered text chunk.
// Callers persist them privately for exact reply correlation only.
type DeliveryReceipt struct {
	MessageIDs []string
}

type Channel interface {
	Name() string
	Run(context.Context, Handler) error
}

// ProactiveDeliverer sends a complete assistant message after the inbound
// request lifecycle has ended. Implementations must re-apply transport
// authorization rather than trusting a locally supplied origin string.
type ProactiveDeliverer interface {
	Deliver(context.Context, string, string, string) (DeliveryReceipt, error)
}

type DeliveryRouter interface {
	Deliver(context.Context, string, string, string, string) (DeliveryReceipt, error)
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
