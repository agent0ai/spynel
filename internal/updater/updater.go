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
)

const (
	DefaultCheckTimeout = 10 * time.Second
	defaultRegistryURL  = "https://registry.npmjs.org/spynel/latest"
	maxRegistryResponse = 64 * 1024
	periodicChecksEnv   = "SPYNEL_NPM_PERIODIC_UPDATE_CHECKS"
	checkedAtEnv        = "SPYNEL_NPM_UPDATE_CHECKED_AT"
	latestVersionEnv    = "SPYNEL_NPM_UPDATE_LATEST"
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

// PeriodicChecksEnabled reports whether the validated npm package is running
// under an interactive launch that authorized proactive checks. Archive,
// directly invoked, automatic-startup, and noninteractive processes must not
// claim npm-owned periodic update state.
func (m *Manager) PeriodicChecksEnabled() bool {
	return m != nil && m.PackageRoot != "" && m.LauncherManaged && m.PeriodicChecks
}

// InitialAvailability reads the launcher's bounded startup-check snapshot.
// A valid timestamp records an attempted check even when the registry failed;
// in that case availability remains false until the next hourly refresh.
func (m *Manager) InitialAvailability() (available bool, checkedAt time.Time, ok bool) {
	if !m.PeriodicChecksEnabled() {
		return false, time.Time{}, false
	}
	checkedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(os.Getenv(checkedAtEnv)))
	if err != nil {
		return false, time.Time{}, false
	}
	latest := strings.TrimSpace(os.Getenv(latestVersionEnv))
	if _, valid := parseVersion(latest); valid {
		available = compareVersions(latest, m.CurrentVersion) > 0
	}
	return available, checkedAt, true
}

// Manager owns npm release discovery and launcher-managed updates.
type Manager struct {
	CurrentVersion  string
	PackageRoot     string
	LauncherManaged bool
	PeriodicChecks  bool
	RegistryURL     string
	CheckTimeout    time.Duration
	Client          *http.Client
}

type packageMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type installedMarker struct {
	Version string `json:"version"`
}

// Detect constructs an update manager and recognizes the npm wrapper either
// from its explicit launcher environment or from the executable's vendor path.
func Detect(currentVersion string) *Manager {
	manager := &Manager{
		CurrentVersion: currentVersion,
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
		manager.PeriodicChecks = os.Getenv(periodicChecksEnv) == "1"
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

type parsedVersion struct {
	raw        string
	numbers    [3]int
	prerelease []string
}

func parseVersion(value string) (parsedVersion, bool) {
	raw := strings.TrimPrefix(strings.TrimSpace(value), "v")
	buildParts := strings.Split(raw, "+")
	if len(buildParts) > 2 || (len(buildParts) == 2 && !validIdentifiers(buildParts[1], false)) {
		return parsedVersion{}, false
	}
	withoutBuild := buildParts[0]
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
		if !validIdentifiers(parts[1], true) {
			return parsedVersion{}, false
		}
		parsed.prerelease = strings.Split(parts[1], ".")
	}
	return parsed, true
}

func validIdentifiers(value string, rejectNumericLeadingZeros bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, character := range identifier {
			if character < '0' || character > '9' {
				numeric = false
			}
			if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
				return false
			}
		}
		if rejectNumericLeadingZeros && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
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
