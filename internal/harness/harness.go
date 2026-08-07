package harness

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/frdel/spynel/internal/core"
)

// HarnessConfig is the provider-neutral runtime contract used to construct a
// coding harness. Provider-specific adapters translate the common policies to
// the closest native CLI options they support.
type HarnessConfig struct {
	Name           string
	Command        string
	Cwd            string
	Model          string
	Effort         string
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
	ID            string
	DisplayName   string
	Description   string
	Efforts       []string
	DefaultEffort string
	Default       bool
}

// ModelProvider is optional because some third-party harness extensions may
// not be able to enumerate models. Callers must degrade to free-form input.
type ModelProvider interface {
	Models(context.Context) ([]Model, error)
}

// Availability is implemented by runtime supervisors that can remain usable
// for configuration even when their selected harness executable is missing.
type Availability interface {
	Available() (bool, string)
	ReadyEvents() <-chan struct{}
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
