package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/workspace"
)

type supervisedFixture struct {
	name    string
	started chan string
	stopped chan string
	report  StatusReporter
}

type pairingFixture struct {
	retried bool
	phone   string
}

type runtimeAuthorizationFixture struct {
	valid   bool
	started atomic.Int32
	revoked atomic.Bool
}

type losesAuthorizationFixture struct {
	valid   atomic.Bool
	started chan struct{}
}

type statusFixture struct {
	report StatusReporter
}

func (c *statusFixture) Name() string { return "whatsapp" }

func (c *statusFixture) Run(ctx context.Context, _ Handler) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *statusFixture) SetStatusReporter(report StatusReporter) { c.report = report }

func (c *losesAuthorizationFixture) Name() string { return "telegram" }

func (c *losesAuthorizationFixture) Run(context.Context, Handler) error {
	c.valid.Store(false)
	close(c.started)
	return errors.New("live allow-list disappeared")
}

func (c *losesAuthorizationFixture) ValidateRuntimeAuthorization() error {
	if !c.valid.Load() {
		return errors.New("Telegram runtime authorization is unavailable")
	}
	return nil
}

func (c *losesAuthorizationFixture) RevokeRuntimeAuthorization() { c.valid.Store(false) }

func (c *runtimeAuthorizationFixture) Name() string { return "telegram" }

func (c *runtimeAuthorizationFixture) Run(ctx context.Context, _ Handler) error {
	c.started.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func (c *runtimeAuthorizationFixture) ValidateRuntimeAuthorization() error {
	if !c.valid {
		return errors.New("Telegram runtime authorization is unavailable")
	}
	return nil
}

func (c *runtimeAuthorizationFixture) RevokeRuntimeAuthorization() { c.revoked.Store(true) }

func (c *pairingFixture) Name() string { return "whatsapp" }

func (c *pairingFixture) Run(ctx context.Context, _ Handler) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *pairingFixture) RetryPairing() error {
	c.retried = true
	return nil
}

func (c *pairingFixture) PairPhone(_ context.Context, phone string) (string, error) {
	c.phone = phone
	return "ABCD-EFGH", nil
}

func (c *supervisedFixture) Name() string { return "telegram" }

func (c *supervisedFixture) Run(ctx context.Context, _ Handler) error {
	c.started <- c.name
	if c.report != nil {
		c.report(ConnectionStatus{Name: "telegram", State: ConnectionConnected})
	}
	<-ctx.Done()
	c.stopped <- c.name
	return ctx.Err()
}

func (c *supervisedFixture) SetStatusReporter(report StatusReporter) { c.report = report }

func TestSupervisorHotReloadsAndDisablesChannels(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	settings := config.NewStore(cfg)
	started := make(chan string, 4)
	stopped := make(chan string, 4)
	managed := []Managed{{
		Name:        "telegram",
		Enabled:     func(cfg config.Config) bool { return cfg.Channels.Telegram.Enabled },
		Fingerprint: func(cfg config.Config) string { return cfg.Channels.Telegram.Token },
		Build: func(cfg config.Config) (Channel, error) {
			return &supervisedFixture{name: cfg.Channels.Telegram.Token, started: started, stopped: stopped}, nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor := NewSupervisor(settings, nil, managed, nil, nil)
	events := make(chan string, 16)
	supervisor.SetEventLogger(func(_, component, event, _ string) { events <- component + "/" + event })
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()

	update := func(enabled bool, token string) {
		if _, err := settings.Update(func(next *config.Config) error {
			next.Channels.Telegram.Enabled = enabled
			next.Channels.Telegram.Token = token
			if enabled {
				next.Channels.Telegram.AllowedUsers = []string{"7"}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(channel <-chan string, want string) {
		t.Helper()
		select {
		case got := <-channel:
			if got != want {
				t.Fatalf("lifecycle event = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal(fmt.Sprintf("timed out waiting for %q", want))
		}
	}
	update(true, "one")
	wait(started, "one")
	update(true, "two")
	wait(stopped, "one")
	wait(started, "two")
	update(false, "two")
	wait(stopped, "two")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	close(events)
	var lifecycle []string
	for event := range events {
		lifecycle = append(lifecycle, event)
	}
	joined := strings.Join(lifecycle, " ")
	for _, want := range []string{"channel.telegram/connecting", "channel.telegram/connected", "channel.telegram/reconnecting", "channel.telegram/disconnected"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("channel lifecycle lacks %q: %v", want, lifecycle)
		}
	}
}

func TestSupervisorRoutesPairingActionsToRunningChannel(t *testing.T) {
	fixture := &pairingFixture{}
	supervisor := NewSupervisor(nil, nil, nil, nil, nil)
	supervisor.running["whatsapp"] = &runningChannel{instance: fixture}
	if err := supervisor.RetryPairing("whatsapp"); err != nil || !fixture.retried {
		t.Fatalf("retry pairing = retried %t, err %v", fixture.retried, err)
	}
	code, err := supervisor.PairPhone(context.Background(), "whatsapp", "15551234567")
	if err != nil || code != "ABCD-EFGH" || fixture.phone != "15551234567" {
		t.Fatalf("phone pairing = code %q phone %q err %v", code, fixture.phone, err)
	}
	if err := supervisor.RetryPairing("telegram"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("missing pairing channel error = %v", err)
	}
}

func TestSupervisorLogsStatusTransitionsPerGeneration(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = true
	cfg.Channels.WhatsApp.Mode = "dedicated"
	var fixtures []*statusFixture
	managed := []Managed{{
		Name:        "whatsapp",
		Enabled:     func(config.Config) bool { return true },
		Fingerprint: func(cfg config.Config) string { return cfg.Channels.WhatsApp.Mode },
		Build: func(config.Config) (Channel, error) {
			fixture := &statusFixture{}
			fixtures = append(fixtures, fixture)
			return fixture, nil
		},
	}}
	var statuses []ConnectionStatus
	var events []string
	supervisor := NewSupervisor(nil, nil, managed, func(status ConnectionStatus) {
		statuses = append(statuses, status)
	}, nil)
	supervisor.SetEventLogger(func(_, component, event, _ string) {
		events = append(events, component+"/"+event)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		supervisor.stopAll()
		supervisor.wait.Wait()
	}()

	if err := supervisor.reconcile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	first := fixtures[0].report
	connected := ConnectionStatus{Name: "whatsapp", State: ConnectionConnected, Identity: "+15551234567", Link: "https://wa.me/15551234567"}
	first(ConnectionStatus{Name: "whatsapp", State: ConnectionConnecting})
	first(ConnectionStatus{Name: "whatsapp", State: ConnectionConnecting, Detail: "waiting for pairing"})
	for range 20 {
		first(connected)
	}
	first(ConnectionStatus{Name: "whatsapp", State: ConnectionConnected, Detail: "metadata refreshed", Identity: connected.Identity, Link: connected.Link})
	first(ConnectionStatus{Name: "whatsapp", State: ConnectionError, Detail: "disconnected"})
	first(ConnectionStatus{Name: "whatsapp", State: ConnectionError, Detail: "disconnected"})
	first(connected)
	first(connected)

	beforeReplacementStatuses := len(statuses)
	cfg.Channels.WhatsApp.Mode = "self"
	if err := supervisor.reconcile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	fixtures[1].report(connected)
	first(ConnectionStatus{Name: "whatsapp", State: ConnectionError, Detail: "stale callback"})

	wantEvents := []string{
		"channel.whatsapp/connecting",
		"channel.whatsapp/connected",
		"channel.whatsapp/connection_error",
		"channel.whatsapp/connected",
		"channel.whatsapp/reconnecting",
		"channel.whatsapp/connecting",
		"channel.whatsapp/connected",
	}
	if got := strings.Join(events, " "); got != strings.Join(wantEvents, " ") {
		t.Fatalf("lifecycle events = %q, want %q", got, strings.Join(wantEvents, " "))
	}
	if len(statuses) != beforeReplacementStatuses+2 {
		t.Fatalf("replacement status count = %d, want %d; stale callback may have escaped", len(statuses), beforeReplacementStatuses+2)
	}
	if got := statuses[len(statuses)-1]; got != connected {
		t.Fatalf("last status = %#v, want replacement generation %#v", got, connected)
	}
}

func TestSupervisorKeepsInvalidAuthorizationInPersistentErrorWithoutRetry(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.Telegram.Enabled = true
	builds := 0
	fixture := &runtimeAuthorizationFixture{}
	managed := []Managed{{
		Name: "telegram", Enabled: func(config.Config) bool { return true }, Fingerprint: func(config.Config) string { return "invalid" },
		Build: func(config.Config) (Channel, error) { builds++; return fixture, nil },
	}}
	var statuses []ConnectionStatus
	supervisor := NewSupervisor(nil, nil, managed, func(status ConnectionStatus) { statuses = append(statuses, status) }, nil)
	if err := supervisor.reconcile(context.Background(), cfg); err == nil {
		t.Fatal("invalid runtime authorization was not reported")
	}
	if err := supervisor.reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("stable invalid fingerprint was retried: %v", err)
	}
	if builds != 1 || fixture.started.Load() != 0 {
		t.Fatalf("invalid runtime builds=%d starts=%d", builds, fixture.started.Load())
	}
	if len(statuses) != 1 || statuses[0].State != ConnectionError {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestSupervisorRevokesStaleRuntimeBeforeHotReplacement(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = "one"
	var fixtures []*runtimeAuthorizationFixture
	managed := []Managed{{
		Name: "telegram", Enabled: func(config.Config) bool { return true }, Fingerprint: func(cfg config.Config) string { return cfg.Channels.Telegram.Token },
		Build: func(config.Config) (Channel, error) {
			fixture := &runtimeAuthorizationFixture{valid: true}
			fixtures = append(fixtures, fixture)
			return fixture, nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := NewSupervisor(nil, nil, managed, nil, nil)
	if err := supervisor.reconcile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Channels.Telegram.Token = "two"
	if err := supervisor.reconcile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 || !fixtures[0].revoked.Load() || fixtures[1].revoked.Load() {
		t.Fatalf("hot replacement fixtures = %#v", fixtures)
	}
	cancel()
	supervisor.stopAll()
	supervisor.wait.Wait()
}

func TestSupervisorKeepsCurrentGenerationWhenCandidateBuildFails(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = "working"
	started := make(chan string, 2)
	stopped := make(chan string, 2)
	managed := []Managed{{
		Name:        "telegram",
		Enabled:     func(config.Config) bool { return true },
		Fingerprint: func(cfg config.Config) string { return cfg.Channels.Telegram.Token },
		Build: func(cfg config.Config) (Channel, error) {
			if cfg.Channels.Telegram.Token == "broken" {
				return nil, errors.New("candidate rejected")
			}
			return &supervisedFixture{name: cfg.Channels.Telegram.Token, started: started, stopped: stopped}, nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor := NewSupervisor(nil, nil, managed, nil, nil)
	if err := supervisor.reconcile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	<-started
	cfg.Channels.Telegram.Token = "broken"
	if err := supervisor.reconcile(ctx, cfg); err == nil {
		t.Fatal("broken candidate was accepted")
	}
	select {
	case <-stopped:
		t.Fatal("current generation stopped after candidate failure")
	case <-time.After(25 * time.Millisecond):
	}
	supervisor.stopAll()
	supervisor.wait.Wait()
}

func TestSupervisorDoesNotRetryWhenLiveAuthorizationDisappears(t *testing.T) {
	cfg := config.Default()
	builds := 0
	fixture := &losesAuthorizationFixture{started: make(chan struct{})}
	fixture.valid.Store(true)
	managed := []Managed{{
		Name: "telegram", Enabled: func(config.Config) bool { return true }, Fingerprint: func(config.Config) string { return "same" },
		Build: func(config.Config) (Channel, error) { builds++; return fixture, nil },
	}}
	statuses := make(chan ConnectionStatus, 4)
	supervisor := NewSupervisor(nil, nil, managed, func(status ConnectionStatus) { statuses <- status }, nil)
	if err := supervisor.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.started:
	case <-time.After(time.Second):
		t.Fatal("fixture did not start")
	}
	for {
		select {
		case status := <-statuses:
			if status.State == ConnectionError {
				goto observedError
			}
		case <-time.After(time.Second):
			t.Fatal("authorization error status was not retained")
		}
	}

observedError:
	if err := supervisor.reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("stable failed runtime was retried: %v", err)
	}
	if builds != 1 {
		t.Fatalf("live authorization loss triggered %d builds", builds)
	}
	supervisor.stopAll()
	supervisor.wait.Wait()
}
