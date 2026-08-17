package cli

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/theme"
)

var errOwnershipLost = errors.New("workspace server ownership changed during startup")

type primaryTerm struct {
	election *instance.Election
	token    string
	cancel   context.CancelFunc
	listener net.Listener
	service  *app.Service

	apiDone          chan struct{}
	apiError         chan error
	channelsDone     <-chan error
	orchestratorDone chan error
	stopOnce         sync.Once
}

func runOwnerElection(ctx context.Context, cfg config.Config, version string, election *instance.Election, restart, update func()) error {
	ticker := time.NewTicker(instance.RetryInterval)
	defer ticker.Stop()
	var term *primaryTerm
	var nextHeartbeat time.Time
	defer func() {
		if term != nil {
			term.stop()
		}
	}()

	step := func() error {
		now := time.Now()
		if term != nil {
			select {
			case err := <-term.apiError:
				term.service.Runtime.LogEvent("error", "localapi", "stopped", "Workspace API stopped: "+errorText(err))
				term.stop()
				term = nil
				return nil
			default:
			}
			if now.Before(nextHeartbeat) {
				return nil
			}
			_, owned, err := election.Renew(term.token)
			if err != nil {
				term.service.Runtime.LogEvent("error", "instance", "lease_renew_failed", "Renew primary lease: "+err.Error())
			}
			if err != nil || !owned {
				term.stop()
				term = nil
				return nil
			}
			nextHeartbeat = now.Add(instance.HeartbeatInterval)
			return nil
		}

		current, err := election.Current()
		if err == nil && !election.CanTakeOver(current) {
			return nil
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		token, err := election.NewToken()
		if err != nil {
			listener.Close()
			return err
		}
		_, acquired, err := election.TryAcquire(listener.Addr().String(), token)
		if err != nil || !acquired {
			listener.Close()
			return err
		}
		term, err = startPrimaryTerm(ctx, cfg, version, election, listener, token, restart, update)
		if err != nil {
			_ = election.Release(token)
			listener.Close()
			if errors.Is(err, errOwnershipLost) {
				return nil
			}
			return err
		}
		nextHeartbeat = time.Now().Add(instance.HeartbeatInterval)
		return nil
	}

	if err := step(); err != nil {
		return err
	}
	for {
		var primaryRequests <-chan string
		if term != nil {
			primaryRequests = term.service.PrimaryRequests()
		}
		select {
		case <-ctx.Done():
			return nil
		case targetID := <-primaryRequests:
			term.service.Runtime.LogEvent("info", "instance", "handoff", "Handing primary ownership to instance "+targetID)
			if err := term.handoff(targetID); err != nil {
				term.service.Runtime.LogEvent("error", "instance", "handoff_failed", "Primary handoff: "+err.Error())
			}
			term = nil
		case <-ticker.C:
			if err := step(); err != nil {
				return err
			}
		}
	}
}

func startPrimaryTerm(parent context.Context, original config.Config, version string, election *instance.Election, listener net.Listener, token string, restart, update func()) (*primaryTerm, error) {
	// A secondary may have waited for hours before taking over. Re-read YAML so
	// it never resurrects the snapshot from its own startup.
	cfg, err := config.Load(original.Path)
	if err != nil {
		runtimeState := app.NewRuntimeAt(original.StatePath("runtime", "logs"), election.ID()+"-reload")
		runtimeState.LogEvent("error", "config", "reload_failed", "Configuration reload before primary startup failed")
		runtimeState.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	service, err := buildService(cfg, version)
	if err != nil {
		cancel()
		return nil, err
	}
	service.Runtime.LogEvent("info", "config", "reloaded", "Configuration loaded for primary startup")
	service.SetRecoveryOwnershipFence(func(action func() error) (bool, error) {
		return election.RunWhileOwner(token, action)
	})
	service.SetPrimaryInstanceID(election.ID())
	if err := service.Start(ctx); err != nil {
		service.Runtime.LogEvent("error", "harness", "unavailable", "Harness unavailable: "+err.Error())
	}
	if _, owned, renewErr := election.Renew(token); renewErr != nil || !owned {
		_ = service.Close()
		cancel()
		if renewErr != nil {
			return nil, renewErr
		}
		return nil, errOwnershipLost
	}
	service.Runtime.LogEvent("info", "runtime", "primary_started", "Spynel primary server started")
	term := &primaryTerm{
		election: election, token: token, cancel: cancel, listener: listener, service: service,
		apiDone: make(chan struct{}), apiError: make(chan error, 1), orchestratorDone: make(chan error, 1),
	}
	reportConnection := func(status channel.ConnectionStatus) { service.SetConnectionStatus(status) }
	term.channelsDone, err = startChannels(ctx, service, reportConnection)
	if err != nil {
		_ = service.Close()
		cancel()
		return nil, err
	}
	apiServer := &localapi.Server{Service: service, Token: token}
	go func() {
		defer service.Runtime.RecoverPanic("localapi", "server_panic")
		err := apiServer.Serve(ctx, listener)
		term.apiError <- err
		close(term.apiDone)
	}()

	go func() {
		defer service.Runtime.RecoverPanic("orchestrator", "worker_panic")
		term.orchestratorDone <- runPrimaryOrchestrator(ctx, service)
	}()
	go func() {
		defer service.Runtime.RecoverPanic("runtime", "request_panic")
		select {
		case <-ctx.Done():
		case <-service.RestartRequests():
			restart()
		case <-service.UpdateRequests():
			update()
		}
	}()
	return term, nil
}

func runPrimaryOrchestrator(ctx context.Context, service *app.Service) error {
	if availability, ok := service.Harness.(harness.Availability); ok {
		if ready, _ := availability.Available(); !ready {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-availability.ReadyEvents():
			}
		}
	}
	return service.Orchestrator.Run(ctx)
}

func (term *primaryTerm) stop() {
	_ = term.stopFor("")
}

func (term *primaryTerm) handoff(targetID string) error {
	return term.stopFor(targetID)
}

func (term *primaryTerm) stopFor(targetID string) error {
	var stopErr error
	stopped := false
	term.stopOnce.Do(func() {
		defer term.service.Runtime.Close()
		stopped = true
		term.service.SetPrimaryInstanceID("")
		term.cancel()
		_ = term.listener.Close()
		_ = term.service.Harness.Close()
		<-term.apiDone
		<-term.channelsDone
		<-term.orchestratorDone
		term.service.Orchestrator.Wait()
		if targetID != "" {
			_, handedOff, err := term.election.Handoff(term.token, targetID)
			if err == nil && handedOff {
				return
			}
			if err == nil {
				err = errOwnershipLost
			}
			if releaseErr := term.election.Release(term.token); releaseErr != nil {
				err = errors.Join(err, releaseErr)
			}
			stopErr = err
			return
		}
		if err := term.election.Release(term.token); err != nil {
			term.service.Runtime.LogEvent("error", "instance", "lease_release_failed", "Release primary lease: "+err.Error())
		}
	})
	if !stopped && targetID != "" {
		return errOwnershipLost
	}
	return stopErr
}

func errorText(err error) string {
	if err == nil {
		return "without an error"
	}
	return err.Error()
}

type tuiStateEvents struct {
	connections chan channel.ConnectionStatus
	pairings    chan channel.PairingEvent
	notices     chan channel.Notice
	titles      chan string
	themes      chan theme.Theme
	runtime     chan core.RuntimeStatus
	durableWork chan core.DurableWorkCounts
	activity    chan core.Event
	activitySet chan int
}

func startTUIStatePolling(ctx context.Context, client *localapi.Client, initial app.SharedState, themeDirectory string, conversations ...string) tuiStateEvents {
	events := tuiStateEvents{
		connections: make(chan channel.ConnectionStatus, 4),
		pairings:    make(chan channel.PairingEvent, 4),
		notices:     make(chan channel.Notice, 4), titles: make(chan string, 1),
		themes: make(chan theme.Theme, 1), runtime: make(chan core.RuntimeStatus, 1),
		durableWork: make(chan core.DurableWorkCounts, 1),
		activity:    make(chan core.Event, 16),
		activitySet: make(chan int, 1),
	}
	conversation := ""
	if len(conversations) > 0 {
		conversation = conversations[0]
	}
	initialActivity := 0
	if conversation != "" {
		initialActivity = initial.ConversationActivity
	}
	go bridgeTUIActivity(ctx, events.activity, events.activitySet, initialActivity)
	go func() {
		previous := initial
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			state, err := client.State(ctx)
			if err != nil {
				continue
			}
			publishTUIStateChanges(events, previous, state, themeDirectory, conversation)
			previous = state
		}
	}()
	return events
}

func publishTUIStateChanges(events tuiStateEvents, previous, state app.SharedState, themeDirectory string, conversation ...string) {
	if state.Title != previous.Title {
		publishLatest(events.titles, state.Title)
	}
	if state.Theme != previous.Theme {
		if values, err := theme.LoadDir(themeDirectory); err == nil {
			if selected, ok := theme.Find(values, state.Theme); ok {
				publishLatest(events.themes, selected)
			}
		}
	}
	previousConnections := connectionStateMap(previous.Connections)
	for _, status := range state.Connections {
		if old, ok := previousConnections[status.Name]; !ok || old != status {
			publishLatest(events.connections, status)
		}
	}
	previousPairings := pairingStateMap(previous.Pairings)
	for _, pairing := range state.Pairings {
		if old, ok := previousPairings[pairing.Name]; !ok || old != pairing {
			publishLatest(events.pairings, pairing)
		}
	}
	if state.Runtime != previous.Runtime {
		publishLatest(events.runtime, state.Runtime)
	}
	if state.DurableWork != previous.DurableWork {
		publishLatest(events.durableWork, state.DurableWork)
	}
	if state.NoticeSequence != previous.NoticeSequence && state.Notice.Channel != "" {
		publishLatest(events.notices, state.Notice)
	}
	if len(conversation) > 0 {
		publishLatest(events.activitySet, state.ConversationActivity)
	}
}

// bridgeTUIActivity converts latest-value shared-state counts into the balanced
// event stream consumed by the TUI. Keeping the desired count separate from the
// output means neither startup nor polling can block behind an unstarted or slow
// TUI consumer, even when the overlap count exceeds the event buffer.
func bridgeTUIActivity(ctx context.Context, output chan<- core.Event, desired <-chan int, initial int) {
	current, target := 0, initial
	for {
		if current == target {
			select {
			case <-ctx.Done():
				return
			case target = <-desired:
			}
			continue
		}
		event := core.Event{Kind: core.EventActivity, Active: current < target}
		select {
		case <-ctx.Done():
			return
		case target = <-desired:
		case output <- event:
			if event.Active {
				current++
			} else {
				current--
			}
		}
	}
}

func connectionStateMap(values []channel.ConnectionStatus) map[string]channel.ConnectionStatus {
	result := make(map[string]channel.ConnectionStatus, len(values))
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}

func pairingStateMap(values []channel.PairingEvent) map[string]channel.PairingEvent {
	result := make(map[string]channel.PairingEvent, len(values))
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}

func publishLatest[T any](target chan T, value T) {
	select {
	case <-target:
	default:
	}
	select {
	case target <- value:
	default:
	}
}
