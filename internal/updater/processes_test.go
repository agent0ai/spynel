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
	closeRecord, err := manager.RegisterProcess()
	if err != nil {
		t.Fatal(err)
	}
	defer closeRecord()
	if version != "2.0.0" || os.Getenv("SPYNEL_TEST_NOT_READY") != "1" {
		if err := MarkProcessReady(); err != nil {
			t.Fatal(err)
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, RestartSignal(), syscall.SIGTERM)
	fmt.Println("ready")
	if <-signals == RestartSignal() {
		closeRecord()
		_ = os.Setenv("SPYNEL_TEST_LIFECYCLE_VERSION", "2.0.0")
		executable, _ := os.Executable()
		if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
			t.Fatal(err)
		}
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
	if count, err := KillAll(ctx, nil); err != nil || count != 3 {
		t.Fatalf("kill all installations: %d %v", count, err)
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
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := (&Manager{InstallRoot: root}).RestartInstances(ctx, "2.0.0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restart claimed readiness: %v", err)
	}
	records, err := processRecords([]int{process.Process.Pid})
	if err != nil || len(records) != 1 || records[0].Version != "2.0.0" || records[0].Ready {
		t.Fatalf("expected started but unready updated process: %v %v", records, err)
	}
}
