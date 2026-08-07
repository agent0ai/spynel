package updater

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
)

func TestCheckFindsNewerNPMVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]string{"name": "spynel", "version": "1.3.0"})
	}))
	defer server.Close()
	manager := &Manager{
		CurrentVersion: "1.2.3", PackageRoot: t.TempDir(), LauncherManaged: true,
		RegistryURL: server.URL, CheckTimeout: time.Second,
	}
	result, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.InstalledViaNPM || !result.Available || !result.CanAutoInstall || result.Latest != "1.3.0" {
		t.Fatalf("result = %#v", result)
	}
}

func TestCheckTimesOutAndArchiveInstallSkipsRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(writer).Encode(map[string]string{"version": "1.3.0"})
	}))
	defer server.Close()
	manager := &Manager{CurrentVersion: "1.2.3", PackageRoot: t.TempDir(), RegistryURL: server.URL, CheckTimeout: 10 * time.Millisecond}
	if _, err := manager.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	result, err := (&Manager{CurrentVersion: "1.2.3", RegistryURL: server.URL}).Check(context.Background())
	if err != nil || result.InstalledViaNPM || result.Latest != "" {
		t.Fatalf("archive result = %#v, %v", result, err)
	}
}

func TestMigrateRecordsBaselineAndExposesTransition(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root
	statePath := filepath.Join(root, ".spynel", "runtime", "application-version.json")
	manager := &Manager{CurrentVersion: "1.2.3", StatePath: statePath}
	if err := manager.Migrate(context.Background(), cfg, false, extensions.Runner{}); err != nil {
		t.Fatal(err)
	}
	var got Transition
	manager.CurrentVersion = "1.4.0"
	manager.Migrations = []Migration{{Name: "tasks", Run: func(_ context.Context, transition Transition) error {
		got = transition
		return nil
	}}}
	if err := manager.Migrate(context.Background(), cfg, false, extensions.Runner{}); err != nil {
		t.Fatal(err)
	}
	if got.FromVersion != "1.2.3" || got.ToVersion != "1.4.0" || got.Config.Root != root {
		t.Fatalf("transition = %#v", got)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\n  \"version\": \"1.4.0\"\n}\n" {
		t.Fatalf("version state = %s", data)
	}
}

func TestFailedMigrationLeavesVersionForRetry(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "runtime", "application-version.json")
	manager := &Manager{CurrentVersion: "1.0.0", StatePath: statePath}
	if err := manager.Migrate(context.Background(), config.Config{Root: root}, false, extensions.Runner{}); err != nil {
		t.Fatal(err)
	}
	manager.CurrentVersion = "2.0.0"
	manager.Migrations = []Migration{{Name: "broken", Run: func(context.Context, Transition) error { return errors.New("failed") }}}
	if err := manager.Migrate(context.Background(), config.Config{Root: root}, false, extensions.Runner{}); err == nil {
		t.Fatal("failed migration unexpectedly succeeded")
	}
	data, _ := os.ReadFile(statePath)
	if string(data) != "{\n  \"version\": \"1.0.0\"\n}\n" {
		t.Fatalf("failed migration advanced state: %s", data)
	}
}

func TestConcurrentMigrationRunsTransitionOnce(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "runtime", "application-version.json")
	baseline := &Manager{CurrentVersion: "1.0.0", StatePath: statePath}
	if err := baseline.Migrate(context.Background(), config.Config{Root: root}, false, extensions.Runner{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	manager := &Manager{CurrentVersion: "2.0.0", StatePath: statePath, Migrations: []Migration{{Name: "once", Run: func(context.Context, Transition) error {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	}}}}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsSeen <- manager.Migrate(context.Background(), config.Config{Root: root}, false, extensions.Runner{})
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent migration ran %d times", calls.Load())
	}
}

func TestCompareSemanticVersions(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"1.2.0", "1.1.9", 1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"v2.0.0+build", "2.0.0", 0},
	} {
		if got := compareVersions(test.left, test.right); got != test.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
