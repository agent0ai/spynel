//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/updater"
	"golang.org/x/sys/unix"
)

func supervisorExecutorFixture() error {
	_, err := enterExecutor(os.Args[1:], "synthetic-version")
	if err != nil {
		return err
	}
	root := os.Getenv("SPYNEL_TEST_SUPERVISOR_ROOT")
	file, err := os.OpenFile(filepath.Join(root, "attempts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	fmt.Fprintln(file, os.Getpid())
	file.Close()
	mode := os.Getenv("SPYNEL_TEST_SUPERVISOR_MODE")
	if mode == "startup" {
		os.Exit(19)
	}
	if err := updater.MarkProcessReady(); err != nil {
		return err
	}
	if err := admitExecutor(filepath.Join(root, "logs"), "synthetic-version", []string{"serve"}); err != nil {
		return err
	}
	if mode == "provider" {
		file, err := os.OpenFile(filepath.Join(root, "harness-owner"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return err
		}
	}
	switch mode {
	case "panic", "provider":
		go func() { panic("synthetic-supervisor-panic password=synthetic-secret") }()
		select {}
	case "kill":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	case "native":
		fmt.Fprintln(os.Stderr, "synthetic-native-stderr")
		os.Exit(17)
	case "stop":
		_ = executorIntent("stop")
		os.Exit(23)
	case "normal":
		return completeExecutorRun(nil)
	case "update":
		return completeExecutorRun(&updateRequest{args: []string{"serve"}, result: updater.Result{Current: "1.3.0", Latest: "1.3.0"}})
	case "wait":
		<-executor.ctx.Done()
		return completeExecutorRun(nil)
	}
	return fmt.Errorf("unknown synthetic mode")
}

func supervisorFixture(t *testing.T, mode string) (*exec.Cmd, string) {
	t.Helper()
	root := t.TempDir()
	command := exec.Command(os.Args[0], "__supervisor_fixture")
	command.Dir = root
	command.Env = append(os.Environ(), "SPYNEL_TEST_SUPERVISOR_MODE="+mode, "SPYNEL_TEST_SUPERVISOR_ROOT="+root, "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "HOME="+root)
	return command, root
}

func TestNativeSupervisorUpdatePublishesHandoffBeforeExit(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			command, root := supervisorFixture(t, "update")
			path := filepath.Join(root, "spynel-update-fixture")
			channel := ""
			if available {
				channel = path
			}
			command.Env = append(command.Env, "TMPDIR="+root, "SPYNEL_NPM_UPDATE_STATE="+channel)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatal("update did not return an exit status")
			}
			if !available {
				if command.ProcessState.ExitCode() != 1 || !strings.Contains(string(output), "publish npm update handoff") {
					t.Fatalf("failed channel authorized an update: %v, %s", err, output)
				}
				return
			}
			if command.ProcessState.ExitCode() != npmUpdateExitCode {
				t.Fatalf("update status: %v, %s", err, output)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte(`"version":"1.3.0"`)) || !bytes.Contains(data, []byte(`"args":["serve"]`)) {
				t.Fatalf("native update was not published: %s, %v", data, err)
			}
		})
	}
}

func TestNativeSupervisorExitPolicyAndDiagnostics(t *testing.T) {
	for _, test := range []struct {
		mode             string
		attempts, status int
	}{{"normal", 1, 0}, {"startup", 1, 19}, {"stop", 1, 23}, {"provider", 4, 2}, {"panic", 4, 2}, {"kill", 4, 137}, {"native", 4, 17}} {
		t.Run(test.mode, func(t *testing.T) {
			command, root := supervisorFixture(t, test.mode)
			output, err := command.CombinedOutput()
			status := 0
			if err != nil {
				status = command.ProcessState.ExitCode()
			}
			if status != test.status {
				t.Fatalf("status %d: %s", status, output)
			}
			data, err := os.ReadFile(filepath.Join(root, "attempts"))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(strings.Fields(string(data))); got != test.attempts {
				t.Fatalf("attempts %d want %d: %s", got, test.attempts, output)
			}
			if test.mode != "startup" {
				entries := readDiagnosticEntries(t, filepath.Join(root, "logs"))
				var found bool
				for _, entry := range entries {
					if entry.Event == "executor_exit" {
						found = true
					}
					if strings.Contains(entry.Text, "synthetic-secret") {
						t.Fatal("credential retained")
					}
				}
				if !found {
					t.Fatal("no parent-observed exit evidence")
				}
			}
		})
	}
}

func TestNativeSupervisorStopDuringBackoff(t *testing.T) {
	command, root := supervisorFixture(t, "native")
	pipe, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	output := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		var buffer [1024]byte
		for {
			n, err := pipe.Read(buffer[:])
			b.Write(buffer[:n])
			if strings.Contains(b.String(), "bounded delay") {
				output <- b.String()
				return
			}
			if err != nil {
				output <- b.String()
				return
			}
		}
	}()
	select {
	case <-output:
	case <-time.After(5 * time.Second):
		t.Fatal("no backoff")
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "attempts"))
	if len(strings.Fields(string(data))) != 1 {
		t.Fatal("restarted after stop")
	}
}

func TestNativeSupervisorPrivateModeAndSingleRunSelection(t *testing.T) {
	for _, args := range [][]string{{"docs"}, {"send"}, {"run", "--once"}, {"serve", "--automatic-startup"}, {"init", "--no-start"}, {"update"}, {"events"}, {"__provider"}} {
		if supervisorEligible(args) {
			t.Fatalf("replayed command %v", args)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "__executor", "2", "invalid", "serve")
	if err := command.Run(); err == nil {
		t.Fatal("unauthenticated executor accepted")
	}
}

func TestQueuedStopHasPriorityOverRestart(t *testing.T) {
	signals := make(chan os.Signal, 3)
	signals <- updater.RestartSignal()
	signals <- syscall.SIGTERM
	signals <- updater.RestartSignal()
	if !queuedSupervisorStop(signals) {
		t.Fatal("restart requests hid a queued stop")
	}
}
