package channel

import (
	"context"
	"io"

	"github.com/frdel/spynel/internal/core"
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
	Name     string
	State    ConnectionState
	Detail   string
	Identity string
	Link     string
}

type StatusReporter func(ConnectionStatus)

type PairingEvent struct {
	Name     string
	State    string
	Code     string
	Rendered string
	Detail   string
}

type PairingReporter func(PairingEvent)

type Notice struct {
	Channel string
	Sender  string
	Text    string
}

type NoticeReporter func(Notice)

type Channel interface {
	Name() string
	Run(context.Context, Handler) error
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

type NoticeReporterSetter interface {
	SetNoticeReporter(NoticeReporter)
}
