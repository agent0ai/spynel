//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestNativeUnknownFollowupDeliveryKeepsTerminalCoverage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "synthetic-methods")
	completion := filepath.Join(root, "complete")
	t.Setenv("SPYNEL_TEST_WORKER_PROVIDER", "1")
	t.Setenv("SPYNEL_TEST_WORKER_UNCERTAIN_STEER", "1")
	t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", log)
	t.Setenv("SPYNEL_TEST_WORKER_COMPLETIONS", completion)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	supervisor := harness.NewSupervisor(harness.NewBuiltinRegistry(), harness.HarnessConfig{
		Name: "codex", Command: executable, Cwd: root, Sandbox: "read-only", ApprovalPolicy: "never",
	})
	lockPath, err := harness.ProcessOwnershipPath(root)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.SetProcessStartGuard(lockPath, nil)
	service := app.New(cfg, supervisor)
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error("native provider/guardian cleanup:", err)
		}
	})
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := core.Message{Channel: "cli", Conversation: "uncertain", SourceMessageID: "first", Text: "synthetic original"}
	second := core.Message{Channel: first.Channel, Conversation: first.Conversation, SourceMessageID: "second", Text: "synthetic guidance"}
	var terminal atomic.Int32
	if err := service.Handle(t.Context(), first, func(event core.Event) {
		if event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			terminal.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	err = service.Handle(t.Context(), second, nil)
	if err == nil || !strings.Contains(err.Error(), "synthetic acknowledgement failed after effect") || !supervisor.IsActive("chat:cli:uncertain") {
		t.Fatal("uncertain delivery lost its original execution or error", err)
	}
	admissionErr := err
	if err := os.WriteFile(completion+".1", nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitWorkerAgent(t, func() bool { return terminal.Load() == 1 })
	snapshot, err := service.History.RecoveryWindow(first.Channel, first.Conversation, time.Time{}, 20)
	if err != nil || snapshot.Covered["first"] != "terminal_coverage" || snapshot.Covered["second"] != "terminal_coverage" {
		t.Fatal("applied native guidance escaped terminal coverage", snapshot.Covered, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	starts, steers, effects, providers := 0, 0, 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		switch line {
		case "turn/start":
			starts++
		case "turn/steer":
			steers++
		case "followup-effect":
			effects++
		default:
			if strings.HasPrefix(line, "provider:") {
				providers++
				t.Log(line)
			}
		}
	}
	if starts != 1 || steers != 1 || effects != 1 || providers != 1 || terminal.Load() != 1 {
		t.Fatalf("native work duplicated: providers=%d starts=%d steers=%d effects=%d terminal=%d", providers, starts, steers, effects, terminal.Load())
	}
	if !errors.Is(admissionErr, orchestrator.ErrConversationPending) {
		t.Fatal("uncertain admission was not retained for reconciliation", admissionErr)
	}
	t.Log("one native provider turn, applied follow-up, original terminal and exact source coverage")
}
