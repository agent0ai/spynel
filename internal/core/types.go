package core

import "time"

const SpynelASCII = `     ████     ████     ███████ ██████  ██    ██ ███    ██ ███████ ██
   ██    ██ ██    ██   ██      ██   ██  ██  ██  ████   ██ ██      ██
  ██  ██  ███  ██  ██  ███████ ██████    ████   ██ ██  ██ █████   ██
   ██    ██ ██    ██        ██ ██         ██    ██  ██ ██ ██      ██
     ████     ████     ███████ ██         ██    ██   ████ ███████ ███████`

// SpynelLogoMarkdown marks the full terminal logo for semantic primary-color
// rendering when a welcome is persisted as an ordinary chat message.
const SpynelLogoMarkdown = "```spynel-logo\n" + SpynelASCII + "\n```"

// ScreenWhatsAppQR identifies the chrome-free, full-terminal pairing view.
const ScreenWhatsAppQR = "fullscreen:whatsapp:qr"

// Message is the transport-neutral input accepted by the application.
type Message struct {
	Channel      string `json:"channel"`
	Conversation string `json:"conversation"`
	Sender       string `json:"sender,omitempty"`
	// ReplyTo is a bounded provider-neutral reference derived only from the
	// accepted inbound event: native message ID followed by an optional preview.
	ReplyTo      string    `json:"reply_to,omitempty"`
	InstanceID   string    `json:"instance_id,omitempty"`
	FollowupOnly bool      `json:"followup_only,omitempty"`
	Text         string    `json:"text"`
	ReceivedAt   time.Time `json:"received_at"`
}

// Event is a streamed harness or application response.
type Event struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Active is meaningful only for EventActivity. The application emits true
	// exactly when a main communication-agent turn takes ownership of this
	// response stream and false before its terminal event is delivered.
	Active bool `json:"active,omitempty"`
	// FinalText identifies the last assistant-message item when Text contains
	// the complete streamed turn. Remote chat transports deliver this item.
	FinalText   *string              `json:"final_text,omitempty"`
	ThreadID    string               `json:"thread_id,omitempty"`
	TurnID      string               `json:"turn_id,omitempty"`
	Clear       bool                 `json:"clear,omitempty"`
	Done        bool                 `json:"done,omitempty"`
	Continues   bool                 `json:"continues,omitempty"`
	Local       bool                 `json:"local,omitempty"`
	Screen      *Screen              `json:"screen,omitempty"`
	Attachments []OutboundAttachment `json:"attachments,omitempty"`
	// Execution is a provider-neutral lifecycle signal. Adapters populate it
	// from structured protocol events; renderers must never infer it from Text.
	Execution *ExecutionStatus `json:"execution,omitempty"`
}

// ExecutionStatus describes a live provider turn without mixing in a durable
// task/goal phase or outcome. A stalled state is valid only when the provider
// supplies explicit structured evidence; consumers must not infer it from a
// quiet text stream. Detail is optional, sanitized by the application, and
// intended for short retry/error context only.
type ExecutionStatus struct {
	State            string    `json:"state"`
	Detail           string    `json:"detail,omitempty"`
	At               time.Time `json:"at,omitempty"`
	ReconnectAttempt int       `json:"reconnect_attempt,omitempty"`
	ReconnectTotal   int       `json:"reconnect_total,omitempty"`
}

// OutboundAttachment is a validated local file that a channel may deliver
// using its native media API. Kind is either "attachment" or "photo".
type OutboundAttachment struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	MaxBytes  int64  `json:"max_bytes"`
}

// Screen is a transport-neutral UI surface. ParentID marks a nested screen so
// the TUI can restore its exact parent form state after selection or Escape.
// ActionMessage is an optional application-authored assistant reply produced
// by a screen action; a result-only response may leave ID empty. Text-only
// channels use the same setting keys as commands.
type Screen struct {
	ID             string
	ParentID       string
	ActionMessage  string
	Title          string
	Hints          []ScreenHint
	Tabs           []string
	ActiveTab      int
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

// ScreenHint describes one context-sensitive TUI footer binding. Screens own
// their advertised controls so action lists, wizards, and editable forms do
// not inherit misleading hints from one generic form footer.
type ScreenHint struct {
	Key    string
	Action string
}

type ChatEntry struct {
	Role string
	Text string
}

type ScreenControl struct {
	Key string
	// Section renders a labeled rule immediately before this control. Empty
	// values keep the control in the preceding section.
	Section             string
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
	Logs               int `json:"logs"`
	Jobs               int `json:"jobs"`
	LiveBackgroundJobs int `json:"live_background_jobs"`
}

// DurableWorkCounts is the current nonterminal task and goal census used by
// constrained live status surfaces. Detailed diagnostics remain on the
// application shared-state boundary.
type DurableWorkCounts struct {
	Tasks int `json:"tasks"`
	Goals int `json:"goals"`
}

const (
	EventDelta  = "delta"
	EventFinal  = "final"
	EventError  = "error"
	EventStatus = "status"
	// EventActivity exposes the main communication-agent activity boundary to
	// presentation channels without coupling them to harness or job internals.
	EventActivity = "activity"
	EventScreen   = "screen"
	// EventThemePicker asks an interactive TUI to open its inline theme list.
	EventThemePicker = "theme-picker"
)

// Emit is safe for asynchronous use by a harness implementation.
type Emit func(Event)
