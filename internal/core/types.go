package core

import "time"

const SpynelASCII = `███████╗██████╗ ██╗   ██╗███╗   ██╗███████╗██╗
██╔════╝██╔══██╗╚██╗ ██╔╝████╗  ██║██╔════╝██║
███████╗██████╔╝ ╚████╔╝ ██╔██╗ ██║█████╗  ██║
╚════██║██╔═══╝   ╚██╔╝  ██║╚██╗██║██╔══╝  ██║
███████║██║        ██║   ██║ ╚████║███████╗███████╗
╚══════╝╚═╝        ╚═╝   ╚═╝  ╚═══╝╚══════╝╚══════╝`

// Message is the transport-neutral input accepted by the application.
type Message struct {
	Channel      string    `json:"channel"`
	Conversation string    `json:"conversation"`
	Sender       string    `json:"sender,omitempty"`
	Text         string    `json:"text"`
	ReceivedAt   time.Time `json:"received_at"`
}

// Event is a streamed harness or application response.
type Event struct {
	Kind     string  `json:"kind"`
	Text     string  `json:"text,omitempty"`
	ThreadID string  `json:"thread_id,omitempty"`
	TurnID   string  `json:"turn_id,omitempty"`
	Clear    bool    `json:"clear,omitempty"`
	Done     bool    `json:"done,omitempty"`
	Local    bool    `json:"local,omitempty"`
	Screen   *Screen `json:"screen,omitempty"`
}

// Screen is a transport-neutral UI surface. ParentID marks a nested screen so
// the TUI can restore its exact parent form state after selection or Escape.
// Text-only channels use the same setting keys as commands.
type Screen struct {
	ID             string
	ParentID       string
	Title          string
	Banner         string
	Status         string
	Subtitle       string
	Controls       []ScreenControl
	Markdown       bool
	SaveDisabled   bool
	StartAtTop     bool
	InitialControl string
	Required       bool
	ExitOnAction   bool
	Conversation   string
	Transcript     []ChatEntry
}

type ChatEntry struct {
	Role string
	Text string
}

type ScreenControl struct {
	Key                 string
	Label               string
	Description         string
	DescriptionMarkdown bool
	Kind                string
	Value               string
	Options             []string
	Secret              bool
	Configured          bool
	Advanced            bool
}

// SlashCommand describes a command shared by the application and interactive
// channel affordances. Value is inserted into a composer; Usage is displayed
// to users and may include argument placeholders.
type SlashCommand struct {
	Value       string
	Usage       string
	Description string
}

// RuntimeStatus is the live application activity summary shown by channels.
type RuntimeStatus struct {
	Logs int
	Jobs int
}

const (
	EventDelta  = "delta"
	EventFinal  = "final"
	EventError  = "error"
	EventStatus = "status"
	EventScreen = "screen"
)

// Emit is safe for asynchronous use by a harness implementation.
type Emit func(Event)
