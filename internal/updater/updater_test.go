package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	for _, invalid := range []string{"1.2.3-alpha..1", "1.2.3-01", "1.2.3+", "1.2.3+bad?", "1.2.3+one+two"} {
		if _, ok := parseVersion(invalid); ok {
			t.Fatalf("parseVersion(%q) accepted an invalid semantic version", invalid)
		}
	}
}

func TestPeriodicAvailabilityRequiresValidatedLauncherAndSemanticSnapshot(t *testing.T) {
	manager := &Manager{CurrentVersion: "1.2.3", PackageRoot: t.TempDir(), LauncherManaged: true, PeriodicChecks: true}
	checkedAt := time.Date(2026, 8, 16, 7, 0, 0, 123, time.UTC)
	t.Setenv(checkedAtEnv, checkedAt.Format(time.RFC3339Nano))
	t.Setenv(latestVersionEnv, "1.3.0")
	available, gotCheckedAt, ok := manager.InitialAvailability()
	if !manager.PeriodicChecksEnabled() || !ok || !available || !gotCheckedAt.Equal(checkedAt) {
		t.Fatalf("initial availability = enabled %t, available %t, checked %v, ok %t", manager.PeriodicChecksEnabled(), available, gotCheckedAt, ok)
	}

	t.Setenv(latestVersionEnv, "invalid")
	available, _, ok = manager.InitialAvailability()
	if !ok || available {
		t.Fatalf("invalid latest snapshot = available %t, ok %t", available, ok)
	}

	manager.PeriodicChecks = false
	if manager.PeriodicChecksEnabled() {
		t.Fatal("launcher-managed skipped startup enabled periodic checks")
	}
	if _, _, ok := manager.InitialAvailability(); ok {
		t.Fatal("launcher-managed skipped startup accepted periodic update state")
	}

	manager.PeriodicChecks = true
	manager.LauncherManaged = false
	if manager.PeriodicChecksEnabled() {
		t.Fatal("validated npm tree without its launcher enabled periodic checks")
	}
}
