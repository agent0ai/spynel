//go:build linux || darwin

package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWorkerUpdateLockAndPrivateMetadata(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	path, err := updateLockPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := lockUpdate(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if writer, err := lockUpdate(path, false); err == nil {
		writer()
		t.Fatal("update raced worker launch")
	}
	reader()
	writer, err := lockUpdate(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if reader, err := lockUpdate(path, true); err == nil {
		reader()
		t.Fatal("worker launch raced update")
	}
	other, err := lockUpdate(filepath.Join(filepath.Dir(path), "unrelated.lock"), false)
	if err != nil {
		t.Fatal("another installation was blocked", err)
	}
	other()
	writer()
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockUpdate(path, false); err == nil {
		unlock()
		t.Fatal("public lock file accepted")
	}
	record := ProcessRegistration{WorkerProtocol: 18, WorkerUpdateSocket: "/synthetic/control.sock", WorkerUpdateToken: "synthetic-private-token", WorkerUpdates: true}
	public, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "synthetic") || strings.Contains(string(public), "worker_protocol") || strings.Contains(string(public), "worker_updates") {
		t.Fatal("registry-only update metadata leaked onto worker identity wire")
	}
	private, err := record.registryJSON()
	if err != nil || !strings.Contains(string(private), "synthetic-private-token") {
		t.Fatal("registry writer lost update metadata")
	}
}

func TestLegacyApplicationCannotRaceWorkerReplacement(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	process := lifecycleFixture(t, root)
	record, err := ProcessRecord(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	record.WorkerUpdates = false
	data, _ := record.registryJSON()
	directory, _ := processDirectory()
	path := filepath.Join(directory, strconv.Itoa(record.PID)+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{InstallRoot: root}
	if err := manager.CheckRestartableFor(0); err == nil || !strings.Contains(err.Error(), "fence worker launch") {
		t.Fatal("legacy launch was not fenced", err)
	}
	if err := manager.CheckRestartable(); err != nil {
		t.Fatal("ordinary compatible behavior changed", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) || VerifyProcess(record) != nil {
		t.Fatal("preflight changed the legacy application")
	}
}
