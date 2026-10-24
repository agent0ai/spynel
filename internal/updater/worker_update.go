package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

// WorkerUpdateRequest is an installation-control protocol, independent of the
// harness protocol. It never reads or translates another version's journals.
type WorkerUpdateRequest struct {
	Generation string
	Action     string
	Protocol   int
	Owner      ProcessRegistration
}

func RegisterWorkerUpdate(socket, token string) error {
	directory, err := processDirectory()
	if err != nil {
		return err
	}
	path := filepath.Join(directory, strconv.Itoa(os.Getpid())+".json")
	record, err := readProcess(path)
	if err != nil {
		return err
	}
	if !record.HarnessWorker {
		return errors.New("update endpoint requires a worker registration")
	}
	record.WorkerUpdateSocket, record.WorkerUpdateToken = socket, token
	data, err := record.registryJSON()
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(path, data, 0600)
}

func updateLockPath(root string) (string, error) {
	directory, err := processDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, fmt.Sprintf("update-%x.lock", sha256.Sum256([]byte(root)))), nil
}

// LockWorkerLaunch prevents a predecessor from launching a worker while its
// installation is being replaced, or after a different version was published.
func LockWorkerLaunch(ctx context.Context) (func(), error) {
	record, err := ProcessRecord(os.Getpid())
	if errors.Is(err, os.ErrNotExist) && Detect("").InstallationRoot() == "" {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if record.Installation == "" {
		return func() {}, nil
	}
	path, err := updateLockPath(record.Installation)
	if err != nil {
		return nil, err
	}
	// Retiring a worker can restart its application before Finish releases the
	// installation lock. Let that successor wait instead of failing startup.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var unlock func()
	for {
		unlock, err = lockUpdate(path, true)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for installation update: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !WorkerInstallationCurrent(record) {
		unlock()
		return nil, errors.New("installation changed; waiting for its updated application to restart")
	}
	return unlock, nil
}

func WorkerInstallationCurrent(record ProcessRegistration) bool {
	version := ""
	if npmRootFromExecutable(record.Executable) == record.Installation {
		data, readErr := os.ReadFile(filepath.Join(record.Installation, "package.json"))
		var metadata packageMetadata
		if readErr == nil && json.Unmarshal(data, &metadata) == nil && validNPMRoot(record.Installation, metadata.Version) {
			version = metadata.Version
		}
	} else if marker, readErr := readBundle(filepath.Join(record.Installation, "current")); readErr == nil {
		version = marker.Version
	}
	return version != "" && version == record.Version
}

// A released OS lock also releases a pause whose reply was lost. File presence
// and timestamps are never used as ownership evidence.
func WorkerUpdateActive(root string) bool {
	path, err := updateLockPath(root)
	if err != nil {
		return true
	}
	unlock, err := lockUpdate(path, true)
	if err != nil {
		return true
	}
	unlock()
	return false
}

func (m *Manager) workerUpdate(ctx context.Context, worker ProcessRegistration, action string, owner ProcessRegistration) error {
	if worker.Installation != m.InstallationRoot() || !m.ownsProcessPath(m.InstallationRoot(), worker.Executable) {
		return errors.New("worker installation identity is inconsistent")
	}
	if err := VerifyProcess(worker); err != nil {
		return err
	}
	if worker.WorkerUpdateSocket == "" || worker.WorkerUpdateToken == "" {
		return errors.New("this older worker cannot pause safely for an update; let jobs and pending delivery finish, then run spynel killall, update and relaunch once")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		info, err := os.Lstat(worker.WorkerUpdateSocket)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("worker update endpoint unavailable")
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", worker.WorkerUpdateSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	data, err := json.Marshal(WorkerUpdateRequest{Generation: worker.Generation, Action: action, Owner: owner, Protocol: core.WorkerProtocol})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/update", bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+worker.WorkerUpdateToken)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("worker update request was not confirmed; retry after checking installation state")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// Only fixed protocol errors cross this boundary; never echo endpoint data.
		if response.StatusCode == http.StatusConflict {
			return errors.New("update deferred: a worker has active work, pending delivery, or another update; retry when it clears")
		}
		return errors.New("worker update ownership or readiness could not be verified")
	}
	return nil
}

// WorkerUpdate holds installation launch admission while idle workers are
// paused. Close resumes them on failure; Finish retires them after publication.
type WorkerUpdate struct {
	manager *Manager
	owner   ProcessRegistration
	workers []ProcessRegistration
	unlock  func()
}

func (m *Manager) BeginWorkerUpdate(ctx context.Context, protocol int) (*WorkerUpdate, error) {
	path, err := updateLockPath(m.InstallationRoot())
	if err != nil {
		return nil, err
	}
	unlock, err := lockUpdate(path, false)
	if err != nil {
		return nil, err
	}
	guard := &WorkerUpdate{manager: m, unlock: unlock}
	if err := m.CheckRestartableFor(protocol); err != nil {
		guard.Close()
		return nil, err
	}
	records, err := liveProcesses()
	if err != nil {
		guard.Close()
		return nil, err
	}
	for _, record := range records {
		if !record.HarnessWorker || record.Installation != m.InstallationRoot() {
			continue
		}
		compatible := protocol > 0 && record.WorkerProtocol == protocol
		if compatible && (protocol != core.WorkerProtocol || record.WorkerUpdateSocket == "") {
			continue
		}
		if guard.owner.PID == 0 {
			guard.owner, err = ProcessRecord(os.Getpid())
			if err != nil {
				guard.Close()
				return nil, err
			}
		}
		if compatible {
			if err := m.workerUpdate(ctx, record, "recover", guard.owner); err != nil {
				guard.Close()
				return nil, err
			}
			continue
		}
		// Include an uncertain pause so Close attempts to release it too.
		guard.workers = append(guard.workers, record)
		if err := m.workerUpdate(ctx, record, "pause", guard.owner); err != nil {
			guard.Close()
			return nil, err
		}
	}
	return guard, nil
}

func (g *WorkerUpdate) Close() {
	if g == nil || g.unlock == nil {
		return
	}
	for _, worker := range g.workers {
		_ = g.manager.workerUpdate(context.Background(), worker, "resume", g.owner)
	}
	g.unlock()
	g.unlock = nil
}

func (g *WorkerUpdate) Finish(ctx context.Context) error {
	for _, worker := range g.workers {
		if err := g.manager.workerUpdate(ctx, worker, "stop", g.owner); err != nil {
			return err
		}
		deadline := time.Now().Add(15 * time.Second)
		// Reuse confirmed-exit handling without authorizing signal escalation.
		if err := waitForStoppedProcess(ctx, worker, deadline, deadline, VerifyProcess); err != nil {
			return err
		}
	}
	g.workers = nil
	return nil
}

// HoldWorkerUpdate serves the npm parent's lifetime pipe. EOF aborts; a single
// commit line follows successful package replacement. No shell or retry loop.
func (m *Manager) HoldWorkerUpdate(ctx context.Context, protocol int, input io.Reader, output io.Writer) error {
	guard, err := m.BeginWorkerUpdate(ctx, protocol)
	if err != nil {
		return err
	}
	defer guard.Close()
	if _, err := io.WriteString(output, "ready\n"); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(input, 8))
	if err != nil {
		return err
	}
	if string(data) == "commit\n" {
		return guard.Finish(ctx)
	}
	if len(data) == 0 {
		return nil
	}
	return errors.New("invalid update completion")
}
