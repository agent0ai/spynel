package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/agent0ai/spynel/internal/core"
)

// HarnessConfig is the provider-neutral runtime contract used to construct a
// coding harness. Provider-specific adapters translate the common policies to
// the closest native CLI options they support.
type HarnessConfig struct {
	Name           string
	Command        string
	Args           []string
	Env            []string
	Cwd            string
	Model          string
	Effort         string
	LegacyEffort   bool
	ServiceMode    string
	ApprovalPolicy string
	Sandbox        string
	Network        bool
	SessionsFile   string
	Version        string
	Stderr         io.Writer
}

// Model describes one model choice exposed by a harness. IDs are the exact
// values accepted by SetModel or by the harness command line.
type Model struct {
	ID                 string
	DisplayName        string
	Description        string
	Efforts            []string
	DefaultEffort      string
	ServiceModes       []ModelPropertyOption
	DefaultServiceMode string
	Default            bool
}

// ModelPropertyOption is one provider-supplied value for a model-associated
// inference or service property. Values are passed through exactly as exposed.
type ModelPropertyOption struct {
	ID          string
	DisplayName string
	Description string
}

// InferenceSelection is captured at provider-dispatch admission. Empty values
// mean inherit the harness/model default.
type InferenceSelection struct {
	Model        string
	Effort       string
	LegacyEffort bool
	ServiceMode  string
}

// InferenceDispatcher lets adapters atomically snapshot every forward-looking
// model-associated property rather than consulting mutable process config.
type InferenceDispatcher interface {
	SendWithInference(context.Context, string, string, InferenceSelection, core.Emit) (threadID string, steered bool, err error)
	SetInference(InferenceSelection)
}

// ValidateInferenceSelection rejects stale or unsupported properties without
// passing them to a provider. A custom model remains usable only with inherited
// properties because its capabilities are unknown.
func ValidateInferenceSelection(models []Model, selection InferenceSelection) error {
	if selection.Effort == "" && selection.ServiceMode == "" {
		return nil
	}
	var selected *Model
	for index := range models {
		if models[index].ID == selection.Model || selection.Model == "" && models[index].Default {
			selected = &models[index]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("model %q has no verified inference-property capabilities; reset effort and service mode to inherit", selection.Model)
	}
	if selection.Effort != "" {
		valid := false
		for _, value := range selected.Efforts {
			valid = valid || value == selection.Effort
		}
		if !valid {
			return fmt.Errorf("reasoning effort %q is not supported by model %q", selection.Effort, selected.ID)
		}
	}
	if selection.ServiceMode != "" {
		valid := false
		for _, value := range selected.ServiceModes {
			valid = valid || value.ID == selection.ServiceMode
		}
		if !valid {
			return fmt.Errorf("service mode %q is not supported by model %q", selection.ServiceMode, selected.ID)
		}
	}
	return nil
}

// ModelProvider is optional because some third-party harness extensions may
// not be able to enumerate models. Callers must degrade to free-form input.
type ModelProvider interface {
	Models(context.Context) ([]Model, error)
}

// ModelDispatcher snapshots a forward-looking model selection independently
// from the adapter instance. Supervisor takes this snapshot at dispatch
// admission so a concurrent configuration commit has one deterministic order:
// admitted work keeps its snapshot and later work receives the new model.
type ModelDispatcher interface {
	SendWithModel(context.Context, string, string, string, core.Emit) (threadID string, steered bool, err error)
	SetModel(string)
}

// Availability is implemented by runtime supervisors that can remain usable
// for configuration even when their selected harness executable is missing.
type Availability interface {
	Available() (bool, string)
	ReadyEvents() <-chan struct{}
}

// FollowUpMode describes how a harness accepts another user message while a
// turn is active. Harnesses that do not implement FollowUpProvider are queued
// conservatively by Supervisor, which makes a basic adapter safe by default.
type FollowUpMode string

const (
	FollowUpQueue FollowUpMode = "queue"
	FollowUpSteer FollowUpMode = "steer"
)

// FollowUpProvider is an optional harness capability. Native steering keeps a
// follow-up in the current provider turn; queue mode starts it after the active
// provider turn completes, using the same durable conversation session.
type FollowUpProvider interface {
	FollowUpMode() FollowUpMode
}

// NativeSteerer delivers only to an already-active provider turn. It must
// never fall back to starting a new turn when completion wins a race. The
// beforeDelivery callback is the atomic durable reservation boundary: the
// adapter calls it exactly once after fencing completion and immediately
// before provider delivery, or not at all when the turn is already inactive.
type NativeSteerer interface {
	Steer(context.Context, string, string, core.Emit, func() bool) (threadID string, err error)
}

var (
	errNativeTurnInactive       = errors.New("native provider turn is no longer active")
	errNativeDeliveryUnreserved = errors.New("native provider delivery was not reserved")
)

// ControlRequest is a non-owning coordination message for an existing turn.
// The supervisor delivers it through the current execution emitter so the
// command caller never becomes responsible for provider output or completion.
type ControlRequest struct {
	ID                  string
	Prompt              string
	ContinuationPrompt  string
	Validate            func() bool
	PrepareContinuation func() bool
	ReserveProviderTurn func() bool
}

type ControlResult struct {
	Queued    bool
	Duplicate bool
}

// ControlSender is implemented by the provider-neutral supervisor. Harness
// adapters continue to expose only their declared native-steer/queue behavior.
type ControlSender interface {
	SendControl(context.Context, string, ControlRequest) (ControlResult, error)
}

// ConversationSender preserves the exact user message separately from the
// rendered harness prompt. Supervisors use it to collapse several queued chat
// follow-ups into one provider turn without concatenating repeated framework
// instructions and history snapshots. Basic harness implementations may omit
// it and continue to receive ordinary Send calls.
type ConversationSender interface {
	SendConversation(context.Context, string, string, string, core.Emit) (threadID string, steered bool, err error)
}

type Harness interface {
	Start(context.Context) error
	Send(context.Context, string, string, core.Emit) (threadID string, steered bool, err error)
	Interrupt(context.Context, string) (bool, error)
	ResetSession(string) error
	ThreadID(string) string
	IsActive(string) bool
	Close() error
}

type Factory func(HarnessConfig) (Harness, error)

type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

func (r *Registry) Register(name string, factory Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

func (r *Registry) Create(cfg HarnessConfig) (Harness, error) {
	r.mu.RLock()
	factory := r.factories[cfg.Name]
	r.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("unknown coding harness %q", cfg.Name)
	}
	return factory(cfg)
}
