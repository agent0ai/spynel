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
	supervised := os.Getenv("SPYNEL_TEST_SUPERVISOR") == "1"
	var closeRecord func()
	var err error
	if diagnostic {
		closeRecord, err = manager.RegisterDiagnosticReader()
	} else if guardian {
		closeRecord, err = manager.RegisterProviderGuardian()
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
	if !diagnostic && !guardian && (version != "2.0.0" || os.Getenv("SPYNEL_TEST_NOT_READY") != "1") {
		if err := MarkProcessReady(); err != nil {
			t.Fatal(err)
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, RestartSignal(), syscall.SIGTERM)
	fmt.Println("ready")
	if <-signals == RestartSignal() {
		if diagnostic || guardian {
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
		if err := SetExecutor(0, false, true); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
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
	records, err := processRecords([]int{server.Process.Pid, reader.Process.Pid, guardian.Process.Pid})
	if err != nil || len(records) != 3 {
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
	if err := (&Manager{InstallRoot: root}).CheckRestartable(); err == nil {
		t.Fatal("reused PID adopted a stale registration")
	}
	if err := signalRegisteredProcess(record, terminationSignal()); err == nil || !strings.Contains(err.Error(), "start time changed") {
		t.Fatalf("stale registration signalled: %v", err)
	}
	if err := fixture.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("replacement process stopped")
	}
}
