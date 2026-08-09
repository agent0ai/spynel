package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/config"
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
	instance    Channel
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
	logEvent func(level, component, event, message string)

	mu         sync.Mutex
	running    map[string]*runningChannel
	generation uint64
	wait       sync.WaitGroup
}

// SetEventLogger installs the structured lifecycle boundary used by the
// application owner. The ordinary writer remains available to adapters for
// bounded free-form diagnostics.
func (s *Supervisor) SetEventLogger(logEvent func(level, component, event, message string)) {
	s.logEvent = logEvent
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
	defer func() {
		s.stopAll()
		s.wait.Wait()
	}()
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
				revokeRuntimeAuthorization(current.instance)
				current.cancel()
				s.event("info", managed.Name, "disconnected", "Channel disabled and disconnected")
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
			revokeRuntimeAuthorization(current.instance)
			current.cancel()
			s.event("info", managed.Name, "reconnecting", "Channel configuration changed; reconnecting")
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
			s.event("error", managed.Name, "build_failed", "Channel construction failed: "+err.Error())
			continue
		}
		if reporter, ok := instance.(ConnectionReporter); ok {
			reporter.SetStatusReporter(func(status ConnectionStatus) {
				if s.isCurrent(managed.Name, running) {
					s.eventForStatus(status)
					s.publish(status)
				}
			})
		}
		if logger, ok := instance.(LogWriterSetter); ok {
			logger.SetLogWriter(s.log)
		}
		if authorizer, ok := instance.(RuntimeAuthorizer); ok {
			if err := authorizer.ValidateRuntimeAuthorization(); err != nil {
				cancel()
				s.publish(ConnectionStatus{Name: managed.Name, State: ConnectionError, Detail: err.Error()})
				problems = append(problems, fmt.Errorf("%s: %w", managed.Name, err))
				s.event("error", managed.Name, "authorization_failed", "Channel runtime authorization failed: "+err.Error())
				continue
			}
		}
		s.mu.Lock()
		if s.running[managed.Name] == running {
			running.instance = instance
		}
		s.mu.Unlock()
		s.publish(ConnectionStatus{Name: managed.Name, State: ConnectionConnecting})
		s.event("info", managed.Name, "connecting", "Channel connection starting")
		s.wait.Add(1)
		go func() {
			defer s.wait.Done()
			s.runOne(channelContext, managed.Name, running, instance)
		}()
	}
	return errors.Join(problems...)
}

func (s *Supervisor) pairingController(name string) (PairingController, error) {
	s.mu.Lock()
	running := s.running[name]
	var instance Channel
	if running != nil {
		instance = running.instance
	}
	s.mu.Unlock()
	if instance == nil {
		return nil, fmt.Errorf("%s is not running", name)
	}
	controller, ok := instance.(PairingController)
	if !ok {
		return nil, fmt.Errorf("%s does not support interactive pairing", name)
	}
	return controller, nil
}

// RetryPairing asks the active channel instance to replace its expired pairing
// session without changing persisted settings.
func (s *Supervisor) RetryPairing(name string) error {
	controller, err := s.pairingController(name)
	if err != nil {
		return err
	}
	return controller.RetryPairing()
}

// PairPhone asks the active channel instance for a phone-linking code.
func (s *Supervisor) PairPhone(ctx context.Context, name, phone string) (string, error) {
	controller, err := s.pairingController(name)
	if err != nil {
		return "", err
	}
	return controller.PairPhone(ctx, phone)
}

func (s *Supervisor) Deliver(ctx context.Context, name, conversation, eventID, text string) error {
	s.mu.Lock()
	running := s.running[name]
	var instance Channel
	if running != nil {
		instance = running.instance
	}
	s.mu.Unlock()
	if instance == nil {
		return fmt.Errorf("%s is disconnected", name)
	}
	deliverer, ok := instance.(ProactiveDeliverer)
	if !ok {
		return fmt.Errorf("%s does not support proactive delivery", name)
	}
	return deliverer.Deliver(ctx, conversation, eventID, text)
}

func (s *Supervisor) runOne(ctx context.Context, name string, running *runningChannel, instance Channel) {
	err := instance.Run(ctx, s.handler)
	if authorizer, ok := instance.(RuntimeAuthorizer); ok && err != nil && ctx.Err() == nil {
		if authorizationErr := authorizer.ValidateRuntimeAuthorization(); authorizationErr != nil {
			if !s.isCurrent(name, running) {
				return
			}
			s.logf("%s: %v", name, authorizationErr)
			s.publish(ConnectionStatus{Name: name, State: ConnectionError, Detail: authorizationErr.Error()})
			s.event("error", name, "authorization_failed", "Channel runtime authorization failed: "+authorizationErr.Error())
			return
		}
	}
	if !s.removeIfCurrent(name, running) {
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		s.logf("%s: %v", name, err)
		s.publish(ConnectionStatus{Name: name, State: ConnectionError, Detail: err.Error()})
		s.event("error", name, "disconnected", "Channel stopped unexpectedly: "+err.Error())
	} else {
		s.event("info", name, "disconnected", "Channel stopped")
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
		revokeRuntimeAuthorization(current.instance)
		current.cancel()
	}
}

func revokeRuntimeAuthorization(instance Channel) {
	if authorizer, ok := instance.(RuntimeAuthorizer); ok {
		authorizer.RevokeRuntimeAuthorization()
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

func (s *Supervisor) event(level, name, event, message string) {
	if s.logEvent != nil {
		s.logEvent(level, "channel."+name, event, message)
	}
}

func (s *Supervisor) eventForStatus(status ConnectionStatus) {
	switch status.State {
	case ConnectionConnected:
		s.event("info", status.Name, "connected", "Channel connected")
	case ConnectionConnecting:
		s.event("info", status.Name, "connecting", "Channel connecting")
	case ConnectionError:
		s.event("error", status.Name, "connection_error", "Channel connection failed: "+status.Detail)
	}
}
