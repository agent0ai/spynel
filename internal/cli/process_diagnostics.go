package cli

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
)

const maxCrashBytes = 64 << 10

var capturedStderrTerminal bool
var processRun = diagnosticRunID()

// This private launcher protocol contains no arguments, configuration or environment.
type processDiagnostic struct {
	Directory string `json:"directory"`
	Instance  string `json:"instance"`
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	PID       int    `json:"pid"`
	Status    *int   `json:"status,omitempty"`
	Signal    string `json:"signal,omitempty"`
}

func (d processDiagnostic) identity() string {
	return fmt.Sprintf("pid=%d run=%s version=%s revision=%s", d.PID, d.Instance, d.Version, d.Revision)
}

func newProcessDiagnostic(directory, version string) processDiagnostic {
	revision := "unknown"
	modified := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				modified = true
			}
		}
	}
	if modified {
		revision += "-dirty"
	}
	return processDiagnostic{
		Directory: directory, Version: version, Revision: revision, PID: os.Getpid(),
		Instance: fmt.Sprintf("pid-%d-%s", os.Getpid(), processRun),
	}
}

func diagnosticRunID() string {
	if inherited, err := hex.DecodeString(os.Getenv("SPYNEL_DIAGNOSTICS_RUN")); err == nil && len(inherited) == 8 {
		return hex.EncodeToString(inherited)
	}
	return time.Now().UTC().Format("20060102t150405.000000000")
}

func diagnosticsWarning() {
	// Never send storage failures back through the runtime logger or include private payloads.
	fmt.Fprintln(os.Stderr, "spynel: durable process diagnostics unavailable; retain terminal stderr and host exit records")
}

func beginProcessDiagnostics(cfg config.Config, version string) (*app.Runtime, func()) {
	d := newProcessDiagnostic(cfg.StatePath("runtime", "logs"), version)
	r := app.NewRuntimeAt(d.Directory, d.Instance)
	r.LogEvent("info", "process", "process_started", d.identity())
	capturedStderrTerminal = terminalOutput(os.Stderr)
	if executor != nil {
		capturedStderrTerminal = terminalOutput(os.Stdout)
		if executorDiagnostic(d) != nil {
			diagnosticsWarning()
		}
		return r, func() { warnDiagnosticStorage(r, d.Instance) }
	}
	// The npm parent owns the actual exit result. This reader owns stderr even
	// when the native process fails without running Go defers.
	if os.Getenv("SPYNEL_DIAGNOSTICS_FD") == "3" {
		_ = os.Unsetenv("SPYNEL_DIAGNOSTICS_FD") // An exec restart must not reuse a closed descriptor.
		file := os.NewFile(3, "launcher-diagnostics")
		err := json.NewEncoder(file).Encode(d)
		_ = file.Close()
		if err != nil {
			diagnosticsWarning()
		}
	}
	executable, err := os.Executable()
	if err != nil {
		diagnosticsWarning()
		return r, func() { warnDiagnosticStorage(r, d.Instance) }
	}
	stop, err := startCrashReader(d, exec.Command(executable, "__diagnostics", "crash"))
	if err != nil {
		diagnosticsWarning()
		return r, func() { warnDiagnosticStorage(r, d.Instance) }
	}
	return r, func() {
		stop()
		warnDiagnosticStorage(r, d.Instance)
	}
}

func warnDiagnosticStorage(r *app.Runtime, instance string) {
	for _, entry := range r.Logs() {
		if entry.Instance == instance && entry.Component == "runtime_log" {
			diagnosticsWarning()
			return
		}
	}
}

func startCrashReader(d processDiagnostic, command *exec.Cmd) (func(), error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	command.Stdin = reader
	command.Stderr = os.Stderr
	ack, err := command.StdoutPipe()
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = ack.Close()
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	done := make(chan error, 1)
	initialized := make(chan struct{})
	activated := false
	restore := func() {}
	var original *os.File
	var closeOnce sync.Once
	finish := func() { closeOnce.Do(func() { restore(); _ = writer.Close() }) }
	go func() {
		err := command.Wait()
		<-initialized
		finish()
		// Keep a spare read end until restoration: a reader failure must never
		// turn application writes into SIGPIPE. Drain pending stderr afterward.
		if activated {
			// A separate File avoids the write lock held by an application
			// goroutine still completing a large write to the old pipe.
			_, _ = io.Copy(original, reader)
			_ = original.Close()
			if err != nil {
				diagnosticsWarning()
			}
		}
		_ = reader.Close()
		done <- err
	}()
	stop := func() {
		finish()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// This process owns only the diagnostic reader, never application work.
			_ = command.Process.Kill()
			diagnosticsWarning()
		}
	}
	err = json.NewEncoder(writer).Encode(d)
	if err == nil {
		ready := make(chan error, 1)
		go func() {
			var value [1]byte
			_, err := io.ReadFull(ack, value[:])
			if err == nil && value[0] != 'R' {
				err = errors.New("invalid diagnostic reader readiness")
			}
			ready <- err
		}()
		select {
		case err = <-ready:
		case <-time.After(2 * time.Second):
			err = errors.New("diagnostic reader startup timed out")
		}
	}
	_ = ack.Close()
	if err == nil {
		restore, original, err = redirectDiagnosticStderr(writer)
		activated = err == nil
	}
	if err != nil {
		restore = func() {}
	}
	close(initialized)
	if err != nil {
		stop()
		return nil, err
	}
	return stop, nil
}

func runProcessDiagnostics(mode string, input io.Reader) error {
	if mode != "crash" && mode != "exit" {
		return errors.New("invalid diagnostic mode")
	}
	var d processDiagnostic
	var capture []byte
	reader := bufio.NewReaderSize(input, 8192)
	if mode == "crash" {
		// The reader's lifetime follows pipe EOF, not terminal process-group signals.
		signal.Ignore(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
		header, err := reader.ReadSlice('\n')
		if err != nil || json.Unmarshal(header, &d) != nil {
			return errors.New("invalid diagnostic header")
		}
	} else {
		decoder := json.NewDecoder(io.LimitReader(input, 8193))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&d); err != nil {
			return errors.New("invalid diagnostic exit record")
		}
	}
	if !filepath.IsAbs(d.Directory) || len(d.Instance) > 39 || d.Instance == "" || d.PID <= 0 ||
		len(d.Directory) > 4096 || len(d.Version) > 128 || len(d.Revision) > 64 || len(d.Signal) > 32 ||
		d.Status != nil && (*d.Status < 0 || *d.Status > 255) {
		return errors.New("invalid diagnostic identity")
	}
	if mode == "crash" {
		if _, err := os.Stdout.Write([]byte{'R'}); err != nil {
			return errors.New("diagnostic reader readiness failed")
		}
		var readErr error
		capture, readErr = captureProcessStderr(reader, os.Stderr)
		if readErr != nil {
			return errors.New("diagnostic input failed")
		}
		if len(capture) == 0 {
			return nil
		}
	}
	r := app.NewRuntimeAt(d.Directory, d.Instance+"-observer")
	defer r.Close()
	if mode == "exit" {
		level := "info"
		if d.Signal != "" || d.Status == nil || *d.Status != 0 && *d.Status != npmUpdateExitCode {
			level = "error"
		}
		status := "unknown"
		if d.Status != nil {
			status = fmt.Sprint(*d.Status)
		}
		r.LogEvent(level, "process", "native_exit", fmt.Sprintf("%s boundary=npm_launcher status=%s signal=%s", d.identity(), status, d.Signal))
	}
	if strings.TrimSpace(string(capture)) != "" {
		r.LogEvent("info", "process", "native_stderr", d.identity()+" boundary=stderr_reader\n"+string(capture))
	}
	r.Close()
	// Storage failures remain honest and visible even in this short-lived reader.
	for _, entry := range r.Logs() {
		if entry.Instance == d.Instance+"-observer" && entry.Component == "runtime_log" {
			return errors.New("diagnostic storage unavailable")
		}
	}
	return nil
}

// Keep a bounded tail until a fatal header appears, then preserve its beginning.
// Drain and mirror all bytes regardless of storage/output errors or excerpt size.
func captureProcessStderr(input io.Reader, terminal io.Writer) ([]byte, error) {
	var captured []byte
	var fatal, truncated bool
	buffer := make([]byte, 4096)
	for {
		n, err := input.Read(buffer)
		chunk := buffer[:n]
		_, _ = terminal.Write(chunk)
		if !fatal {
			captured = append(captured, chunk...)
			for _, header := range [][]byte{[]byte("panic:"), []byte("fatal error:"), []byte("SIGSEGV"), []byte("SIGABRT"), []byte("SIGQUIT"), []byte("SIGBUS"), []byte("SIGILL"), []byte("SIGFPE"), []byte("terminate called")} {
				if index := bytes.Index(captured, header); index >= 0 {
					captured = captured[index:]
					fatal = true
					break
				}
			}
			if len(captured) > maxCrashBytes {
				truncated = true
				if fatal {
					captured = captured[:maxCrashBytes]
				} else {
					captured = append([]byte(nil), captured[len(captured)-maxCrashBytes:]...)
				}
			}
		} else if len(captured) < maxCrashBytes {
			captured = append(captured, chunk[:min(len(chunk), maxCrashBytes-len(captured))]...)
		} else if n > 0 {
			truncated = true
		}
		if err != nil {
			if !fatal && truncated {
				// Do not retain a trailing fragment of a credential-bearing line.
				if newline := bytes.IndexByte(captured, '\n'); newline >= 0 {
					captured = captured[newline+1:]
				} else {
					captured = nil
				}
			}
			if truncated {
				captured = append(captured, []byte("\n[stderr excerpt truncated]")...)
			}
			if err == io.EOF {
				err = nil
			}
			return captured, err
		}
	}
}
