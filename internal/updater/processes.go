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

	"github.com/agent0ai/spynel/internal/fsx"
)

// ProcessRegistration identifies a live server/TUI, including secondary TUIs
// and servers in other workspaces. It contains no workspace credentials.
type ProcessRegistration struct {
	PID                int    `json:"pid"`
	Generation         string `json:"generation"`
	Executable         string `json:"executable"`
	Installation       string `json:"installation,omitempty"`
	Version            string `json:"version"`
	Ready              bool   `json:"ready"`
	CoordinatedUpdates bool   `json:"coordinated_updates"`
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
	record := ProcessRegistration{PID: os.Getpid(), Generation: hex.EncodeToString(nonce[:]), Executable: executable, Installation: m.InstallationRoot(), Version: m.CurrentVersion}
	record.CoordinatedUpdates = m.PackageRoot == "" || m.CoordinatedUpdates
	path := filepath.Join(directory, strconv.Itoa(record.PID)+".json")
	data, err := json.Marshal(record)
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
	data, err := json.Marshal(record)
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
	err = json.NewDecoder(io.LimitReader(file, 4097)).Decode(&record)
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
		path, err := installationProcessPath(pid)
		if err != nil {
			if _, recordErr := readProcess(filepath.Join(directory, strconv.Itoa(pid)+".json")); recordErr == nil && processAlive(pid) {
				return nil, fmt.Errorf("cannot verify executable of registered Spynel process %d: %w", pid, err)
			}
			continue
		}
		record, err := readProcess(filepath.Join(directory, strconv.Itoa(pid)+".json"))
		if err == nil && processMatches(record, path) {
			info, err := buildinfo.ReadFile(processImagePath(pid, path))
			if err != nil {
				info, err = buildinfo.ReadFile(record.Executable)
			}
			if err == nil && info.Path == own.Path {
				records = append(records, record)
				continue
			}
		}
		if filepath.Base(strings.TrimSuffix(path, " (deleted)")) != "spynel" {
			continue
		}
		info, err := buildinfo.ReadFile(processImagePath(pid, path))
		if err == nil && info.Path == own.Path {
			executable := strings.TrimSuffix(path, " (deleted)")
			root := npmRootFromExecutable(executable)
			if strings.HasPrefix(filepath.Base(root), ".spynel-") {
				root = filepath.Join(filepath.Dir(root), "spynel")
			}
			if !validNPMRoot(root, "") {
				root = ""
			}
			records = append(records, ProcessRegistration{PID: pid, Executable: executable, Installation: root})
		}
	}
	return records, nil
}

// CheckRestartable refuses to strand an older, unregistered process silently.
func (m *Manager) CheckRestartable() error {
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
		if (record.Installation == root || m.ownsProcessPath(root, record.Executable)) && (record.Generation == "" || record.Installation != root || m.PackageRoot != "" && !record.CoordinatedUpdates) {
			return fmt.Errorf("Spynel process %d has no valid coordinated-restart registration; run spynel killall once, then launch the current version", record.PID)
		}
	}
	return nil
}

// RestartInstances waits for every other instance of this installation to
// publish a new generation using the updated version, preserving its terminal.
func (m *Manager) RestartInstances(ctx context.Context, version string) (int, error) {
	if err := m.CheckRestartable(); err != nil {
		return 0, err
	}
	records, err := liveProcesses()
	if err != nil {
		return 0, err
	}
	var pending []ProcessRegistration
	for _, record := range records {
		if record.Installation == m.InstallationRoot() {
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
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	count := len(pending)
	for len(pending) > 0 {
		for index := len(pending) - 1; index >= 0; index-- {
			before := pending[index]
			after, err := readProcess(filepath.Join(directory, strconv.Itoa(before.PID)+".json"))
			path, pathErr := installationProcessPath(before.PID)
			if err == nil && pathErr == nil && after.Ready && processMatches(after, path) && after.Installation == before.Installation && after.Generation != before.Generation && after.Version == version {
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
	path, err := installationProcessPath(record.PID)
	if err != nil {
		return err
	}
	if !processMatches(record, path) {
		return errors.New("Spynel process identity changed before signal delivery")
	}
	return process.Signal(signal)
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
	for _, record := range records {
		if err := signalRegisteredProcess(record, terminationSignal()); !processGone(err) {
			return err
		}
	}
	killAt := time.Now().Add(10 * time.Second)
	deadline := killAt.Add(5 * time.Second)
	for _, record := range records {
		for {
			path, err := installationProcessPath(record.PID)
			if err != nil || !processMatches(record, path) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("Spynel process %d did not stop", record.PID)
			}
			if time.Now().After(killAt) {
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
	return nil
}

func processGone(err error) bool {
	return err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}
