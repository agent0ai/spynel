package channel

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/workspace"
)

type supervisedFixture struct {
	name    string
	started chan string
	stopped chan string
}

func (c *supervisedFixture) Name() string { return "telegram" }

func (c *supervisedFixture) Run(ctx context.Context, _ Handler) error {
	c.started <- c.name
	<-ctx.Done()
	c.stopped <- c.name
	return ctx.Err()
}

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
}
