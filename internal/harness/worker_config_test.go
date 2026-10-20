package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/updater"
)

func newWorkerConfigurationHost(t *testing.T) (*workerHost, *inferenceSupervisorHarness) {
	t.Helper()
	base := &supervisorHarness{name: "configured", followUp: FollowUpQueue, active: map[string]bool{}, emits: map[string]core.Emit{}}
	target := &inferenceSupervisorHarness{supervisorHarness: base}
	registry := NewRegistry()
	registry.Register("configured", func(HarnessConfig) (Harness, error) { return target, nil })
	registry.Register("replacement", func(HarnessConfig) (Harness, error) {
		return &supervisorHarness{name: "replacement", active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
	})
	registry.Register("broken", func(HarnessConfig) (Harness, error) {
		return &supervisorHarness{startErr: errors.New("synthetic start failure")}, nil
	})
	cfg := HarnessConfig{Name: "configured", Cwd: t.TempDir(), Model: "old", Effort: "medium", ServiceMode: "default"}
	supervisor := NewSupervisor(registry, cfg)
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info := WorkerInfo{Protocol: WorkerProtocol, ID: strings.Repeat("a", 32), PID: os.Getpid(), Socket: filepath.Join(cfg.Cwd, "control.sock"), ConfigDigest: ConfigDigest(cfg), Process: updater.ProcessRegistration{PID: os.Getpid(), HarnessWorker: true, Generation: strings.Repeat("b", 32), StartTime: "synthetic", Executable: executable}}
	host := &workerHost{supervisor: supervisor, info: info, token: strings.Repeat("c", 32), recordPath: filepath.Join(cfg.Cwd, "worker.json"), stop: make(chan struct{}), executions: map[uint64]*workerExecution{}}
	data, _ := json.Marshal(workerRecord{Info: info, Token: host.token})
	if err := fsx.AtomicWriteFile(host.recordPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return host, target
}

func TestWorkerConfigurationOrdersQueuedInferenceAndPublication(t *testing.T) {
	host, target := newWorkerConfigurationHost(t)
	previous := host.supervisor.HarnessConfig()
	for _, prompt := range []string{"active", "queued"} {
		if _, _, err := host.supervisor.Send(t.Context(), "chat", prompt, nil); err != nil {
			t.Fatal(err)
		}
	}
	next := previous
	next.Model = "new"
	next.Effort = "xhigh"
	next.ServiceMode = "priority"
	request := WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous)}
	reply := host.apply(request)
	if reply.Error != "" || reply.ConfigDigest != ConfigDigest(next) || target.closed || !host.supervisor.IsActive("chat") {
		t.Fatalf("active configuration commit: %+v", reply)
	}
	record, err := readWorkerRecord(host.recordPath)
	if err != nil || record.Info.ConfigDigest != reply.ConfigDigest {
		t.Fatal("committed runtime is not discoverable")
	}
	if retry := host.apply(request); retry.Error != "" || retry.ConfigDigest != reply.ConfigDigest {
		t.Fatalf("lost-reply retry: %+v", retry)
	}
	conflict := next
	conflict.Model = "obsolete"
	if host.apply(WorkerRequest{Action: "configure", Config: &conflict, ExpectedConfig: ConfigDigest(previous)}).Error == "" {
		t.Fatal("stale settings overwrote committed inference")
	}
	target.finish("chat")
	waitSupervisorState(t, func() bool { target.mu.Lock(); defer target.mu.Unlock(); return len(target.selections["chat"]) == 2 })
	target.mu.Lock()
	got := append([]InferenceSelection(nil), target.selections["chat"]...)
	target.mu.Unlock()
	want := []InferenceSelection{{Model: "old", Effort: "medium", ServiceMode: "default"}, {Model: "new", Effort: "xhigh", ServiceMode: "priority"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider snapshots = %+v, want %+v", got, want)
	}
	target.finish("chat")
}

func TestWorkerConfigurationPublicationFailureKeepsInference(t *testing.T) {
	host, _ := newWorkerConfigurationHost(t)
	previous := host.supervisor.HarnessConfig()
	next := previous
	next.Model = "new"
	data, err := os.ReadFile(host.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(host.recordPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(host.recordPath, 0700); err != nil {
		t.Fatal(err)
	}
	request := WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous)}
	if host.apply(request).Error == "" {
		t.Fatal("failed publication reported a commit")
	}
	if host.info.ConfigDigest != ConfigDigest(previous) || !reflect.DeepEqual(host.supervisor.HarnessConfig(), previous) {
		t.Fatal("failed publication changed runtime inference")
	}
	if host.apply(WorkerRequest{Action: "configure", Config: &previous}).Error == "" {
		t.Fatal("matching retry invented a saved record")
	}
	if err := os.Remove(host.recordPath); err != nil {
		t.Fatal(err)
	}
	if err := fsx.AtomicWriteFile(host.recordPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if reply := host.apply(request); reply.Error != "" {
		t.Fatalf("restored storage did not commit: %+v", reply)
	}
}

func TestWorkerConfigurationReplacementRetainsDeliveryAndFailureState(t *testing.T) {
	host, target := newWorkerConfigurationHost(t)
	previous := host.supervisor.HarnessConfig()
	next := previous
	next.Name = "replacement"
	request := WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous)}
	host.executions[1] = &workerExecution{WorkerExecution: WorkerExecution{Sequence: 1, Done: true, LastCursor: 1}}
	if host.apply(request).Error == "" || target.closed {
		t.Fatal("replacement discarded retained terminal delivery")
	}
	delete(host.executions, 1)
	broken := previous
	broken.Name = "broken"
	if host.apply(WorkerRequest{Action: "configure", Config: &broken, ExpectedConfig: ConfigDigest(previous)}).Error == "" {
		t.Fatal("failed provider start was accepted")
	}
	if host.info.ConfigDigest != ConfigDigest(previous) || host.supervisor.HarnessConfig().Name != previous.Name {
		t.Fatal("failed replacement changed saved selection")
	}
	if available, _ := host.supervisor.Available(); available {
		t.Fatal("failed replacement claimed availability")
	}
	// An explicit retry of the saved choice must restart it, not return a no-op success.
	if reply := host.apply(WorkerRequest{Action: "configure", Config: &previous, ExpectedConfig: ConfigDigest(previous)}); reply.Error != "" {
		t.Fatalf("saved selection could not recover: %+v", reply)
	}
	if available, _ := host.supervisor.Available(); !available {
		t.Fatal("retry left provider unavailable")
	}
	if reply := host.apply(request); reply.Error != "" || reply.ConfigDigest != ConfigDigest(next) {
		t.Fatalf("idle replacement: %+v", reply)
	}
}

func TestWorkerConfigurationRejectsChangedIdentityAndInvalidInput(t *testing.T) {
	host, _ := newWorkerConfigurationHost(t)
	previous := host.supervisor.HarnessConfig()
	for _, mutate := range []func(*HarnessConfig){func(c *HarnessConfig) { c.Cwd += "/other" }, func(c *HarnessConfig) { c.Version = "other" }, func(c *HarnessConfig) { c.Effort = "bad effort" }} {
		next := previous
		mutate(&next)
		if host.apply(WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous)}).Error == "" {
			t.Fatal("invalid runtime configuration accepted")
		}
	}
	if host.apply(WorkerRequest{Action: "configure"}).Error == "" {
		t.Fatal("missing settings accepted")
	}
	if host.info.ConfigDigest != ConfigDigest(previous) {
		t.Fatal("rejected input mutated configuration")
	}
}

func TestWorkerConfigurationRestoresIdleRuntimeOnPublicationFailure(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "restored", true: "rollback-failed"}[rollbackFails], func(t *testing.T) {
			host, _ := newWorkerConfigurationHost(t)
			previous := host.supervisor.HarnessConfig()
			next := previous
			next.Name = "replacement"
			host.supervisor.registry.Register(previous.Name, func(HarnessConfig) (Harness, error) {
				if rollbackFails {
					return nil, errors.New("synthetic rollback failure")
				}
				return &supervisorHarness{name: previous.Name, active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
			})
			if err := os.Remove(host.recordPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(host.recordPath, 0700); err != nil {
				t.Fatal(err)
			}
			if host.apply(WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous)}).Error == "" {
				t.Fatal("failed publication accepted")
			}
			if host.info.ConfigDigest != ConfigDigest(previous) {
				t.Fatal("failed publication changed discoverable selection")
			}
			if rollbackFails {
				select {
				case <-host.stop:
				default:
					t.Fatal("unrestored runtime kept admitting work")
				}
			} else if host.supervisor.HarnessConfig().Name != previous.Name {
				t.Fatal("failed publication left the replacement selected")
			}
		})
	}
}

func TestWorkerConfigurationCanonicalCommitFenceAndFailure(t *testing.T) {
	for _, stage := range []string{"success", "save", "publication", "restore"} {
		t.Run(stage, func(t *testing.T) {
			host, target := newWorkerConfigurationHost(t)
			previous := host.supervisor.HarnessConfig()
			next := previous
			next.Model = "new"
			saved := "old"
			initialSelection := target.selection
			host.settings = func(_ string, current, desired HarnessConfig, _ WorkerSettings) (WorkerSettingsPlan, error) {
				return WorkerSettingsPlan{Commit: func() error {
					// CommitInference holds the dispatch snapshot fence here.
					if target.selection != initialSelection {
						t.Error("runtime changed before canonical persistence")
					}
					if stage == "save" {
						return errors.New("synthetic save failure")
					}
					saved = "new"
					if stage == "publication" || stage == "restore" {
						_ = os.Remove(host.recordPath)
					}
					return nil
				}, Restore: func() error {
					if stage == "restore" {
						return errors.New("synthetic restoration failure")
					}
					saved = "old"
					return nil
				}}, nil
			}
			reply := host.apply(WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous), Settings: &WorkerSettings{}})
			if stage == "success" {
				if reply.Error != "" || saved != "new" || host.supervisor.HarnessConfig().Model != "new" {
					t.Fatal("canonical/runtime commit did not finish", reply.Error)
				}
			} else if reply.Error == "" || host.supervisor.HarnessConfig().Model != "old" || stage != "restore" && saved != "old" {
				t.Fatal("failed canonical commit changed runtime or saved settings", reply.Error)
			}
			if stage == "restore" {
				select {
				case <-host.stop:
				default:
					t.Fatal("unrestored settings kept admitting provider work")
				}
			}
		})
	}
}

func TestWorkerConfigurationUnavailableCanonicalCommitAndFailure(t *testing.T) {
	for _, stage := range []string{"success", "save", "publication", "restore", "running", "retained", "stale"} {
		t.Run(stage, func(t *testing.T) {
			host, target := newWorkerConfigurationHost(t)
			previous := host.supervisor.HarnessConfig()
			if stage != "running" {
				if err := host.supervisor.Close(); err != nil {
					t.Fatal(err)
				}
				registry := NewRegistry()
				registry.Register(previous.Name, func(HarnessConfig) (Harness, error) { return nil, errors.New("synthetic missing command") })
				host.supervisor = NewSupervisor(registry, previous)
				if err := host.supervisor.Start(t.Context()); err == nil {
					t.Fatal("unavailable fixture started")
				}
				t.Cleanup(func() { _ = host.supervisor.Close() })
			}
			next := previous
			next.Model = "saved-intent"
			if stage == "running" {
				next.Command = "synthetic-missing-command"
			}
			saved, commits := "old", 0
			missing := errors.New("synthetic missing command")
			host.settings = func(string, HarnessConfig, HarnessConfig, WorkerSettings) (WorkerSettingsPlan, error) {
				return WorkerSettingsPlan{Unavailable: missing, Commit: func() error {
					commits++
					if stage == "save" {
						return errors.New("synthetic save failure")
					}
					saved = "new"
					if stage == "publication" || stage == "restore" {
						_ = os.Remove(host.recordPath)
					}
					return nil
				}, Restore: func() error {
					if stage == "restore" {
						return errors.New("synthetic restoration failure")
					}
					saved = "old"
					return nil
				}}, nil
			}
			request := WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous), Settings: &WorkerSettings{}}
			if stage == "retained" {
				host.executions[1] = &workerExecution{WorkerExecution: WorkerExecution{Done: true}}
			}
			if stage == "stale" {
				request.ExpectedConfig = [32]byte{}
			}
			reply := host.apply(request)
			if stage == "success" {
				if reply.Error != "" || saved != "new" || host.supervisor.HarnessConfig().Model != next.Model || host.info.ConfigDigest != ConfigDigest(next) {
					t.Fatal("unavailable intent did not commit", reply.Error)
				}
				if available, detail := host.supervisor.Available(); available || detail != missing.Error() {
					t.Fatal("saving unavailable intent invented readiness")
				}
			} else if reply.Error == "" || host.supervisor.HarnessConfig().Model != previous.Model || host.info.ConfigDigest != ConfigDigest(previous) || stage != "restore" && saved != "old" {
				t.Fatal("refused unavailable commit changed selection", reply.Error)
			}
			if stage == "running" && (commits != 0 || target.closed) {
				t.Fatal("unavailable intent replaced a running provider")
			}
			if (stage == "retained" || stage == "stale") && commits != 0 {
				t.Fatal("unavailable intent bypassed delivery or configuration fencing")
			}
			if stage == "restore" {
				select {
				case <-host.stop:
				default:
					t.Fatal("unrestored canonical intent kept the worker alive")
				}
			}
		})
	}
}

func TestWorkerUnavailableSettingsPreservesPendingProviderShutdown(t *testing.T) {
	host, _ := newWorkerConfigurationHost(t)
	previous := host.supervisor.HarnessConfig()
	failed := &uncertainCloseHarness{supervisorHarness: supervisorHarness{startErr: errors.New("synthetic startup failure"), active: map[string]bool{}, emits: map[string]core.Emit{}}}
	host.supervisor.registry.Register("unconfirmed-close", func(HarnessConfig) (Harness, error) { return failed, nil })
	broken := previous
	broken.Name = "unconfirmed-close"
	first := host.apply(WorkerRequest{Action: "configure", Config: &broken, ExpectedConfig: ConfigDigest(previous)})
	if !strings.Contains(first.Error, errProviderShutdownPending.Error()) {
		t.Fatal("fixture did not retain unconfirmed provider shutdown", first.Error)
	}
	next := previous
	next.Name = "unavailable-selection"
	committed := false
	host.settings = func(string, HarnessConfig, HarnessConfig, WorkerSettings) (WorkerSettingsPlan, error) {
		return WorkerSettingsPlan{Unavailable: errors.New("synthetic missing command"), Commit: func() error { committed = true; return nil }, Restore: func() error { committed = false; return nil }}, nil
	}
	reply := host.apply(WorkerRequest{Action: "configure", Config: &next, ExpectedConfig: ConfigDigest(previous), Settings: &WorkerSettings{}})
	if !strings.Contains(reply.Error, errProviderShutdownPending.Error()) || committed || host.supervisor.HarnessConfig().Name != previous.Name || host.info.ConfigDigest != ConfigDigest(previous) {
		t.Fatal("unavailable intent bypassed unconfirmed provider shutdown", reply.Error)
	}
}
