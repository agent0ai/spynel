package updater

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestParseProcessStat(t *testing.T) {
	for _, state := range []string{"S", "R", "Z", "X"} {
		data := "123 (name ) with spaces) " + state + " 42 " + strings.Repeat("0 ", 17) + "12345 0"
		fields, err := parseProcessStat(data)
		if err != nil || fields[0] != state || fields[1] != "42" || fields[19] != "12345" {
			t.Fatalf("parse %q = %v, %v", data, fields, err)
		}
	}
	for _, data := range []string{"", "123 (bad", "123 (short) Z 1", "123 (bad start) S 1 " + strings.Repeat("0 ", 17) + "unknown"} {
		if _, err := parseProcessStat(data); err == nil {
			t.Fatalf("accepted malformed stat: %q", data)
		}
	}
}

func TestUnreadableProcessFixture(t *testing.T) {
	if os.Getenv("SPYNEL_TEST_UNREADABLE") != "1" {
		return
	}
	closeRecord, err := (&Manager{InstallRoot: os.Getenv("SPYNEL_TEST_UNREADABLE_ROOT")}).RegisterProcess()
	if err != nil {
		t.Fatal(err)
	}
	defer closeRecord()
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	fmt.Println("ready")
	<-signals
}

func TestUnreadableLiveRegistrationBlocksCoordination(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	binary := filepath.Join(root, "releases", "fixture", "spynel")
	uninstallFixtureBinary(t, binary)
	command := exec.Command(binary, "-test.run=^TestUnreadableProcessFixture$")
	command.Env = append(os.Environ(), "SPYNEL_TEST_UNREADABLE=1", "SPYNEL_TEST_UNREADABLE_ROOT="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	if ready, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("unreadable fixture startup: %q, %v", ready, err)
	}
	if _, err := installationProcessPath(command.Process.Pid); err == nil {
		t.Skip("host can inspect nondumpable children; permission denial remains covered by the platform fixture")
	}
	if !processAlive(command.Process.Pid) {
		t.Fatal("unreadable live child classified dead")
	}
	record, err := ProcessRecord(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyProcess(record); err == nil || errors.Is(err, syscall.ESRCH) {
		t.Fatal("unreadable live process was verified or classified dead")
	}
	manager := &Manager{InstallRoot: root}
	if err := manager.CheckRestartable(); err == nil || !strings.Contains(err.Error(), "cannot verify executable") {
		t.Fatalf("unreadable preflight = %v", err)
	}
	if _, err := manager.RestartInstances(t.Context(), "2.0.0"); err == nil {
		t.Fatal("unreadable live child accepted as restartable")
	}
	if err := manager.stopProcesses(t.Context(), root); err == nil || !strings.Contains(err.Error(), "cannot verify executable") {
		t.Fatalf("unreadable uninstall target accepted: %v", err)
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("unreadable live child was stopped")
	}
	t.Log("real nondumpable registered child remained live and blocked preflight/restart")
}
