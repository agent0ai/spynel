//go:build linux || darwin

package updater

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestLifecycleProcessFixture(t *testing.T) {
	root := os.Getenv("SPYNEL_TEST_LIFECYCLE_ROOT")
	if root == "" {
		return
	}
	version := os.Getenv("SPYNEL_TEST_LIFECYCLE_VERSION")
	manager := &Manager{InstallRoot: root, CurrentVersion: version}
	diagnostic := os.Getenv("SPYNEL_TEST_DIAGNOSTIC_READER") == "1"
	guardian := os.Getenv("SPYNEL_TEST_PROVIDER_GUARDIAN") == "1"
	worker := os.Getenv("SPYNEL_TEST_HARNESS_WORKER") == "1"
	client := os.Getenv("SPYNEL_TEST_APPLICATION_CLIENT") == "1"
	supervised := os.Getenv("SPYNEL_TEST_SUPERVISOR") == "1"
	var closeRecord func()
	var err error
	if diagnostic {
		closeRecord, err = manager.RegisterDiagnosticReader()
	} else if guardian {
		closeRecord, err = manager.RegisterProviderGuardian()
	} else if worker {
		closeRecord, err = manager.RegisterHarnessWorker()
	} else if client {
		closeRecord, err = manager.RegisterApplicationClient()
	} else if supervised {
		closeRecord, err = manager.RegisterSupervisor()
	} else {
		closeRecord, err = manager.RegisterProcess()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closeRecord()
	if transition := os.Getenv("SPYNEL_TEST_REGISTRATION_TRANSITION"); transition != "" {
		closeRecord()
		fmt.Println("ready")
		time.Sleep(150 * time.Millisecond)
		if transition == "exit" {
			return
		}
		closeNewRecord, registerErr := manager.RegisterProcess()
		if registerErr != nil {
			t.Fatal(registerErr)
		}
		defer closeNewRecord()
	}
	if !diagnostic && !guardian && !worker && !client && (version != "2.0.0" || os.Getenv("SPYNEL_TEST_NOT_READY") != "1") {
		if err := MarkProcessReady(); err != nil {
			t.Fatal(err)
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, RestartSignal(), syscall.SIGTERM)
	fmt.Println("ready")
	if <-signals == RestartSignal() {
		if diagnostic || guardian || worker || client {
			t.Fatal("helper received an application restart")
		}
		closeRecord()
		_ = os.Setenv("SPYNEL_TEST_LIFECYCLE_VERSION", "2.0.0")
		executable, _ := os.Executable()
		if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
			t.Fatal(err)
		}
	}
	if supervised {
		if dependent := os.Getenv("SPYNEL_TEST_STOP_DEPENDENT"); dependent != "" {
			pid, err := strconv.Atoi(dependent)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := ProcessRecord(pid); errors.Is(err, os.ErrNotExist) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("dependent did not unregister")
				}
				time.Sleep(time.Millisecond)
			}
		}
		if err := SetExecutor(0, false, true); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if os.Getenv("SPYNEL_TEST_UNREGISTER_BEFORE_EXIT") == "1" {
		closeRecord()
		time.Sleep(500 * time.Millisecond)
	}
}

func TestStopWaitsForUnregisteredDependentExit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPYNEL_TEST_UNREGISTER_BEFORE_EXIT", "1")
	dependent := lifecycleFixture(t, t.TempDir())
	t.Setenv("SPYNEL_TEST_UNREGISTER_BEFORE_EXIT", "0")
	t.Setenv("SPYNEL_TEST_SUPERVISOR", "1")
	t.Setenv("SPYNEL_TEST_STOP_DEPENDENT", strconv.Itoa(dependent.Process.Pid))
	supervisor := lifecycleFixture(t, t.TempDir())
	records, err := processRecords([]int{supervisor.Process.Pid, dependent.Process.Pid})
	if err != nil || len(records) != 2 {
		t.Fatalf("stop discovery = %v, %v", records, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := stopProcessRecords(ctx, records); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if !errors.Is(VerifyProcess(record), syscall.ESRCH) {
			t.Fatal("shutdown succeeded before confirmed native exit")
		}
	}
}

func TestRestartPreflightPreservesIncompatibleWorker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPYNEL_TEST_HARNESS_WORKER", "1")
	root := t.TempDir()
	worker := lifecycleFixture(t, root)
	record, err := ProcessRecord(worker.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := processDirectory()
	path := filepath.Join(directory, strconv.Itoa(record.PID)+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(before, &data); err != nil {
		t.Fatal(err)
	}
	data["worker_protocol"] = core.WorkerProtocol - 1
	incompatible, _ := json.Marshal(data)
	if err := os.WriteFile(path, incompatible, 0600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{InstallRoot: root, CurrentVersion: "1.0.0"}
	if err := manager.CheckRestartable(); err == nil {
		t.Fatal("preflight allowed a controller to strand its incompatible live worker")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(incompatible) || VerifyProcess(record) != nil {
		t.Fatalf("refusal changed the live worker: %v", err)
	}
}

func TestUpdateWorkerCompatibilityBeforePublication(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPYNEL_TEST_HARNESS_WORKER", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := lifecycleFixture(t, root)
	record, err := ProcessRecord(worker.Process.Pid)
	if err != nil || record.WorkerProtocol != core.WorkerProtocol {
		t.Fatalf("worker compatibility registration = %#v, %v", record, err)
	}
	identity, err := json.Marshal(record)
	if err != nil || strings.Contains(string(identity), "worker_protocol") {
		t.Fatalf("registry metadata changed the strict native-identity wire schema: %s, %v", identity, err)
	}
	directory, _ := processDirectory()
	path := filepath.Join(directory, strconv.Itoa(record.PID)+".json")
	manager := &Manager{InstallRoot: root, CurrentVersion: "1.0.0"}
	for _, test := range []struct {
		name                                  string
		worker, target                        int
		otherInstallation, available, allowed bool
	}{
		{"compatible-release", core.WorkerProtocol, core.WorkerProtocol, false, true, true},
		{"new-incompatible-target", core.WorkerProtocol, core.WorkerProtocol + 1, false, true, false},
		{"old-incompatible-worker", core.WorkerProtocol - 1, core.WorkerProtocol, false, true, false},
		{"unknown-worker", 0, core.WorkerProtocol, false, true, false},
		{"unknown-target", core.WorkerProtocol, 0, false, true, false},
		{"current-restart", core.WorkerProtocol, 0, false, false, true},
		{"other-installation", 0, 0, true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := record
			current.WorkerProtocol = test.worker
			selected := *manager
			if test.otherInstallation {
				selected.InstallRoot = t.TempDir()
			}
			before, _ := current.registryJSON()
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			result := Result{CanAutoInstall: true, Source: "npm", Current: "1.0.0", Latest: "2.0.0", Available: test.available, WorkerProtocol: test.target}
			err := selected.PrepareUpdate(t.Context(), result)
			if (err == nil) != test.allowed {
				t.Fatalf("publication preflight = %v, allowed %t", err, test.allowed)
			}
			if !test.allowed && !strings.Contains(err.Error(), "spynel killall") {
				t.Fatalf("missing explicit drain/stop guidance: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) || VerifyProcess(current) != nil {
				t.Fatalf("preflight changed or stopped worker: %v", err)
			}
		})
	}
}

func TestUnregisteredLiveProcessIsNotDeadOrSignalled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	fixture := lifecycleFixture(t, t.TempDir())
	record, err := ProcessRecord(fixture.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := processDirectory()
	if err := os.Remove(filepath.Join(directory, strconv.Itoa(record.PID)+".json")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyProcess(record); err == nil || processGone(err) {
		t.Fatalf("missing live registration treated as verified/dead: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := stopProcessRecords(ctx, []ProcessRegistration{record}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unregistered live stop = %v", err)
	}
	if !processAlive(record.PID) {
		t.Fatal("unregistered live process was signalled")
	}
}

func TestStopWaitsForUnconfirmedImageExitWithoutSignalAuthority(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	fixture := lifecycleFixture(t, t.TempDir())
	record, err := ProcessRecord(fixture.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if processGone(errProcessIdentityUnavailable) {
		t.Fatal("missing live image became death evidence")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	past := time.Now().Add(-time.Second)
	err = waitForStoppedProcess(ctx, record, past, time.Now().Add(time.Second), func(ProcessRegistration) error {
		return errProcessIdentityUnavailable
	})
	if !errors.Is(err, context.DeadlineExceeded) || VerifyProcess(record) != nil {
		t.Fatalf("unconfirmed image wait = %v; fixture must remain live until timeout", err)
	}
	if err := signalRegisteredProcess(record, terminationSignal()); err != nil {
		t.Fatal(err)
	}
	probes := 0
	err = waitForStoppedProcess(t.Context(), record, time.Now().Add(time.Second), time.Now().Add(5*time.Second), func(r ProcessRegistration) error {
		probes++
		if probes <= 3 {
			return errProcessIdentityUnavailable
		}
		return VerifyProcess(r)
	})
	if err != nil || probes < 4 || !errors.Is(VerifyProcess(record), syscall.ESRCH) {
		t.Fatalf("unconfirmed exit was not joined: probes %d, %v", probes, err)
	}
}

func TestVerifyProcessNativeAbsenceNeedsConfirmedExit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	fixture := lifecycleFixture(t, t.TempDir())
	record, err := ProcessRecord(fixture.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Darwin may lose native identity before signal-zero confirms exit.
	for _, missing := range []error{syscall.ESRCH, os.ErrNotExist} {
		probe := func(r ProcessRegistration) error {
			return verifyProcess(r, func(int) (bool, string, error) { return false, "", missing })
		}
		if err := probe(record); !unconfirmedProcessExit(err) || processGone(err) {
			t.Fatalf("unconfirmed native absence must wait without signal authority: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err := waitForStoppedProcess(ctx, record, time.Now().Add(-time.Second), time.Now().Add(time.Second), probe)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || VerifyProcess(record) != nil {
			t.Fatalf("native absence must not signal a live fixture: %v", err)
		}
	}
	if err := fixture.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Wait()
	if err := verifyProcess(record, func(int) (bool, string, error) { return false, "", syscall.ESRCH }); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("confirmed native exit must remain death evidence: %v", err)
	}
}

func TestUnregisteredSupervisorCannotAuthorizeDependentStop(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dependent := lifecycleFixture(t, t.TempDir())
	t.Setenv("SPYNEL_TEST_SUPERVISOR", "1")
	supervisor := lifecycleFixture(t, t.TempDir())
	records, err := processRecords([]int{supervisor.Process.Pid, dependent.Process.Pid})
	if err != nil || len(records) != 2 {
		t.Fatalf("stop discovery = %v, %v", records, err)
	}
	directory, _ := processDirectory()
	if err := os.Remove(filepath.Join(directory, strconv.Itoa(supervisor.Process.Pid)+".json")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := stopProcessRecords(ctx, records); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unacknowledged supervisor stop = %v", err)
	}
	for _, record := range records {
		if !processAlive(record.PID) {
			t.Fatal("unacknowledged supervisor authorized a signal")
		}
	}
}

func TestProcessDiscoveryReconcilesRegistrationTransitions(t *testing.T) {
	for _, transition := range []string{"start", "exit"} {
		t.Run(transition, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SPYNEL_TEST_REGISTRATION_TRANSITION", transition)
			fixture := lifecycleFixture(t, t.TempDir())
			if transition == "exit" {
				done := make(chan struct{})
				go func() { _ = fixture.Wait(); close(done) }()
				t.Cleanup(func() { <-done })
			}
			records, err := processRecords([]int{fixture.Process.Pid})
			if err != nil {
				t.Fatal(err)
			}
			if transition == "exit" {
				if len(records) != 0 {
					t.Fatal("exited process became an unregistered legacy instance")
				}
			} else if len(records) != 1 || records[0].Generation == "" {
				t.Fatal("starting process became an unregistered legacy instance")
			}
		})
	}
}

func TestDiagnosticReaderIsDiscoverableWithoutBecomingARestartTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	server := lifecycleFixture(t, root)
	t.Setenv("SPYNEL_TEST_DIAGNOSTIC_READER", "1")
	reader := lifecycleFixture(t, root)
	t.Setenv("SPYNEL_TEST_DIAGNOSTIC_READER", "0")
	t.Setenv("SPYNEL_TEST_PROVIDER_GUARDIAN", "1")
	guardian := lifecycleFixture(t, root)
	t.Setenv("SPYNEL_TEST_PROVIDER_GUARDIAN", "0")
	t.Setenv("SPYNEL_TEST_HARNESS_WORKER", "1")
	worker := lifecycleFixture(t, root)
	t.Setenv("SPYNEL_TEST_HARNESS_WORKER", "0")
	t.Setenv("SPYNEL_TEST_APPLICATION_CLIENT", "1")
	client := lifecycleFixture(t, root)
	directory, _ := processDirectory()
	path := filepath.Join(directory, strconv.Itoa(reader.Process.Pid)+".json")
	before, err := readProcess(path)
	if err != nil || !before.DiagnosticReader || before.Ready {
		t.Fatalf("diagnostic registration lost its role: %#v %v", before, err)
	}
	guardianPath := filepath.Join(directory, strconv.Itoa(guardian.Process.Pid)+".json")
	guardianBefore, err := readProcess(guardianPath)
	if err != nil || !guardianBefore.ProviderGuardian || guardianBefore.Ready || restartTarget(guardianBefore) {
		t.Fatalf("guardian registration lost its role: %#v, %v", guardianBefore, err)
	}
	workerPath := filepath.Join(directory, strconv.Itoa(worker.Process.Pid)+".json")
	workerBefore, err := readProcess(workerPath)
	if err != nil || !workerBefore.HarnessWorker || workerBefore.Ready || restartTarget(workerBefore) {
		t.Fatalf("worker registration lost its role: %#v, %v", workerBefore, err)
	}
	clientPath := filepath.Join(directory, strconv.Itoa(client.Process.Pid)+".json")
	clientBefore, err := readProcess(clientPath)
	if err != nil || !clientBefore.ApplicationClient || clientBefore.Ready || restartTarget(clientBefore) {
		t.Fatalf("client registration lost its role: %#v, %v", clientBefore, err)
	}
	manager := &Manager{InstallRoot: root, CurrentVersion: "1.0.0"}
	if err := manager.CheckRestartable(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if count, err := manager.RestartInstances(ctx, "2.0.0"); err != nil || count != 1 {
		t.Fatalf("diagnostic reader changed restart targets: %d %v", count, err)
	}
	after, err := readProcess(path)
	if err != nil || after.Generation != before.Generation || after.Ready {
		t.Fatalf("diagnostic reader was restarted: %#v %v", after, err)
	}
	guardianAfter, err := readProcess(guardianPath)
	if err != nil || guardianAfter.Generation != guardianBefore.Generation {
		t.Fatal("guardian was restarted as an application")
	}
	workerAfter, err := readProcess(workerPath)
	if err != nil || workerAfter.Generation != workerBefore.Generation {
		t.Fatal("worker was restarted as an application")
	}
	clientAfter, err := readProcess(clientPath)
	if err != nil || clientAfter != clientBefore {
		t.Fatal("client was restarted as an application")
	}
	records, err := processRecords([]int{server.Process.Pid, reader.Process.Pid, guardian.Process.Pid, worker.Process.Pid, client.Process.Pid})
	if err != nil || len(records) != 5 {
		t.Fatalf("shutdown discovery lost the reader: %d %v", len(records), err)
	}
	if err := stopProcessRecords(ctx, records); err != nil {
		t.Fatal(err)
	}
}

func lifecycleFixture(t *testing.T, installation string) *exec.Cmd {
	t.Helper()
	binary := filepath.Join(installation, "releases", filepath.Base(t.TempDir()), "spynel")
	uninstallFixtureBinary(t, binary)
	command := exec.Command(binary, "-test.run=^TestLifecycleProcessFixture$")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "SPYNEL_TEST_LIFECYCLE_ROOT="+installation, "SPYNEL_TEST_LIFECYCLE_VERSION=1.0.0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	reader := bufio.NewReader(stdout)
	ready, err := reader.ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("fixture startup: %q %v", ready, err)
	}
	return command
}

func TestRestartAllWorkspacesAndStopAllInstallations(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// macOS uses HOME for UserConfigDir.
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	first, second := lifecycleFixture(t, root), lifecycleFixture(t, root)
	other := lifecycleFixture(t, t.TempDir())
	ids := []int{first.Process.Pid, second.Process.Pid, other.Process.Pid}
	before, err := processRecords(ids)
	if err != nil || len(before) != 3 {
		t.Fatalf("live records: %d %v", len(before), err)
	}
	manager := &Manager{InstallRoot: root, CurrentVersion: "1.0.0"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	count, err := manager.RestartInstances(ctx, "2.0.0")
	if err != nil || count != 2 {
		t.Fatalf("coordinated restart: %d %v", count, err)
	}
	after, err := processRecords(ids)
	if err != nil || len(after) != 3 {
		t.Fatalf("restarted records: %d %v", len(after), err)
	}
	for _, record := range after {
		want := "2.0.0"
		if record.PID == other.Process.Pid {
			want = "1.0.0"
		}
		if record.Version != want {
			t.Fatalf("process %d version: %s", record.PID, record.Version)
		}
	}
	preflightOnly := errors.New("verified discovery without stopping unrelated test runs")
	if _, err := KillAll(ctx, func(records []ProcessRegistration) error {
		found := make(map[int]bool)
		for _, record := range records {
			found[record.PID] = true
		}
		for _, pid := range ids {
			if !found[pid] {
				t.Fatalf("killall omitted fixture %d", pid)
			}
		}
		return preflightOnly
	}); err != preflightOnly {
		t.Fatalf("killall preflight: %v", err)
	}
	// Other updater test invocations share the same executable identity.
	// Exercise termination only against this test's fixture processes.
	if err := stopProcessRecords(ctx, after); err != nil {
		t.Fatalf("stop all fixture installations: %v", err)
	}
	if remaining, err := processRecords(ids); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining processes: %d %v", len(remaining), err)
	}
}

func TestProcessRecordCannotTargetAnUnrelatedExecutable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	directory, err := processDirectory()
	if err != nil {
		t.Fatal(err)
	}
	path, err := installationProcessPath(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	record := ProcessRegistration{PID: command.Process.Pid, Generation: "0123456789abcdef0123456789abcdef", Executable: path, Version: "1.0.0"}
	data, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(directory, strconv.Itoa(record.PID)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	records, err := processRecords([]int{record.PID})
	if err != nil || len(records) != 0 {
		t.Fatalf("unrelated executable accepted: %v %v", records, err)
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("unrelated process was stopped")
	}
}

func TestRestartRejectsLegacyProcessBeforeSignaling(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	process := lifecycleFixture(t, root)
	directory, err := processDirectory()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, strconv.Itoa(process.Process.Pid)+".json")
	record, err := readProcess(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{InstallRoot: root}
	record.Installation = t.TempDir()
	data, _ := json.Marshal(record)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckRestartable(); err == nil {
		t.Fatal("incorrect installation record hid a running instance")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckRestartable(); err == nil || !strings.Contains(err.Error(), "spynel killall") {
		t.Fatalf("legacy preflight: %v", err)
	}
	if err := process.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("preflight stopped the legacy process")
	}
}

func TestRestartRequiresApplicationReadiness(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPYNEL_TEST_NOT_READY", "1")
	root := t.TempDir()
	process := lifecycleFixture(t, root)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	observed := make(chan struct{})
	defer func() { cancel(); <-observed }()
	go func() {
		defer close(observed)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if record, err := ProcessRecord(process.Process.Pid); err == nil && record.Version == "2.0.0" {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	if _, err := (&Manager{InstallRoot: root}).RestartInstances(ctx, "2.0.0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("restart claimed readiness: %v", err)
	}
	records, err := processRecords([]int{process.Process.Pid})
	if err != nil || len(records) != 1 || records[0].Version != "2.0.0" || records[0].Ready {
		t.Fatalf("expected started but unready updated process: %v %v", records, err)
	}
}

func TestRestartAfterRunningExecutableIsUnlinked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	process := lifecycleFixture(t, root)
	path, err := installationProcessPath(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	uninstallFixtureBinary(t, path)
	if records, err := processRecords([]int{process.Process.Pid}); err != nil || len(records) != 1 {
		t.Fatalf("lost running instance after executable replacement: %v %v", records, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if count, err := (&Manager{InstallRoot: root}).RestartInstances(ctx, "2.0.0"); err != nil || count != 1 {
		t.Fatalf("restart after executable replacement: %d %v", count, err)
	}
}

func TestSupervisorWithoutExecutorRemainsDiscoverableAndStops(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPYNEL_TEST_SUPERVISOR", "1")
	fixture := lifecycleFixture(t, t.TempDir())
	records, err := processRecords([]int{fixture.Process.Pid})
	if err != nil || len(records) != 1 || !records[0].Supervisor || !restartTarget(records[0]) {
		t.Fatalf("lost supervisor in backoff: %v %v", records, err)
	}
	if verifiedExecutor(records[0], records[0].Version) {
		t.Fatal("missing executor reported ready")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := stopProcessRecords(ctx, records); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorGenerationChangeRejectsSignal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPYNEL_TEST_SUPERVISOR", "1")
	fixture := lifecycleFixture(t, t.TempDir())
	records, err := processRecords([]int{fixture.Process.Pid})
	if err != nil || len(records) != 1 {
		t.Fatal(err)
	}
	record := records[0]
	current := record
	current.Generation = "11111111111111111111111111111111"
	data, _ := json.Marshal(current)
	directory, _ := processDirectory()
	if err := os.WriteFile(filepath.Join(directory, strconv.Itoa(record.PID)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := signalRegisteredProcess(record, terminationSignal()); err == nil {
		t.Fatal("changed generation was signalled")
	}
	if err := fixture.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("new generation was stopped")
	}
}

func TestCancelledCoordinatorPreservesPeerGeneration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	peer := lifecycleFixture(t, root)
	before, err := ProcessRecord(peer.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	count, err := (&Manager{InstallRoot: root}).RestartInstances(ctx, "2.0.0")
	if !errors.Is(err, context.Canceled) || count != 0 {
		t.Fatalf("cancelled coordination = %d %v", count, err)
	}
	after, err := ProcessRecord(peer.Process.Pid)
	if err != nil || after.Generation != before.Generation {
		t.Fatal("cancelled coordinator signalled the peer")
	}
}

func TestZombieRegistrationDoesNotBlockUpdates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	peer := lifecycleFixture(t, root)
	zombie := lifecycleFixture(t, root)
	if err := zombie.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(zombie.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := zombie.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child was reaped before assertion: %v", err)
	}
	if processAlive(zombie.Process.Pid) {
		t.Fatal("unreaped zombie classified live")
	}
	if _, err := ProcessRecord(zombie.Process.Pid); err != nil {
		t.Fatalf("lost retained registration: %v", err)
	}
	manager := &Manager{InstallRoot: root, CurrentVersion: "1.0.0"}
	if err := manager.CheckRestartable(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if count, err := manager.RestartInstances(ctx, "2.0.0"); err != nil || count != 1 {
		t.Fatalf("restart with zombie = %d, %v", count, err)
	}
	if records, err := processRecords([]int{peer.Process.Pid, zombie.Process.Pid}); err != nil || len(records) != 1 {
		t.Fatalf("discovery = %v, %v", records, err)
	}
	if err := zombie.Wait(); err == nil {
		t.Fatal("killed child unexpectedly succeeded")
	}
	if processAlive(zombie.Process.Pid) {
		t.Fatal("reaped process classified live")
	}
	t.Log("real unreaped zombie and exited child excluded; live peer restarted with readiness and new generation")
}

func TestProcessInspectionFailuresAreConservative(t *testing.T) {
	for _, test := range []struct {
		name              string
		alive             bool
		inspection, probe error
		want              bool
	}{
		{"live", true, nil, nil, true},
		{"zombie", false, nil, nil, false},
		{"exited", false, os.ErrNotExist, syscall.ESRCH, false},
		{"permission", false, os.ErrPermission, syscall.EPERM, true},
		{"unreadable", false, errors.New("unreadable state"), nil, true},
		{"unknown", false, errors.New("unknown state"), errors.New("unknown probe"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := processMayBeAlive(test.alive, test.inspection, test.probe); got != test.want {
				t.Fatalf("may be alive = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReusedPIDStartTimeRejectsSignal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	fixture := lifecycleFixture(t, t.TempDir())
	records, err := processRecords([]int{fixture.Process.Pid})
	if err != nil || len(records) != 1 || records[0].start == "" {
		t.Fatalf("missing kernel identity: %v, %v", records, err)
	}
	record := records[0]
	record.start += "-previous-process"
	if err := signalRegisteredProcess(record, terminationSignal()); err == nil || !strings.Contains(err.Error(), "start time changed") {
		t.Fatalf("reused PID accepted: %v", err)
	}
	if err := fixture.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("replacement process stopped")
	}
}

func TestVerifyProcessIsReadOnlyAndFenced(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	fixture := lifecycleFixture(t, t.TempDir())
	record, err := ProcessRecord(fixture.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyProcess(record); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ProcessRegistration){
		func(r *ProcessRegistration) { r.StartTime += "changed" },
		func(r *ProcessRegistration) { r.Executable += ".wrong" },
		func(r *ProcessRegistration) { r.Generation = "00000000000000000000000000000000" },
	} {
		changed := record
		mutate(&changed)
		if err := VerifyProcess(changed); err == nil || errors.Is(err, syscall.ESRCH) {
			t.Fatal("changed live identity accepted or classified dead")
		}
	}
	if current, err := ProcessRecord(record.PID); err != nil || current.Generation != record.Generation || VerifyProcess(record) != nil {
		t.Fatal("read-only verification disturbed the live process")
	}
	if err := fixture.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(VerifyProcess(record), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatal("confirmed exit was not classified dead")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = fixture.Wait()
	if err := VerifyProcess(record); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("reaped process was not classified dead: %v", err)
	}
}

func TestStaleRegistrationStartTimeIsNotAdopted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	fixture := lifecycleFixture(t, root)
	record, err := ProcessRecord(fixture.Process.Pid)
	if err != nil || record.StartTime == "" {
		t.Fatalf("missing registered start identity: %v, %v", record, err)
	}
	record.StartTime += "-previous-process"
	data, _ := json.Marshal(record)
	directory, _ := processDirectory()
	if err := os.WriteFile(filepath.Join(directory, strconv.Itoa(record.PID)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := processRecords([]int{fixture.Process.Pid}); err == nil || !strings.Contains(err.Error(), "start time changed") {
		t.Fatalf("stale registration discovery = %v", err)
	}
	manager := &Manager{InstallRoot: root}
	for name, check := range map[string]func() error{
		"preflight": manager.CheckRestartable,
		"restart": func() error {
			_, err := manager.RestartInstances(t.Context(), "2.0.0")
			return err
		},
		"uninstall": func() error { return manager.stopProcesses(t.Context(), root) },
	} {
		if err := check(); err == nil || !strings.Contains(err.Error(), "start time changed") {
			t.Fatalf("stale registration %s = %v", name, err)
		}
	}
	if err := signalRegisteredProcess(record, terminationSignal()); err == nil || !strings.Contains(err.Error(), "start time changed") {
		t.Fatalf("stale registration signalled: %v", err)
	}
	if err := fixture.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("replacement process stopped")
	}
}
