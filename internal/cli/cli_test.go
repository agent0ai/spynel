package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frdel/spynel/internal/channel"
	"github.com/frdel/spynel/internal/config"
	"github.com/frdel/spynel/internal/harness"
	"github.com/frdel/spynel/internal/workspace"
)

func TestCompleteRunReplacesProcessForRestartRequest(t *testing.T) {
	want := []string{"serve", "--tui", "--config", "/tmp/project/spynel.yaml"}
	request := &restartRequest{args: append([]string(nil), want...)}
	called := false
	err := completeRun(fmt.Errorf("server stopped: %w", request), func(args []string) error {
		called = true
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("restart arguments = %#v, want %#v", args, want)
		}
		args[0] = "mutated"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("restart process function was not called")
	}
	if request.args[0] != "serve" {
		t.Fatal("restart request arguments were exposed to mutation")
	}
}

func TestCompleteRunPreservesOrdinaryErrors(t *testing.T) {
	want := errors.New("failed")
	called := false
	got := completeRun(want, func([]string) error {
		called = true
		return nil
	})
	if !errors.Is(got, want) || called {
		t.Fatalf("completeRun error = %v, restart called = %t", got, called)
	}
}

func TestInitialConnectionStatusesReflectConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.Telegram.Enabled = true

	statuses := initialConnectionStatuses(cfg)
	if len(statuses) != 2 {
		t.Fatalf("status count = %d", len(statuses))
	}
	if statuses[0].Name != "telegram" || statuses[0].State != channel.ConnectionConnecting {
		t.Fatalf("Telegram status = %#v", statuses[0])
	}
	if statuses[1].Name != "whatsapp" || statuses[1].State != channel.ConnectionUnconfigured {
		t.Fatalf("WhatsApp status = %#v", statuses[1])
	}
}

func TestInitNoStartCreatesWorkspaceWithoutEnteringTUI(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-workspace")
	if err := Run([]string{"init", "--no-start", "--dir", root}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, config.FileName)); err != nil {
		t.Fatalf("initialized config: %v", err)
	}
	if !strings.Contains(helpText, "--no-start") || !strings.Contains(helpText, "continue into the TUI") {
		t.Fatalf("init continuation is not documented in help:\n%s", helpText)
	}
}

func TestBuildServiceUsesConfiguredHarnessSandbox(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Sandbox = "read-only"
	service, err := buildService(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Harness.Close()
	runtimeHarness, ok := service.Harness.(interface {
		HarnessConfig() harness.HarnessConfig
	})
	if !ok || runtimeHarness.HarnessConfig().Sandbox != "read-only" {
		t.Fatalf("runtime sandbox = %#v, configurable = %t", runtimeHarness, ok)
	}
}
