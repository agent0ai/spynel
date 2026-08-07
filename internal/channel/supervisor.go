package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/frdel/spynel/internal/config"
)

// Managed describes one hot-reloadable transport without coupling the shared
// supervisor to a concrete channel package.
type Managed struct {
	Name        string
	Enabled     func(config.Config) bool
	Fingerprint func(config.Config) string
	Build       func(config.Config) (Channel, error)
}

type runningChannel struct {
	cancel      context.CancelFunc
	fingerprint string
	generation  uint64
}

// Supervisor owns channel lifecycles. It consumes the persisted config stream,
// cancels stale instances, and retries enabled transports that fail to start.
// A broken integration therefore cannot terminate the TUI or task manager.
type Supervisor struct {
	settings *config.Store
	handler  Handler
	managed  []Managed
	report   StatusReporter
	log      io.Writer

	mu         sync.Mutex
	running    map[string]*runningChannel
	generation uint64
}

func NewSupervisor(settings *config.Store, handler Handler, managed []Managed, report StatusReporter, log io.Writer) *Supervisor {
	return &Supervisor{settings: settings, handler: handler, managed: managed, report: report, log: log, running: map[string]*runningChannel{}}
}

func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.reconcile(ctx, s.settings.Snapshot()); err != nil {
		s.logf("channel supervisor: %v", err)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	defer s.stopAll()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cfg := <-s.settings.Updates():
			if err := s.reconcile(ctx, cfg); err != nil {
				s.logf("channel supervisor: %v", err)
			}
		case <-ticker.C:
			if err := s.reconcile(ctx, s.settings.Snapshot()); err != nil {
				s.logf("channel supervisor: %v", err)
			}
		}
	}
}

func (s *Supervisor) reconcile(ctx context.Context, cfg config.Config) error {
	var problems []error
	for _, managed := range s.managed {
		if managed.Name == "" || managed.Enabled == nil || managed.Fingerprint == nil || managed.Build == nil {
			problems = append(problems, errors.New("invalid managed channel registration"))
			continue
		}
		enabled := managed.Enabled(cfg)
		fingerprint := managed.Fingerprint(cfg)
		s.mu.Lock()
		current := s.running[managed.Name]
		if !enabled {
			if current != nil {
				delete(s.running, managed.Name)
				current.cancel()
			}
			s.mu.Unlock()
			s.publish(ConnectionStatus{Name: managed.Name, State: ConnectionUnconfigured})
			continue
		}
		if current != nil && current.fingerprint == fingerprint {
			s.mu.Unlock()
			continue
		}
		if current != nil {
			delete(s.running, managed.Name)
			current.cancel()
		}
		s.generation++
		generation := s.generation
		channelContext, cancel := context.WithCancel(ctx)
		running := &runningChannel{cancel: cancel, fingerprint: fingerprint, generation: generation}
		s.running[managed.Name] = running
		s.mu.Unlock()

		instance, err := managed.Build(cfg)
		if err != nil {
			s.removeIfCurrent(managed.Name, running)
			cancel()
			s.publish(ConnectionStatus{Name: managed.Name, State: ConnectionError, Detail: err.Error()})
			problems = append(problems, fmt.Errorf("%s: %w", managed.Name, err))
			continue
		}
		if reporter, ok := instance.(ConnectionReporter); ok {
			reporter.SetStatusReporter(func(status ConnectionStatus) {
				if s.isCurrent(managed.Name, running) {
					s.publish(status)
				}
			})
		}
		if logger, ok := instance.(LogWriterSetter); ok {
			logger.SetLogWriter(s.log)
		}
		s.publish(ConnectionStatus{Name: managed.Name, State: ConnectionConnecting})
		go s.runOne(channelContext, managed.Name, running, instance)
	}
	return errors.Join(problems...)
}

func (s *Supervisor) runOne(ctx context.Context, name string, running *runningChannel, instance Channel) {
	err := instance.Run(ctx, s.handler)
	if !s.removeIfCurrent(name, running) {
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		s.logf("%s: %v", name, err)
		s.publish(ConnectionStatus{Name: name, State: ConnectionError, Detail: err.Error()})
	}
}

func (s *Supervisor) isCurrent(name string, expected *runningChannel) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[name] == expected
}

func (s *Supervisor) removeIfCurrent(name string, expected *runningChannel) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[name] != expected {
		return false
	}
	delete(s.running, name)
	return true
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	running := s.running
	s.running = map[string]*runningChannel{}
	s.mu.Unlock()
	for _, current := range running {
		current.cancel()
	}
}

func (s *Supervisor) publish(status ConnectionStatus) {
	if s.report != nil {
		s.report(status)
	}
}

func (s *Supervisor) logf(format string, values ...any) {
	if s.log != nil {
		_, _ = fmt.Fprintf(s.log, format+"\n", values...)
	}
}
