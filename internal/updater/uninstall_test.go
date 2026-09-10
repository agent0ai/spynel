package updater

import (
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

func init() {
	if filepath.Base(os.Args[0]) != "npm" || os.Getenv("SPYNEL_TEST_NPM_ROOT") == "" {
		return
	}
	root := os.Getenv("SPYNEL_TEST_NPM_ROOT")
	want := []string{"uninstall", "--global", "--prefix", filepath.Dir(filepath.Dir(filepath.Dir(root))), "spynel"}
	if strings.Join(os.Args[1:], "\x00") != strings.Join(want, "\x00") {
		fmt.Fprintln(os.Stderr, "unexpected npm uninstall arguments")
		os.Exit(1)
	}
	if pid, _ := strconv.Atoi(os.Getenv("SPYNEL_TEST_NPM_PID")); pid > 0 {
		if path, err := installationProcessPath(pid); err == nil && path == filepath.Join(root, "npm", "vendor", "spynel") {
			os.Exit(2)
		}
	}
	if err := os.RemoveAll(root); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestUninstallProcessFixture(t *testing.T) {
	mode := os.Getenv("SPYNEL_TEST_UNINSTALL_PROCESS")
	if mode == "" {
		return
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func uninstallFixtureBinary(t *testing.T, path string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
}

func uninstallFixtureProcess(t *testing.T, path, mode string) *exec.Cmd {
	t.Helper()
	uninstallFixtureBinary(t, path)
	command := exec.Command(path, "-test.run=^TestUninstallProcessFixture$")
	command.Env = append(os.Environ(), "SPYNEL_TEST_UNINSTALL_PROCESS="+mode)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	var ready string
	if _, err := fmt.Fscanln(stdout, &ready); err != nil || ready != "ready" {
		t.Fatalf("process fixture did not start: %q %v", ready, err)
	}
	return command
}

func TestUninstallStopsOnlyOwnedProcessesAndPreservesWorkspace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(root, ".spynel-install"), []byte(ownershipMarker), 0600); err != nil {
		t.Fatal(err)
	}
	first := uninstallFixtureProcess(t, filepath.Join(root, "releases", "first", "spynel"), "term")
	second := uninstallFixtureProcess(t, filepath.Join(root, "releases", "second", "spynel"), "ignore-term")
	unrelated := uninstallFixtureProcess(t, filepath.Join(t.TempDir(), "spynel"), "term")
	workspace := filepath.Join(root, ".spynel")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "config.yaml"), []byte("keep workspace data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(bin, "spynel")
	if err := os.Symlink(filepath.Join(root, "spynel"), launcher); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{InstallRoot: root}
	removedStartup := false
	err = manager.Uninstall(t.Context(), func() error {
		removedStartup = true
		if err := first.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatal("stopped process before disabling its restart registration")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !removedStartup {
		t.Fatal("startup cleanup was skipped")
	}
	for _, command := range []*exec.Cmd{first, second} {
		if path, err := installationProcessPath(command.Process.Pid); err == nil && manager.ownsProcessPath(root, path) {
			t.Fatal("uninstalled process is still running")
		}
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("unrelated process was stopped")
	}
	if _, err := os.Lstat(launcher); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned launcher remains")
	}
	data, err := os.ReadFile(filepath.Join(workspace, "config.yaml"))
	if err != nil || string(data) != "keep workspace data\n" {
		t.Fatal("workspace data changed")
	}
}

func TestUninstallRejectsUnownedRootsAndFailedStartupCleanup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{InstallRoot: root}
	called := false
	cleanup := func() error { called = true; return errors.New("stop failed") }
	if err := manager.Uninstall(t.Context(), cleanup); err == nil || called {
		t.Fatal("unmanaged directory entered destructive cleanup")
	}
	if err := os.WriteFile(filepath.Join(root, ".spynel-install"), []byte(ownershipMarker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Uninstall(t.Context(), cleanup); err == nil || !called {
		t.Fatal("startup failure was ignored")
	}
	if !ownedRoot(root) {
		t.Fatal("failed cleanup removed installation ownership")
	}
	unlock, err := lockInstall(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	called = false
	if err := manager.Uninstall(t.Context(), cleanup); err == nil || called {
		t.Fatal("uninstall overlapped an active installer")
	}
}

func TestUninstallNPMStopsBinaryBeforePackageRemoval(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(prefix, "lib", "node_modules", "spynel")
	process := uninstallFixtureProcess(t, filepath.Join(root, "npm", "vendor", "spynel"), "term")
	for name, value := range map[string]any{
		"package.json": map[string]string{"name": "spynel", "version": "0.12.2"},
	} {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	uninstallFixtureBinary(t, filepath.Join(bin, "npm"))
	t.Setenv("PATH", bin)
	t.Setenv("SPYNEL_TEST_NPM_ROOT", root)
	t.Setenv("SPYNEL_TEST_NPM_PID", strconv.Itoa(process.Process.Pid))
	manager := &Manager{PackageRoot: root}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := manager.Uninstall(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("npm package remains")
	}
	if path, err := installationProcessPath(process.Process.Pid); err == nil && manager.ownsProcessPath(root, path) {
		t.Fatal("npm process remains: " + strconv.Itoa(process.Process.Pid))
	}
}
