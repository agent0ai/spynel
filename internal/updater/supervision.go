package updater

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

// Includes lawful primary expiry, startup and graceful shutdown.
const RestartTimeout = 60 * time.Second

func (m *Manager) RegisterSupervisor() (func(), error) {
	return m.registerProcess(ProcessRegistration{Supervisor: true})
}

func (m *Manager) RegisterExecutor(parent int, generation string) (func(), error) {
	if err := ValidateExecutorParent(parent, generation); err != nil {
		return nil, err
	}
	if parent != os.Getppid() {
		return nil, errors.New("executor parent changed")
	}
	record, err := ProcessRecord(parent)
	if err != nil || !record.Supervisor || record.Generation != generation || !sameProcessImage(parent, os.Getpid()) {
		return nil, errors.New("invalid supervisor identity")
	}
	return m.registerProcess(ProcessRegistration{SupervisorPID: parent, SupervisorGeneration: generation})
}

func ProcessRecord(pid int) (ProcessRegistration, error) {
	directory, err := processDirectory()
	if err != nil {
		return ProcessRegistration{}, err
	}
	return readProcess(filepath.Join(directory, strconv.Itoa(pid)+".json"))
}

// Only the supervisor writes its logical instance record.
func SetExecutor(pid int, ready, stopping bool) error {
	record, err := ProcessRecord(os.Getpid())
	if err != nil || !record.Supervisor {
		return errors.New("supervisor registration unavailable")
	}
	record.Ready, record.Stopping = ready, stopping
	record.ExecutorPID, record.ExecutorGeneration = 0, ""
	if pid != 0 {
		child, err := ProcessRecord(pid)
		if err != nil || child.SupervisorPID != record.PID || child.SupervisorGeneration != record.Generation || !relatedProcesses(record, child) {
			return errors.New("executor ownership unavailable")
		}
		record.ExecutorPID, record.ExecutorGeneration = pid, child.Generation
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	directory, err := processDirectory()
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(filepath.Join(directory, strconv.Itoa(record.PID)+".json"), data, 0600)
}

func restartTarget(record ProcessRegistration) bool {
	return !record.DiagnosticReader && !record.ProviderGuardian && !record.HarnessWorker && !record.ApplicationClient && record.SupervisorPID == 0
}

// RetireExecutor is called by its supervisor only after direct-child Wait.
func RetireExecutor(pid int) error {
	child, err := ProcessRecord(pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	parent, err := ProcessRecord(os.Getpid())
	if err != nil {
		return err
	}
	if !parent.Supervisor || child.SupervisorPID != parent.PID || child.SupervisorGeneration != parent.Generation || processAlive(pid) {
		return errors.New("cannot retire a live or unrelated executor")
	}
	directory, err := processDirectory()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(directory, strconv.Itoa(pid)+".json"))
}

func relatedProcesses(parent, child ProcessRegistration) bool {
	pid, err := processParentID(child.PID)
	path, pathErr := installationProcessPath(child.PID)
	alive, start, stateErr := inspectProcess(child.PID)
	return err == nil && pid == parent.PID && pathErr == nil && stateErr == nil && alive && (child.StartTime == "" || child.StartTime == start) && processMatches(child, path) && child.Installation == parent.Installation
}

func sameProcessImage(first, second int) bool {
	a, err := installationProcessPath(first)
	b, otherErr := installationProcessPath(second)
	return err == nil && otherErr == nil && a == b
}

func verifiedExecutor(parent ProcessRegistration, version string) bool {
	child, err := ProcessRecord(parent.ExecutorPID)
	return err == nil && child.Ready && child.Version == version && child.Generation == parent.ExecutorGeneration &&
		child.SupervisorPID == parent.PID && child.SupervisorGeneration == parent.Generation && relatedProcesses(parent, child)
}

func ValidateExecutorParent(parent int, generation string) error {
	record, err := ProcessRecord(parent)
	if err != nil || parent != os.Getppid() || !record.Supervisor || record.Generation != generation || !sameProcessImage(parent, os.Getpid()) {
		return errors.New("invalid inherited supervisor identity")
	}
	return nil
}
