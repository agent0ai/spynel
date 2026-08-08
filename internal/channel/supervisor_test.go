package channel

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
	cfg, err := config.Load(filepath.Join(root, config.FileName))
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
