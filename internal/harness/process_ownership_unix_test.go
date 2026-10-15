//go:build linux || darwin

package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

func TestProcessOwnershipHelper(t *testing.T) {
	mode := os.Getenv("SPYNEL_TEST_OWNERSHIP_MODE")
	if mode == "" {
		return
	}
	if mode == "group-parent" {
		command := exec.Command(os.Args[0], "-test.run=^TestProcessOwnershipHelper$")
		command.Env = append(os.Environ(), "SPYNEL_TEST_OWNERSHIP_MODE=group-child")
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if mode == "group-child" {
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, os.Getpid())
		time.Sleep(time.Hour)
		return
	}
	if mode == "tool" {
		signal.Ignore(syscall.SIGTERM)
		_ = os.NewFile(3, "closed-by-provider").Close()
		_ = os.NewFile(4, "closed-by-provider").Close()
		fmt.Fprintln(os.Stdout, os.Getpid())
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if mode == "owner" {
		owner := processOwnership{path: os.Getenv("SPYNEL_TEST_OWNERSHIP_PATH")}
		if err := owner.acquire(); err != nil {
			t.Fatal(err)
		}
		defer owner.release()
	} else {
		file := os.NewFile(3, "inherited-ownership")
		if _, err := file.Stat(); err != nil {
			t.Fatal(err)
		}
		defer file.Close()
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestProviderExitCleansUpRemainingProcessGroup(t *testing.T) {
	owner := processOwnership{path: filepath.Join(t.TempDir(), "harness-owner")}
	if err := owner.acquire(); err != nil {
		t.Fatal(err)
	}
	defer owner.release()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessOwnershipHelper$")
	command.Env = append(os.Environ(), "SPYNEL_TEST_OWNERSHIP_MODE=group-parent")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	command.Stdout, command.Stderr = writer, os.Stderr
	if err := startProvider(command, &owner); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	_ = writer.Close()
	_ = reader.SetReadDeadline(time.Now().Add(5 * time.Second))
	output := bufio.NewReader(reader)
	line, err := output.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	_ = input.Close()
	if _, err := io.ReadAll(output); err != nil {
		t.Fatalf("provider exit left its descendant holding stdout: %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderGuardianRetainsLockAndCleansUpAfterOwnerLoss(t *testing.T) {
	owner := processOwnership{path: filepath.Join(t.TempDir(), "harness-owner")}
	if err := owner.acquire(); err != nil {
		t.Fatal(err)
	}
	defer owner.release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessOwnershipHelper$")
	command.Env = append(os.Environ(), "SPYNEL_TEST_OWNERSHIP_MODE=tool")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := startProvider(command, &owner); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("provider did not start: %q, %v", line, err)
	}
	contender := processOwnership{path: owner.path}
	// Close the owner's lock descriptor, as SIGKILL would, while the provider
	// has already closed inherited descriptors and still has live stdin.
	if err := owner.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := contender.acquire(); !errors.Is(err, errProcessOwnershipBusy) {
		t.Fatalf("guardian failed to preserve ownership: %v", err)
	}
	if err := owner.lifeWrite.Close(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if ctx.Err() != nil {
		t.Fatal("provider guardian failed to stop after owner loss")
	}
	provider, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("guardian left a live provider behind")
	}
	if err := contender.acquire(); err != nil {
		t.Fatalf("confirmed cleanup did not release ownership: %v", err)
	}
	defer contender.release()
}

func ownershipHelper(t *testing.T, mode, path string, owner *processOwnership) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessOwnershipHelper$")
	command.Env = append(os.Environ(), "SPYNEL_TEST_OWNERSHIP_MODE="+mode, "SPYNEL_TEST_OWNERSHIP_PATH="+path)
	if owner != nil {
		command.ExtraFiles = append(command.ExtraFiles, owner.file)
	}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper readiness: %q, %v", line, err)
	}
	return command
}

func TestProcessOwnershipReleasedAfterOwnerSIGKILL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	command := ownershipHelper(t, "owner", path, nil)
	contender := processOwnership{path: path}
	if err := contender.acquire(); !errors.Is(err, errProcessOwnershipBusy) {
		t.Fatalf("live owner bypassed: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := contender.acquire(); err != nil {
		t.Fatalf("dead owner left permanent fence: %v", err)
	}
	defer contender.release()
}

func TestSurvivingProviderRetainsOwnershipUntilExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	owner := processOwnership{path: path}
	if err := owner.acquire(); err != nil {
		t.Fatal(err)
	}
	command := ownershipHelper(t, "provider", path, &owner)
	if err := owner.release(); err != nil {
		t.Fatal(err)
	}
	contender := processOwnership{path: path}
	if err := contender.acquire(); !errors.Is(err, errProcessOwnershipBusy) {
		t.Fatalf("surviving provider lost its lock: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := contender.acquire(); err != nil {
		t.Fatalf("provider exit did not release ownership: %v", err)
	}
	defer contender.release()
}

func TestSupervisorRecoversOwnershipWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-owner")
	owner := processOwnership{path: path}
	if err := owner.acquire(); err != nil {
		t.Fatal(err)
	}
	defer owner.release()
	registry := NewRegistry()
	registry.Register("fixture", func(HarnessConfig) (Harness, error) { return &supervisorHarness{}, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "fixture"})
	supervisor.SetProcessStartGuard(path, nil)
	defer supervisor.Close()
	if err := supervisor.Start(t.Context()); !errors.Is(err, errProcessOwnershipBusy) {
		t.Fatalf("expected busy owner: %v", err)
	}
	if available, _ := supervisor.Available(); available {
		t.Fatal("busy owner reported available")
	}
	if err := owner.release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-supervisor.ReadyEvents():
	case <-time.After(3 * time.Second):
		t.Fatal("ownership did not recover automatically")
	}
	if available, detail := supervisor.Available(); !available {
		t.Fatalf("ownership stayed unavailable: %s", detail)
	}
}

type recoverableShutdownHarness struct {
	supervisorHarness
	finished <-chan struct{}
}

func (h *recoverableShutdownHarness) Close() error {
	select {
	case <-h.finished:
		return h.supervisorHarness.Close()
	default:
		return errors.New("provider still stopping")
	}
}

func TestSupervisorRecoversWhenPendingShutdownCompletes(t *testing.T) {
	finished := make(chan struct{})
	previous := &recoverableShutdownHarness{finished: finished}
	registry := NewRegistry()
	registry.Register("previous", func(HarnessConfig) (Harness, error) { return previous, nil })
	registry.Register("next", func(HarnessConfig) (Harness, error) { return &supervisorHarness{}, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "previous"})
	supervisor.SetProcessStartGuard(filepath.Join(t.TempDir(), "harness-owner"), nil)
	defer supervisor.Close()
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-supervisor.ReadyEvents()
	if err := supervisor.Reconfigure(HarnessConfig{Name: "next"}); !errors.Is(err, errProviderShutdownPending) {
		t.Fatalf("pending close admitted successor: %v", err)
	}
	close(finished)
	select {
	case <-supervisor.ReadyEvents():
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed shutdown did not heal the supervisor")
	}
	if available, detail := supervisor.Available(); !available {
		t.Fatalf("supervisor remains blocked: %s", detail)
	}
	if supervisor.HarnessConfig().Name != "previous" {
		t.Fatal("failed reconfiguration changed the saved selection")
	}
}

func TestProcessOwnershipRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "harness-owner")
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(t.TempDir(), "other"), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			owner := processOwnership{path: path}
			if err := owner.acquire(); err == nil {
				_ = owner.release()
				t.Fatal("unsafe lock file accepted")
			}
		})
	}
}

func TestProcessOwnershipPathUsesCanonicalWorkspaceAndUserConfig(t *testing.T) {
	configuration := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configuration)
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	first, err := ProcessOwnershipPath(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProcessOwnershipPath(alias)
	if err != nil || first != second {
		t.Fatalf("workspace alias changed ownership: %q, %q, %v", first, second, err)
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(first) != filepath.Join(directory, "spynel", "harness") {
		t.Fatal("ownership lock remained on the shared workspace filesystem")
	}
	third, err := ProcessOwnershipPath(t.TempDir())
	if err != nil || third == first {
		t.Fatal("distinct workspaces share an ownership lock")
	}
}
