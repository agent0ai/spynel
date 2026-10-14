//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestServerRetainsHandledTerminationBoundary(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harness.Name = ""
	cfg.Orchestrator.Enabled = false
	cfg.Orchestrator.RetriggerUnrespondedMessages = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "__server_fixture", cfg.Path)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "SPYNEL_NPM_") && !strings.HasPrefix(value, "SPYNEL_DIAGNOSTICS_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "XDG_CONFIG_HOME="+filepath.Join(root, "user-config"), "HOME="+filepath.Join(root, "user-home"))
	var terminal bytes.Buffer
	command.Stdout, command.Stderr = &terminal, &terminal
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() { _ = command.Process.Kill() }()
	for {
		ready := false
		for _, entry := range readDiagnosticEntries(t, cfg.StatePath("runtime", "logs")) {
			ready = ready || entry.Event == "primary_started"
		}
		if ready {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server exited before readiness: %v %s", err, terminal.String())
		case <-ctx.Done():
			t.Fatal("synthetic server did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("handled termination changed exit status: %v %s", err, terminal.String())
	}
	for _, entry := range readDiagnosticEntries(t, cfg.StatePath("runtime", "logs")) {
		if entry.Event == "process_end" && strings.Contains(entry.Text, "reason=handled_signal") && strings.Contains(entry.Text, "result=success") {
			return
		}
	}
	t.Fatal("handled signal was not distinguished from normal quit")
}
