//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/updater"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "__provider" || os.Args[1] == "__worker") {
		if err := Run(os.Args[1:], "synthetic-version"); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("SPYNEL_TEST_WORKER_PROVIDER") == "1" {
		os.Exit(runWorkerAdmissionProvider())
	}
	if len(os.Args) > 1 && os.Args[1] == "__executor" {
		var err error
		if os.Getenv("SPYNEL_TEST_SUPERVISOR_MODE") != "" {
			err = supervisorExecutorFixture()
		} else {
			err = Run(os.Args[1:], "synthetic-version")
		}
		if exit, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(exit.ExitCode())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "__supervisor_fixture" {
		err := Run([]string{"serve"}, "synthetic-version")
		if exit, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(exit.ExitCode())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "__diagnostics" {
		if Run(os.Args[1:], "synthetic-version") != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "__server_fixture" {
		if Run([]string{"serve", "--config", os.Args[2]}, "synthetic-version") != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "__diagnostic_fixture" {
		d := newProcessDiagnostic(os.Args[2], "synthetic-version")
		_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(filepath.Dir(d.Directory), "user-config"))
		_ = os.Setenv("HOME", filepath.Join(filepath.Dir(d.Directory), "user-home"))
		command := exec.Command(os.Args[0], "__diagnostics", "crash")
		stop, err := startCrashReader(d, command)
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stdout, "terminal-stdout-preserved")
		switch os.Args[3] {
		case "goroutine":
			go func() { panic("synthetic-goroutine-cause password=synthetic-secret") }()
			select {}
		case "fatal":
			debug.SetMaxStack(1024)
			crashFixtureRecurse(100000)
		case "normal":
			stop()
		case "native":
			fmt.Fprintln(os.Stderr, "native-fatal-cause password=synthetic-secret")
			os.Exit(17)
		case "capture-failure":
			_ = command.Process.Kill()
			// Confirm reader loss before writing; SIGKILL can interrupt an in-flight tee.
			stop()
			fmt.Fprintln(os.Stderr, "original-failure-after-reader-loss\n"+strings.Repeat("terminal data\n", 10000))
			os.Exit(17)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExitRecorderRegistersBeforeReadingItsInput(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	command := exec.Command(os.Args[0], "__diagnostics", "exit")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		record, err := updater.ProcessRecord(command.Process.Pid)
		if err == nil && record.DiagnosticReader && !record.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exit recorder remained an unregistered process")
		}
		time.Sleep(10 * time.Millisecond)
	}
	diagnostic := newProcessDiagnostic(filepath.Join(t.TempDir(), "logs"), "synthetic-version")
	diagnostic.Status = intPointer(0)
	if err := json.NewEncoder(input).Encode(diagnostic); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

//go:noinline
func crashFixtureRecurse(depth int) int {
	if depth == 0 {
		return 0
	}
	return depth + crashFixtureRecurse(depth-1)
}

func readDiagnosticEntries(t *testing.T, directory string) []app.LogEntry {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []app.LogEntry
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("diagnostic permissions = %o", info.Mode().Perm())
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var entry app.LogEntry
			if json.Unmarshal(line, &entry) == nil {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

func TestCrashReaderRetainsUnhandledFailures(t *testing.T) {
	for _, mode := range []string{"goroutine", "fatal", "native", "normal"} {
		t.Run(mode, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "logs")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "__diagnostic_fixture", directory, mode)
			output, err := command.CombinedOutput()
			if !bytes.Contains(output, []byte("terminal-stdout-preserved")) {
				t.Fatalf("stdout changed: %q", output)
			}
			if mode == "normal" {
				if err != nil || len(readDiagnosticEntries(t, directory)) != 0 {
					t.Fatalf("normal completion was reported as failure: %v", err)
				}
				return
			}
			wantCode := 2
			if mode == "native" {
				wantCode = 17
			}
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != wantCode {
				t.Fatalf("original fatal exit changed: %v\n%s", err, output)
			}
			cause := "synthetic-goroutine-cause"
			if mode == "fatal" {
				cause = "fatal error: stack overflow"
			} else if mode == "native" {
				cause = "native-fatal-cause"
			}
			if !bytes.Contains(output, []byte(cause)) {
				t.Fatalf("original terminal cause missing: %s", output)
			}
			deadline := time.Now().Add(4 * time.Second)
			for {
				for _, entry := range readDiagnosticEntries(t, directory) {
					if strings.Contains(entry.Text, "synthetic-secret") || len([]rune(entry.Text)) > 4096 {
						t.Fatal("unbounded or unredacted diagnostic")
					}
					if entry.Event == "native_stderr" && strings.Contains(entry.Text, cause) && (mode == "native" || strings.Contains(entry.Text, "process_diagnostics_test.go:") || strings.Contains(entry.Text, "runtime/")) && strings.Contains(entry.Text, "version=synthetic-version") {
						return
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("fatal cause/stack did not survive subprocess termination")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

type failedDiagnosticOutput struct{}

func (failedDiagnosticOutput) Write(data []byte) (int, error) {
	return 0, fmt.Errorf("synthetic terminal failure")
}

func TestStderrCaptureIsBoundedAndDrainsDespiteOutputFailure(t *testing.T) {
	input := strings.Repeat("normal status\n", maxCrashBytes) + "panic: retained-fatal-cause\n" + strings.Repeat("stack frame\n", maxCrashBytes)
	var terminal bytes.Buffer
	for _, output := range []io.Writer{&terminal, failedDiagnosticOutput{}} {
		captured, err := captureProcessStderr(strings.NewReader(input), output)
		if err != nil || len(captured) > maxCrashBytes+64 || !bytes.Contains(captured, []byte("retained-fatal-cause")) {
			t.Fatalf("bounded capture failed: bytes=%d error=%v", len(captured), err)
		}
	}
	if terminal.String() != input {
		t.Fatal("terminal output changed")
	}
}

func TestReaderFailurePreservesOriginalExitAndStderr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "__diagnostic_fixture", filepath.Join(t.TempDir(), "logs"), "capture-failure")
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("reader loss changed original exit to %v", err)
	}
	if !bytes.Contains(output, []byte("original-failure-after-reader-loss")) || !bytes.Contains(output, []byte("durable process diagnostics unavailable")) || bytes.Count(output, []byte("terminal data")) != 10000 {
		t.Fatal("reader-loss fallback lost terminal output or its warning")
	}
}

func TestReaderStartupFailureLeavesStderrAttached(t *testing.T) {
	before, err := os.Stderr.Stat()
	if err != nil {
		t.Fatal(err)
	}
	d := newProcessDiagnostic(t.TempDir(), "synthetic-version")
	if _, err := startCrashReader(d, exec.Command(filepath.Join(t.TempDir(), "missing-reader"))); err == nil {
		t.Fatal("missing reader accepted")
	}
	after, err := os.Stderr.Stat()
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("reader startup failure replaced stderr")
	}
}

func TestNativeExitRecordsObservedResult(t *testing.T) {
	for _, test := range []struct {
		name, signal, level string
		status              *int
	}{
		{name: "quit", status: new(int), level: "info"},
		{name: "error", status: intPointer(9), level: "error"},
		{name: "update", status: intPointer(75), level: "info"},
		{name: "kill", signal: "SIGKILL", level: "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newProcessDiagnostic(filepath.Join(t.TempDir(), "logs"), "synthetic-version")
			d.Status, d.Signal = test.status, test.signal
			input, _ := json.Marshal(d)
			if err := runProcessDiagnostics("exit", bytes.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			var observed, stderr bool
			for _, entry := range readDiagnosticEntries(t, d.Directory) {
				if strings.Contains(entry.Text, "synthetic-secret") || strings.Contains(strings.ToLower(entry.Text), "oom") {
					t.Fatal("secret retained or unobserved OOM attributed")
				}
				if entry.Event == "native_exit" {
					observed = entry.Level == test.level && strings.Contains(entry.Text, "boundary=npm_launcher") && strings.Contains(entry.Text, "signal="+test.signal)
				}
				stderr = stderr || entry.Event == "native_stderr"
			}
			if !observed || stderr {
				t.Fatalf("native result missing or misclassified: observed=%v stderr=%v", observed, stderr)
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestDiagnosticStorageAndInputFailure(t *testing.T) {
	if err := runProcessDiagnostics("crash", strings.NewReader(strings.Repeat("x", 9000))); err == nil {
		t.Fatal("oversized header accepted")
	}
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := newProcessDiagnostic(filepath.Join(path, "logs"), "synthetic-version")
	d.Status = intPointer(9)
	input, _ := json.Marshal(d)
	if err := runProcessDiagnostics("exit", bytes.NewReader(input)); err == nil {
		t.Fatal("storage failure reported success")
	}
}
