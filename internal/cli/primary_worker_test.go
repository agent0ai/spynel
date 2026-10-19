//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/localapi"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestPrimaryWorkerRecoversHarnessInstalledAfterFirstLaunch(t *testing.T) {
	for _, scenario := range []struct{ name, initial, selected string }{
		{"builtin", "codex", "codex"}, {"custom", "acp", "acp"}, {"changed-selection", "codex", "acp"},
		{"broken-provider", "codex", "codex"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			t.Setenv("SPYNEL_TEST_WORKER_PROVIDER", "1")
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := workspace.Init(root, false); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.PathForRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			command := filepath.Join(bin, "codex")
			if scenario.selected == "acp" {
				command = filepath.Join(bin, "custom provider")
			}
			cfg.Harness.Name, cfg.Harness.ACPCommand = scenario.initial, command
			cfg.Harness.ReasoningEffort = ""
			cfg.Orchestrator.Enabled, cfg.Orchestrator.RetriggerUnrespondedMessages = false, false
			cfg.Orchestrator.SemanticHeartbeatMinutes = 0
			evidence := filepath.Join(root, "synthetic-methods")
			t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", evidence)
			var term *primaryTerm
			var identity harness.WorkerInfo
			expectedTurns := 0
			t.Cleanup(func() {
				if term != nil {
					_ = term.stop()
				}
				// Only this fixture's native-verified independently surviving child.
				if identity.PID != 0 && updater.VerifyProcess(identity.Process) == nil {
					process, _ := os.FindProcess(identity.PID)
					_ = process.Signal(syscall.SIGTERM)
					waitWorkerAgent(t, func() bool { _, err := os.Stat(filepath.Dir(identity.Socket)); return os.IsNotExist(err) })
				}
			})
			for stage := 0; stage < 3; stage++ {
				if stage == 1 {
					cfg.Harness.Name = scenario.selected
				}
				if scenario.name == "broken-provider" && stage < 2 {
					if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if stage < 2 {
					if err := config.Save(cfg); err != nil {
						t.Fatal(err)
					}
					file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = file.WriteString("\n# retain startup comments\nunused_setup_key: kept\n")
					_ = file.Close()
				} else {
					executable, _ := os.Executable()
					if scenario.name == "broken-provider" {
						if err := os.Remove(command); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(executable, command); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := os.ReadFile(cfg.Path)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				owner, err := instance.New(cfg.StatePath())
				if err != nil {
					t.Fatal(err)
				}
				token, _ := owner.NewToken()
				if _, acquired, err := owner.TryAcquire(listener.Addr().String(), token); err != nil || !acquired {
					t.Fatal("setup primary ownership", err)
				}
				t.Cleanup(func() { _ = owner.Release(token) })
				term, err = startPrimaryTerm(t.Context(), cfg, "synthetic-version", owner, listener, token, nil, nil)
				if err != nil {
					t.Fatalf("primary restart at setup stage %d: %v", stage, err)
				}
				worker := term.service.Orchestrator.Worker
				if stage == 0 {
					identity = worker.Info()
				} else if worker.Info().Process != identity.Process || worker.Info().ID != identity.ID {
					t.Fatal("onboarding replaced the independent worker")
				}
				inspection, err := worker.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
				if err != nil || inspection.Availability == nil || inspection.Availability.Available != (stage == 2) || len(inspection.Executions) != 0 || inspection.NextSequence != 1 {
					t.Fatal("setup lost availability, retained work, or admitted a provider turn", err)
				}
				api := localapi.NewClient(owner)
				status, err := api.Status(t.Context(), "cli/setup")
				api.HTTP.CloseIdleConnections()
				expectedState := "unavailable"
				if stage == 2 {
					expectedState = "connected"
				}
				if err != nil || status.HarnessState != expectedState {
					t.Fatal("configuration/status API did not reflect onboarding availability", err)
				}
				if after, _ := os.ReadFile(cfg.Path); !bytes.Equal(before, after) {
					t.Fatal("onboarding rewrote canonical settings")
				}
				if stage == 2 && scenario.name == "builtin" {
					if _, err := worker.Call(t.Context(), harness.WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/binding", Prompt: "explicit retained work"}); err != nil {
						t.Fatal(err)
					}
					expectedTurns = 1
					changed := cfg
					changed.Harness.Sandbox = "read-only"
					if err := config.Save(changed); err != nil {
						t.Fatal(err)
					}
					settings, err := changed.WorkerSettings()
					if err != nil {
						t.Fatal(err)
					}
					runtime, _ := changed.RuntimeHarness(*inspection.Config)
					if replacement, err := harness.OpenWorker(t.Context(), runtime, owner.ID(), token, settings); err == nil {
						replacement.Close()
						t.Fatal("automatic reconciliation replaced a retained harness")
					}
					retained, err := worker.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
					if err != nil || retained.ConfigDigest != inspection.ConfigDigest || len(retained.Executions) != 1 || retained.Executions[0].Done || retained.NextSequence != 2 {
						t.Fatal("refused reconciliation changed or replayed retained work", err)
					}
					if err := os.WriteFile(cfg.Path, before, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := term.stop(); err != nil {
					t.Fatal(err)
				}
				term = nil
			}
			methods, _ := os.ReadFile(evidence)
			if strings.Count(string(methods), "turn/start\n") != expectedTurns || strings.Contains(string(methods), "session/prompt") {
				t.Fatal("onboarding dispatched an unsolicited provider turn")
			}
		})
	}
}

func TestPrimaryWorkerSurvivesOrdinaryControllerReplacement(t *testing.T) {
	testPrimaryWorkerContinuity(t, []string{"synthetic-version", "synthetic-version", "synthetic-version"})
}

func TestPrimaryWorkerSurvivesVersionChangingControllerReplacement(t *testing.T) {
	testPrimaryWorkerContinuity(t, []string{"synthetic-version", "synthetic-next-version", "synthetic-final-version"})
}

func testPrimaryWorkerContinuity(t *testing.T, versions []string) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetSettings(&cfg, map[string]string{"harness.name": "codex", "harness.reasoning_effort": "inherit", "harness.sandbox": "read-only"}); err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.IntervalSec, cfg.Orchestrator.SemanticHeartbeatMinutes = 1, 0
	cfg.Orchestrator.RetriggerUnrespondedMessages = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	// Only the provider image enters fixture protocol mode; controllers and
	// independent workers run their ordinary native entry points.
	script := "#!/bin/sh\nSPYNEL_TEST_WORKER_PROVIDER=1 exec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	methods := filepath.Join(root, "synthetic-methods")
	t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", methods)
	owner, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	api := localapi.NewClient(owner)
	defer api.HTTP.CloseIdleConnections()
	var controller *exec.Cmd
	var controllerDone chan error
	version := versions[0]
	start := func() {
		t.Helper()
		log, err := os.CreateTemp(root, "controller-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		controller = exec.Command(executable, "__primary_worker_fixture", cfg.Path, version)
		controller.Stdout, controller.Stderr = log, log
		if err := controller.Start(); err != nil {
			t.Fatal(err)
		}
		controllerDone = make(chan error, 1)
		go func(cmd *exec.Cmd, done chan error) { done <- cmd.Wait() }(controller, controllerDone)
		ctx, cancel := context.WithTimeout(t.Context(), localapi.ReadinessTimeout)
		defer cancel()
		if _, err := api.WaitReady(ctx); err != nil {
			t.Fatal("ordinary primary readiness", err)
		}
		status, err := api.Status(ctx, "cli/surviving")
		if err != nil || status.Version != version {
			t.Fatal("ordinary controller did not serve its selected version", err)
		}
	}
	stop := func(signal os.Signal) {
		t.Helper()
		if err := controller.Process.Signal(signal); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-controllerDone:
			if signal == syscall.SIGTERM && err != nil {
				t.Fatal("graceful controller stop", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("controller did not exit")
		}
		controller = nil
	}
	var worker *harness.WorkerClient
	t.Cleanup(func() {
		if worker != nil {
			// This is authenticated fixture cleanup, never a PID-only signal.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := worker.Call(ctx, harness.WorkerRequest{Action: "shutdown"}); err != nil {
				t.Error("worker fixture shutdown", err)
			}
			worker.Close()
		}
		if controller != nil {
			_ = controller.Process.Signal(syscall.SIGTERM)
			select {
			case <-controllerDone:
			case <-time.After(10 * time.Second):
				_ = controller.Process.Kill()
				<-controllerDone
				t.Error("controller fixture required forced cleanup")
			}
		}
		if worker != nil {
			waitWorkerAgent(t, func() bool { _, err := os.Stat(filepath.Dir(worker.Info().Socket)); return os.IsNotExist(err) })
			waitWorkerAgent(t, func() bool { return errors.Is(updater.VerifyProcess(worker.Info().Process), syscall.ESRCH) })
		}
	})
	start()
	lock, err := harness.ProcessOwnershipPath(root)
	if err != nil {
		t.Fatal(err)
	}
	// Check normal startup created the rendezvous before calling OpenWorker;
	// otherwise a test-only launch could conceal missing production binding.
	data, err := os.ReadFile(lock + ".worker.json")
	var record struct{ Info harness.WorkerInfo }
	if err != nil || json.Unmarshal(data, &record) != nil || record.Info.PID <= 1 {
		t.Fatal("ordinary primary did not bind an independent worker", err)
	}
	open := func() {
		t.Helper()
		lease, err := owner.Current()
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := cfg.RuntimeHarness(harness.HarnessConfig{Version: version})
		if err != nil {
			t.Fatal(err)
		}
		worker, err = harness.OpenWorker(t.Context(), runtime, lease.InstanceID, lease.Token)
		if err != nil {
			t.Fatal(err)
		}
		if worker.Info().Process != record.Info.Process || worker.Info().ID != record.Info.ID {
			t.Fatal("controller replacement changed native worker identity")
		}
	}
	open()
	t.Logf("ordinary worker pid=%d start=%s generation=%s", worker.Info().PID, worker.Info().Process.StartTime, worker.Info().Process.Generation)
	request, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	requestDone := make(chan error, 1)
	go func() {
		requestDone <- api.Handle(request, core.Message{Channel: "cli", Conversation: "surviving", Text: "synthetic long-running request", SourceMessageID: "source"}, func(core.Event) {})
	}()
	inspect := func() harness.WorkerReply {
		t.Helper()
		reply, err := worker.Call(t.Context(), harness.WorkerRequest{Action: "inspect"})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 1 && inspect().Executions[0].ThreadID != "" })
	initial := inspect().Executions[0]
	for index, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		obsolete := worker
		stop(signal)
		if signal == syscall.SIGTERM {
			select {
			case <-requestDone:
			case <-time.After(5 * time.Second):
				t.Fatal("controller shutdown did not release its live request")
			}
			cancelRequest()
		}
		if err := updater.VerifyProcess(record.Info.Process); err != nil {
			t.Fatal("controller exit killed the independent worker", err)
		}
		version = versions[index+1]
		start() // Abrupt exit uses ordinary stale takeover, without lease edits.
		open()
		if _, err := obsolete.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: initial.Key}); err == nil {
			t.Fatal("obsolete controller retained control")
		}
		obsolete.Close()
		current := inspect().Executions
		if len(current) != 1 || current[0].Sequence != initial.Sequence || current[0].ThreadID != initial.ThreadID || current[0].Done || current[0].ConversationTurns != initial.ConversationTurns {
			t.Fatal("ordinary primary replaced or completed the retained provider turn")
		}
	}
	// Settings must use the retained worker's runtime after a version change.
	if err := api.ApplySettings(t.Context(), map[string]string{"harness.model": "model-a"}); err != nil {
		t.Fatal("successor canonical settings", err)
	}
	configured := inspect()
	if configured.Config == nil || configured.Config.Version != versions[0] || configured.Config.Model != "model-a" {
		t.Fatal("successor settings changed worker image metadata or lost the selection")
	}
	if err := api.Handle(t.Context(), core.Message{Channel: "cli", Conversation: "surviving", Text: "/stop", SourceMessageID: "stop"}, func(core.Event) {}); err != nil {
		t.Fatal("successor application interruption", err)
	}
	waitWorkerAgent(t, func() bool { return len(inspect().Executions) == 0 })
	observed, err := os.ReadFile(methods)
	if err != nil || strings.Count(string(observed), "turn/start\n") != 1 || strings.Count(string(observed), "turn/interrupt\n") != 1 {
		t.Fatal("ordinary replacement duplicated provider admission or control", err)
	}
}
