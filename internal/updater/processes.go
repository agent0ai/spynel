package updater

import (
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

// ProcessRegistration identifies an application, client or helper, including
// other workspaces. It contains no workspace credentials.
type ProcessRegistration struct {
	PID                  int    `json:"pid"`
	Generation           string `json:"generation"`
	Executable           string `json:"executable"`
	Installation         string `json:"installation,omitempty"`
	Version              string `json:"version"`
	Ready                bool   `json:"ready"`
	CoordinatedUpdates   bool   `json:"coordinated_updates"`
	DiagnosticReader     bool   `json:"diagnostic_reader,omitempty"`
	ProviderGuardian     bool   `json:"provider_guardian,omitempty"`
	HarnessWorker        bool   `json:"harness_worker,omitempty"`
	WorkerProtocol       int    `json:"-"`
	ApplicationClient    bool   `json:"application_client,omitempty"`
	Supervisor           bool   `json:"supervisor,omitempty"`
	SupervisorPID        int    `json:"supervisor_pid,omitempty"`
	SupervisorGeneration string `json:"supervisor_generation,omitempty"`
	ExecutorPID          int    `json:"executor_pid,omitempty"`
	ExecutorGeneration   string `json:"executor_generation,omitempty"`
	Stopping             bool   `json:"stopping,omitempty"`
	StartTime            string `json:"start_time,omitempty"`
	start                string
}

// Registry-only coordination metadata must not change the strict worker wire
// schema, which also embeds ProcessRegistration as native identity evidence.
type processRecord struct {
	ProcessRegistration
	WorkerProtocol int `json:"worker_protocol,omitempty"`
}

func (r ProcessRegistration) registryJSON() ([]byte, error) {
	return json.Marshal(processRecord{ProcessRegistration: r, WorkerProtocol: r.WorkerProtocol})
}

func processDirectory() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	directory = filepath.Join(directory, "spynel", "processes")
	if err := privateDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func (m *Manager) InstallationRoot() string {
	if m == nil {
		return ""
	}
	if m.InstallRoot != "" {
		return m.InstallRoot
	}
	return m.PackageRoot
}

// RegisterProcess is held for the complete server/election/TUI lifetime.
// A new generation is published only by the newly started executable.
func (m *Manager) RegisterProcess() (func(), error) {
	return m.registerProcess(ProcessRegistration{})
}

// RegisterDiagnosticReader keeps executable discovery honest without making a
// pipe reader an application-readiness or coordinated-restart target.
func (m *Manager) RegisterDiagnosticReader() (func(), error) {
	return m.registerProcess(ProcessRegistration{DiagnosticReader: true})
}

func (m *Manager) RegisterProviderGuardian() (func(), error) {
	return m.registerProcess(ProcessRegistration{ProviderGuardian: true})
}

func (m *Manager) RegisterHarnessWorker() (func(), error) {
	return m.registerProcess(ProcessRegistration{HarnessWorker: true, WorkerProtocol: core.WorkerProtocol})
}

// RegisterApplicationClient retains stop/uninstall visibility without making a
// finite request or event subscription an application restart target.
func (m *Manager) RegisterApplicationClient() (func(), error) {
	return m.registerProcess(ProcessRegistration{ApplicationClient: true})
}

func (m *Manager) registerProcess(role ProcessRegistration) (func(), error) {
	directory, err := processDirectory()
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	executable, err := installationProcessPath(os.Getpid())
	if err != nil {
		return nil, err
	}
	alive, start, err := inspectProcess(os.Getpid())
	if err != nil {
		return nil, err
	}
	if !alive || start == "" {
		return nil, errors.New("cannot verify own process start time")
	}
	record := role
	record.PID, record.Generation, record.Executable = os.Getpid(), hex.EncodeToString(nonce[:]), executable
	record.StartTime = start
	record.Installation, record.Version = m.InstallationRoot(), m.CurrentVersion
	record.CoordinatedUpdates = m.PackageRoot == "" || m.CoordinatedUpdates
	path := filepath.Join(directory, strconv.Itoa(record.PID)+".json")
	data, err := record.registryJSON()
	if err != nil {
		return nil, err
	}
	if err := fsx.AtomicWriteFile(path, data, 0600); err != nil {
		return nil, err
	}
	return func() {
		if current, err := readProcess(path); err == nil && current.Generation == record.Generation {
			_ = os.Remove(path)
		}
	}, nil
}

// MarkProcessReady confirms that this launch reached the application service.
func MarkProcessReady() error {
	directory, err := processDirectory()
	if err != nil {
		return err
	}
	path := filepath.Join(directory, strconv.Itoa(os.Getpid())+".json")
	record, err := readProcess(path)
	if err != nil {
		return err
	}
	record.Ready = true
	data, err := record.registryJSON()
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(path, data, 0600)
}

func readProcess(path string) (ProcessRegistration, error) {
	var record ProcessRegistration
	info, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return record, errors.New("invalid private Spynel process record")
	}
	file, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer file.Close()
	var stored processRecord
	err = json.NewDecoder(io.LimitReader(file, 4097)).Decode(&stored)
	record = stored.ProcessRegistration
	record.WorkerProtocol = stored.WorkerProtocol
	if err != nil || record.PID <= 1 || len(record.Generation) != 32 || !filepath.IsAbs(record.Executable) || record.Installation != "" && !filepath.IsAbs(record.Installation) || filepath.Base(path) != strconv.Itoa(record.PID)+".json" {
		return record, errors.New("invalid Spynel process identity")
	}
	return record, nil
}

func processMatches(record ProcessRegistration, path string) bool {
	path = strings.TrimSuffix(path, " (deleted)")
	if path == record.Executable {
		return true
	}
	// npm renames a replaced package while its executable may still be alive.
	root := record.Installation
	return root != "" && npmRootFromExecutable(record.Executable) == root &&
		filepath.Dir(npmRootFromExecutable(path)) == filepath.Dir(root) &&
		strings.HasPrefix(filepath.Base(npmRootFromExecutable(path)), "."+filepath.Base(root)+"-")
}

func liveProcesses() ([]ProcessRegistration, error) {
	ids, err := installationProcessIDs()
	if err != nil {
		return nil, err
	}
	return processRecords(ids)
}

func processRecords(ids []int) ([]ProcessRegistration, error) {
	records, err := collectProcessRecords(ids)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Generation == "" {
			// Kernel PIDs and atomic registration files can straddle startup or exit.
			time.Sleep(2 * time.Second)
			return collectProcessRecords(ids)
		}
	}
	return records, nil
}

func collectProcessRecords(ids []int) ([]ProcessRegistration, error) {
	directory, err := processDirectory()
	if err != nil {
		return nil, err
	}
	own, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, errors.New("cannot verify Spynel executable identity")
	}
	var records []ProcessRegistration
	for _, pid := range ids {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		alive, start, stateErr := inspectProcess(pid)
		if stateErr == nil && !alive || stateErr != nil && !processAlive(pid) {
			continue
		}
		path, err := installationProcessPath(pid)
		if err != nil {
			if _, recordErr := readProcess(filepath.Join(directory, strconv.Itoa(pid)+".json")); recordErr == nil && processAlive(pid) {
				return nil, fmt.Errorf("cannot verify executable of registered Spynel process %d: %w", pid, err)
			}
			continue
		}
		record, err := readProcess(filepath.Join(directory, strconv.Itoa(pid)+".json"))
		if err == nil && processMatches(record, path) {
			if stateErr != nil {
				return nil, fmt.Errorf("cannot verify start time of registered Spynel process %d: %w", pid, stateErr)
			}
			if record.StartTime != "" && record.StartTime != start {
				return nil, fmt.Errorf("registered Spynel process %d start time changed", pid)
			}
			info, err := buildinfo.ReadFile(processImagePath(pid, path))
			if err != nil {
				info, err = buildinfo.ReadFile(record.Executable)
			}
			if err == nil && info.Path == own.Path {
				record.start = start
				records = append(records, record)
				continue
			}
		}
		if filepath.Base(strings.TrimSuffix(path, " (deleted)")) != "spynel" {
			continue
		}
		info, err := buildinfo.ReadFile(processImagePath(pid, path))
		if err == nil && info.Path == own.Path && processAlive(pid) {
			executable := strings.TrimSuffix(path, " (deleted)")
			root := npmRootFromExecutable(executable)
			if strings.HasPrefix(filepath.Base(root), ".spynel-") {
				root = filepath.Join(filepath.Dir(root), "spynel")
			}
			if !validNPMRoot(root, "") {
				root = ""
			}
			if stateErr != nil {
				return nil, fmt.Errorf("cannot verify start time of Spynel process %d: %w", pid, stateErr)
			}
			records = append(records, ProcessRegistration{PID: pid, Executable: executable, Installation: root, start: start})
		}
	}
	return records, nil
}

// CheckRestartable refuses to strand an older, unregistered process silently.
func (m *Manager) CheckRestartable() error {
	return m.CheckRestartableFor(core.WorkerProtocol)
}

// CheckRestartableFor also fences publication against a target worker protocol.
// Zero means unknown; only installations without live workers may proceed.
func (m *Manager) CheckRestartableFor(workerProtocol int) error {
	root := m.InstallationRoot()
	if root == "" {
		return errors.New("updates require a managed Spynel installation")
	}
	if m.PackageRoot != "" && !m.CoordinatedUpdates {
		return errors.New("this npm launcher does not support coordinated updates; run the installed spynel killall command once, then relaunch Spynel")
	}
	records, err := liveProcesses()
	if err != nil {
		return err
	}
	for _, record := range records {
		owned := record.Installation == root || m.ownsProcessPath(root, record.Executable)
		if record.HarnessWorker && owned && (workerProtocol <= 0 || record.WorkerProtocol <= 0 || record.WorkerProtocol != workerProtocol) {
			return errors.New("update cannot preserve a live worker with incompatible or unknown compatibility; let its jobs and pending delivery finish, then explicitly stop Spynel with spynel killall, update and relaunch")
		}
		if record.DiagnosticReader || record.ProviderGuardian || record.HarnessWorker || record.ApplicationClient {
			continue
		}
		if record.SupervisorPID != 0 && (record.Installation == root || m.ownsProcessPath(root, record.Executable)) {
			parent, err := ProcessRecord(record.SupervisorPID)
			if err != nil || !parent.Supervisor || parent.Generation != record.SupervisorGeneration || !relatedProcesses(parent, record) {
				return errors.New("linked executor has no verified supervisor; run spynel killall once, then relaunch Spynel")
			}
		}
		if (record.Installation == root || m.ownsProcessPath(root, record.Executable)) && (record.Generation == "" || record.Installation != root || m.PackageRoot != "" && !record.CoordinatedUpdates) {
			return fmt.Errorf("Spynel process %d has no valid coordinated-restart registration; run spynel killall once, then launch the current version", record.PID)
		}
	}
	return nil
}

// RestartInstances waits for every other instance of this installation to
// publish a new generation using the updated version, preserving its terminal.
func (m *Manager) RestartInstances(ctx context.Context, version string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := m.CheckRestartable(); err != nil {
		return 0, err
	}
	records, err := liveProcesses()
	if err != nil {
		return 0, err
	}
	var pending []ProcessRegistration
	for _, record := range records {
		if restartTarget(record) && record.Installation == m.InstallationRoot() {
			if err := ctx.Err(); err != nil {
				return len(pending), err
			}
			if err := signalRegisteredProcess(record, RestartSignal()); err != nil {
				return len(pending), err
			}
			pending = append(pending, record)
		}
	}
	directory, err := processDirectory()
	if err != nil {
		return len(pending), err
	}
	deadline := time.NewTimer(RestartTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	count := len(pending)
	for len(pending) > 0 {
		for index := len(pending) - 1; index >= 0; index-- {
			before := pending[index]
			after, err := readProcess(filepath.Join(directory, strconv.Itoa(before.PID)+".json"))
			path, pathErr := installationProcessPath(before.PID)
			alive, start, stateErr := inspectProcess(before.PID)
			if err == nil && pathErr == nil && stateErr == nil && alive && start == before.start && (after.StartTime == "" || after.StartTime == start) && after.Ready && processMatches(after, path) && after.Installation == before.Installation && after.Generation != before.Generation && after.Version == version && (!before.Supervisor || verifiedExecutor(after, version)) {
				pending = append(pending[:index], pending[index+1:]...)
			}
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		case <-deadline.C:
			return count, fmt.Errorf("%d Spynel instance(s) did not restart into %s", len(pending), version)
		case <-ticker.C:
		}
	}
	return count, nil
}

func signalRegisteredProcess(record ProcessRegistration, signal os.Signal) error {
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return err
	}
	defer process.Release()
	if err := VerifyProcess(record); err != nil {
		return err
	}
	return process.Signal(signal)
}

// VerifyProcess checks the same native identity used before signalling without
// sending a signal. Only a confirmed dead/zombie process returns ESRCH.
func VerifyProcess(record ProcessRegistration) error {
	return verifyProcess(record, inspectProcess)
}

func verifyProcess(record ProcessRegistration, inspect func(int) (bool, string, error)) (err error) {
	if record.PID <= 1 {
		return errors.New("invalid Spynel process identity")
	}
	defer func() {
		if err != nil {
			// Graceful exit can race any of the identity/registration reads.
			if !processAlive(record.PID) {
				err = syscall.ESRCH
			} else if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				err = fmt.Errorf("%w: %v", errProcessIdentityUnavailable, err)
			}
		}
	}()
	alive, start, err := inspect(record.PID)
	if err != nil {
		return err
	}
	if !alive {
		return syscall.ESRCH
	}
	if record.start != "" && record.start != start || record.StartTime != "" && record.StartTime != start {
		return errors.New("Spynel process start time changed")
	}
	path, err := installationProcessPath(record.PID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %v", errProcessIdentityUnavailable, err)
		}
		return err
	}
	if !processMatches(record, path) {
		return errors.New("Spynel process identity changed")
	}
	if record.Generation != "" {
		current, err := ProcessRecord(record.PID)
		if errors.Is(err, os.ErrNotExist) {
			return errProcessUnregistered
		}
		if err != nil {
			return fmt.Errorf("cannot read Spynel process registration: %v", err)
		}
		if current.Generation != record.Generation {
			return errors.New("Spynel generation changed")
		}
	}
	return nil
}

var errProcessUnregistered = errors.New("Spynel process registration is missing")
var errProcessIdentityUnavailable = errors.New("Spynel process native identity is unavailable")

func unconfirmedProcessExit(err error) bool {
	return errors.Is(err, errProcessUnregistered) || errors.Is(err, errProcessIdentityUnavailable)
}

// KillAll stops verified Spynel processes, including older releases and
// different installations. Normal termination retains provider cleanup.
func KillAll(ctx context.Context, beforeStop func([]ProcessRegistration) error) (int, error) {
	records, err := liveProcesses()
	if err != nil {
		return 0, err
	}
	if beforeStop != nil {
		if err := beforeStop(records); err != nil {
			return 0, err
		}
	}
	return len(records), stopProcessRecords(ctx, records)
}

func stopProcessRecords(ctx context.Context, records []ProcessRegistration) error {
	// Supervisors acknowledge recovery suppression before executor teardown.
	for _, record := range records {
		if !record.Supervisor {
			continue
		}
		if err := signalRegisteredProcess(record, terminationSignal()); !processGone(err) && !unconfirmedProcessExit(err) {
			return fmt.Errorf("cannot stop Spynel process %d: %w", record.PID, err)
		}
		deadline := time.NewTimer(2 * time.Second)
		for {
			current, err := ProcessRecord(record.PID)
			if !processAlive(record.PID) {
				break
			}
			if err == nil && current.Generation != record.Generation {
				return errors.New("supervisor generation changed during shutdown; executors were preserved")
			}
			if err == nil && current.Stopping {
				break
			}
			select {
			case <-ctx.Done():
				deadline.Stop()
				return ctx.Err()
			case <-deadline.C:
				return errors.New("supervisor did not acknowledge shutdown; executors were preserved")
			case <-time.After(10 * time.Millisecond):
			}
		}
		deadline.Stop()
	}
	for _, record := range records {
		if record.Supervisor {
			continue
		}
		if err := signalRegisteredProcess(record, terminationSignal()); !processGone(err) && !unconfirmedProcessExit(err) {
			return fmt.Errorf("cannot stop Spynel process %d: %w", record.PID, err)
		}
	}
	killAt := time.Now().Add(10 * time.Second)
	deadline := killAt.Add(5 * time.Second)
	for _, record := range records {
		if err := waitForStoppedProcess(ctx, record, killAt, deadline, VerifyProcess); err != nil {
			return err
		}
	}
	return nil
}

func waitForStoppedProcess(ctx context.Context, record ProcessRegistration, killAt, deadline time.Time, verify func(ProcessRegistration) error) error {
	for {
		err := verify(record)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		// Missing registration/image never authorizes a signal or proves exit.
		if err != nil && !unconfirmedProcessExit(err) {
			return fmt.Errorf("cannot verify stopping Spynel process %d: %w", record.PID, err)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("cannot verify stopping Spynel process %d: %w", record.PID, err)
			}
			return fmt.Errorf("Spynel process %d did not stop", record.PID)
		}
		if err == nil && time.Now().After(killAt) && (!record.Supervisor || time.Now().After(killAt.Add(2*time.Second))) {
			if err := signalRegisteredProcess(record, os.Kill); !processGone(err) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func processGone(err error) bool {
	return err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}
