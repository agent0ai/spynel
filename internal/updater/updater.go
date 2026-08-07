package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/extensions"
	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	DefaultCheckTimeout = 10 * time.Second
	defaultRegistryURL  = "https://registry.npmjs.org/spynel/latest"
	maxRegistryResponse = 64 * 1024
)

// Result describes the installed and published npm versions. InstalledViaNPM
// is false for release archives and development builds, where Spynel must not
// suggest an npm-owned update operation.
type Result struct {
	InstalledViaNPM bool
	Current         string
	Latest          string
	Available       bool
	CanAutoInstall  bool
	Command         string
}

// Transition is passed to every compiled-in migration. Future migrations can
// inspect and update durable workspace state while retaining the exact
// application versions on both sides of the transition.
type Transition struct {
	FromVersion string
	ToVersion   string
	Config      config.Config
}

// Migration is a named, retry-safe application update hook. A failed hook
// leaves the recorded version unchanged, so all hooks for the transition may
// run again on the next start.
type Migration struct {
	Name string
	Run  func(context.Context, Transition) error
}

// Manager owns npm release discovery and per-workspace application migrations.
// Network checks and migrations are independent: archive installations still
// receive migrations even though they cannot update through npm.
type Manager struct {
	CurrentVersion  string
	StatePath       string
	PackageRoot     string
	LauncherManaged bool
	RegistryURL     string
	CheckTimeout    time.Duration
	Client          *http.Client
	Migrations      []Migration
}

type packageMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type installedMarker struct {
	Version string `json:"version"`
}

type versionState struct {
	Version string `json:"version"`
}

// Detect constructs an update manager and recognizes the npm wrapper either
// from its explicit launcher environment or from the executable's vendor path.
func Detect(currentVersion, statePath string) *Manager {
	manager := &Manager{
		CurrentVersion: currentVersion,
		StatePath:      statePath,
		RegistryURL:    strings.TrimSpace(os.Getenv("SPYNEL_NPM_REGISTRY_URL")),
		CheckTimeout:   DefaultCheckTimeout,
	}
	if manager.RegistryURL == "" {
		manager.RegistryURL = defaultRegistryURL
	}
	root := strings.TrimSpace(os.Getenv("SPYNEL_NPM_PACKAGE_ROOT"))
	if root == "" {
		root = npmRootFromExecutable()
	}
	if validNPMRoot(root, currentVersion) {
		manager.PackageRoot = root
		manager.LauncherManaged = os.Getenv("SPYNEL_NPM_LAUNCHER_MANAGED") == "1"
	}
	return manager
}

func npmRootFromExecutable() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	vendor := filepath.Dir(executable)
	if filepath.Base(vendor) != "vendor" || filepath.Base(filepath.Dir(vendor)) != "npm" {
		return ""
	}
	return filepath.Dir(filepath.Dir(vendor))
}

func validNPMRoot(root, currentVersion string) bool {
	if root == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	var metadata packageMetadata
	if json.Unmarshal(data, &metadata) != nil || metadata.Name != "spynel" {
		return false
	}
	markerData, err := os.ReadFile(filepath.Join(root, "npm", "vendor", ".installed.json"))
	if err != nil {
		return false
	}
	var marker installedMarker
	if json.Unmarshal(markerData, &marker) != nil || marker.Version == "" || marker.Version != metadata.Version {
		return false
	}
	return currentVersion == "" || currentVersion == "dev" || currentVersion == metadata.Version
}

// Check queries npm's small latest-version endpoint with a hard deadline.
func (m *Manager) Check(ctx context.Context) (Result, error) {
	result := Result{
		InstalledViaNPM: m != nil && m.PackageRoot != "",
		Current:         "",
		CanAutoInstall:  m != nil && m.PackageRoot != "" && m.LauncherManaged,
		Command:         "npm update --global spynel",
	}
	if m == nil {
		return result, nil
	}
	result.Current = m.CurrentVersion
	if !result.InstalledViaNPM {
		return result, nil
	}
	if _, ok := parseVersion(m.CurrentVersion); !ok {
		return result, fmt.Errorf("installed Spynel version %q is not semantic", m.CurrentVersion)
	}
	timeout := m.CheckTimeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	checkContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(checkContext, http.MethodGet, m.RegistryURL, nil)
	if err != nil {
		return result, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "spynel/"+m.CurrentVersion)
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(checkContext.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("npm update check timed out after %s", timeout)
		}
		return result, fmt.Errorf("query npm registry: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return result, fmt.Errorf("npm registry returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRegistryResponse+1))
	if err != nil {
		return result, err
	}
	if len(data) > maxRegistryResponse {
		return result, errors.New("npm registry response is too large")
	}
	var metadata packageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return result, fmt.Errorf("decode npm registry response: %w", err)
	}
	if _, ok := parseVersion(metadata.Version); !ok {
		return result, fmt.Errorf("npm returned invalid version %q", metadata.Version)
	}
	result.Latest = metadata.Version
	result.Available = compareVersions(metadata.Version, m.CurrentVersion) > 0
	return result, nil
}

// Migrate runs version-transition hooks once per workspace. Extension hooks
// receive from_version and to_version around the compiled-in migration list.
func (m *Manager) Migrate(ctx context.Context, cfg config.Config, extensionsEnabled bool, hooks extensions.Runner) error {
	if m == nil || m.StatePath == "" {
		return nil
	}
	current, ok := parseVersion(m.CurrentVersion)
	if !ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.StatePath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(m.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open application migration lock: %w", err)
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("lock application migration state: %w", err)
	}
	defer unlockFile(lock)
	data, err := os.ReadFile(m.StatePath)
	if os.IsNotExist(err) {
		return m.writeVersion(current.raw)
	}
	if err != nil {
		return fmt.Errorf("read application version state: %w", err)
	}
	var state versionState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode application version state: %w", err)
	}
	previous, ok := parseVersion(state.Version)
	if !ok {
		return fmt.Errorf("recorded application version %q is not semantic", state.Version)
	}
	if compareParsed(previous, current) == 0 {
		return nil
	}
	transition := Transition{FromVersion: previous.raw, ToVersion: current.raw, Config: cfg}
	payload := map[string]any{
		"from_version": transition.FromVersion,
		"to_version":   transition.ToVersion,
		"workspace":    cfg.Root,
	}
	if extensionsEnabled {
		output, hookErr := hooks.Run(ctx, "update.before", payload)
		if hookErr != nil {
			return fmt.Errorf("update.before: %w", hookErr)
		}
		if output.Cancel {
			return errors.New(emptyAs(output.Message, "update.before extension cancelled the migration"))
		}
	}
	for _, migration := range m.Migrations {
		if migration.Run == nil {
			continue
		}
		if err := migration.Run(ctx, transition); err != nil {
			return fmt.Errorf("migration %s (%s to %s): %w", emptyAs(migration.Name, "unnamed"), transition.FromVersion, transition.ToVersion, err)
		}
	}
	if extensionsEnabled {
		output, hookErr := hooks.Run(ctx, "update.after", payload)
		if hookErr != nil {
			return fmt.Errorf("update.after: %w", hookErr)
		}
		if output.Cancel {
			return errors.New(emptyAs(output.Message, "update.after extension cancelled the migration"))
		}
	}
	return m.writeVersion(current.raw)
}

func (m *Manager) writeVersion(version string) error {
	data, err := json.MarshalIndent(versionState{Version: version}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(m.StatePath), 0o700); err != nil {
		return err
	}
	if err := fsx.AtomicWriteFile(m.StatePath, data, 0o600); err != nil {
		return fmt.Errorf("record application version: %w", err)
	}
	return nil
}

type parsedVersion struct {
	raw        string
	numbers    [3]int
	prerelease []string
}

func parseVersion(value string) (parsedVersion, bool) {
	raw := strings.TrimPrefix(strings.TrimSpace(value), "v")
	withoutBuild := strings.SplitN(raw, "+", 2)[0]
	parts := strings.SplitN(withoutBuild, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return parsedVersion{}, false
	}
	parsed := parsedVersion{raw: raw}
	for index, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return parsedVersion{}, false
		}
		value, err := strconv.Atoi(number)
		if err != nil || value < 0 {
			return parsedVersion{}, false
		}
		parsed.numbers[index] = value
	}
	if len(parts) == 2 {
		if parts[1] == "" {
			return parsedVersion{}, false
		}
		parsed.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range parsed.prerelease {
			if identifier == "" {
				return parsedVersion{}, false
			}
			for _, character := range identifier {
				if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
					return parsedVersion{}, false
				}
			}
		}
	}
	return parsed, true
}

func compareVersions(left, right string) int {
	leftVersion, leftOK := parseVersion(left)
	rightVersion, rightOK := parseVersion(right)
	if !leftOK || !rightOK {
		return 0
	}
	return compareParsed(leftVersion, rightVersion)
}

func compareParsed(left, right parsedVersion) int {
	for index := range left.numbers {
		if left.numbers[index] < right.numbers[index] {
			return -1
		}
		if left.numbers[index] > right.numbers[index] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	limit := len(left.prerelease)
	if len(right.prerelease) < limit {
		limit = len(right.prerelease)
	}
	for index := 0; index < limit; index++ {
		leftIdentifier, rightIdentifier := left.prerelease[index], right.prerelease[index]
		leftNumber, leftNumeric := numericIdentifier(leftIdentifier)
		rightNumber, rightNumeric := numericIdentifier(rightIdentifier)
		switch {
		case leftNumeric && rightNumeric && leftNumber < rightNumber:
			return -1
		case leftNumeric && rightNumeric && leftNumber > rightNumber:
			return 1
		case leftNumeric && !rightNumeric:
			return -1
		case !leftNumeric && rightNumeric:
			return 1
		case leftIdentifier < rightIdentifier:
			return -1
		case leftIdentifier > rightIdentifier:
			return 1
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

func numericIdentifier(value string) (int, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
