package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/media"
	"github.com/agent0ai/spynel/internal/workspace"
)

type channelRoundTrip func(*http.Request) (*http.Response, error)

func (f channelRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagedChannelsRejectSavedRevocationBeforeNativeDelivery(t *testing.T) {
	transport := http.DefaultTransport
	requests := 0
	http.DefaultTransport = channelRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		_ = r.Body.Close()
		return nil, errors.New("synthetic provider reached")
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = "synthetic-token"
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	cfg.Channels.WhatsApp.Enabled = true
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654321"}
	service := app.New(cfg, &heldCLIHarness{})
	t.Cleanup(func() { _ = service.Close() })
	path := filepath.Join(root, "fixture.txt")
	if err := os.WriteFile(path, []byte("harmless document"), 0600); err != nil {
		t.Fatal(err)
	}
	_, attachments, err := media.ParseOutbound("[Send attachment](<"+path+">)", 1024)
	if err != nil {
		t.Fatal(err)
	}
	var adapters []channel.Channel
	for _, managed := range managedChannels(service, nil) {
		adapter, err := managed.Build(cfg)
		if err != nil {
			t.Fatal(err)
		}
		adapters = append(adapters, adapter)
	}
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Channels.Telegram.AllowedUsers = []string{"8"}
		next.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, adapter := range adapters {
		t.Run(adapter.Name(), func(t *testing.T) {
			sender := adapter.(interface {
				Deliver(context.Context, string, string, string, []core.OutboundAttachment) error
			})
			origin := []string{"TG-7", "WA-15557654321"}[i]
			err := sender.Deliver(context.Background(), origin, "synthetic-event", "", attachments)
			if err == nil || !strings.Contains(err.Error(), "authorization") {
				t.Fatalf("saved revocation did not reject before native delivery: %v", err)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("revocation contacted the mocked provider %d times", requests)
	}
	for _, managed := range managedChannels(service, nil) {
		adapter, err := managed.Build(service.Settings.Snapshot())
		if err != nil {
			t.Fatal(err)
		}
		authorizer := adapter.(interface{ ValidateRuntimeAuthorization() error })
		if err := authorizer.ValidateRuntimeAuthorization(); err != nil {
			t.Fatalf("replacement authorization failed: %v", err)
		}
		if _, err := service.Settings.Update(func(next *config.Config) error {
			next.Channels.Telegram.Enabled = false
			next.Channels.WhatsApp.Enabled = false
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := authorizer.ValidateRuntimeAuthorization(); err == nil {
			t.Fatal("disabled channel retained authorization")
		}
		if _, err := service.Settings.Update(func(next *config.Config) error {
			next.Channels.Telegram.Enabled = true
			next.Channels.WhatsApp.Enabled = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
