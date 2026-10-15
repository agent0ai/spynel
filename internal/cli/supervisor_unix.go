//go:build linux || darwin

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/updater"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

const supervisorProtocol = "spynel.executor/v1"

type lifecycleMessage struct {
	Kind       string             `json:"kind"`
	Protocol   string             `json:"protocol,omitempty"`
	Generation string             `json:"generation,omitempty"`
	PID        int                `json:"pid,omitempty"`
	Args       []string           `json:"args,omitempty"`
	CWD        string             `json:"cwd,omitempty"`
	Diagnostic *processDiagnostic `json:"diagnostic,omitempty"`
	Update     *updater.Result    `json:"update,omitempty"`
}

type executorControl struct {
	connection net.Conn
	mu         sync.Mutex
	ack        chan bool
	ctx        context.Context
	cancel     context.CancelFunc
	restart    chan struct{}
	parent     int
	generation string
	unregister func()
}

var executor *executorControl

func supervisorEligible(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if args[0] != "serve" && args[0] != "init" {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--automatic-startup" || strings.HasPrefix(arg, "--automatic-startup=") || arg == "--no-start" || strings.HasPrefix(arg, "--no-start=") || arg == "--help" || arg == "-h" {
			return false
		}
	}
	return args[0] == "serve" || interactiveTerminal()
}

func executorContext() context.Context {
	if executor != nil {
		return executor.ctx
	}
	return context.Background()
}

func executorIntent(kind string) error {
	if executor == nil {
		return nil
	}
	return executor.request(lifecycleMessage{Kind: kind})
}

func (c *executorControl) request(message lifecycleMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(c.connection).Encode(message); err != nil {
		c.cancel()
		return errors.New("supervisor control unavailable")
	}
	select {
	case ok := <-c.ack:
		if ok {
			return nil
		}
	case <-time.After(2 * time.Second):
	}
	c.cancel()
	return errors.New("supervisor did not acknowledge lifecycle intent")
}

func readLifecycle(connection net.Conn, receive func(lifecycleMessage) bool) {
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), 32<<10)
	for scanner.Scan() {
		var message lifecycleMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil || !receive(message) {
			return
		}
	}
}

func enterExecutor(args []string, version string) ([]string, error) {
	if len(args) < 3 {
		return nil, errors.New("invalid internal executor invocation")
	}
	parent, err := strconv.Atoi(args[1])
	if err != nil || parent != os.Getppid() {
		return nil, errors.New("invalid executor parent")
	}
	file := os.NewFile(3, "executor-control")
	unix.CloseOnExec(3)
	connection, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return nil, errors.New("executor requires inherited supervisor control")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &executorControl{connection: connection, ack: make(chan bool, 1), ctx: ctx, cancel: cancel, restart: make(chan struct{}, 1), parent: parent, generation: args[2]}
	go func() {
		defer cancel()
		readLifecycle(connection, func(message lifecycleMessage) bool {
			switch message.Kind {
			case "ack":
				c.ack <- true
			case "shutdown":
				cancel()
			case "restart":
				select {
				case c.restart <- struct{}{}:
				default:
				}
			default:
				return false
			}
			return true
		})
	}()
	if err := c.request(lifecycleMessage{Kind: "hello", Protocol: supervisorProtocol, Generation: args[2], PID: os.Getpid()}); err != nil {
		_ = connection.Close()
		return nil, err
	}
	// Registry identity and actual native image validate the inherited owner.
	if err := updater.ValidateExecutorParent(parent, args[2]); err != nil {
		_ = connection.Close()
		return nil, err
	}
	owner, err := updater.ProcessRecord(parent)
	if err != nil || owner.Version != version {
		_ = connection.Close()
		return nil, errors.New("executor version does not match supervisor")
	}
	c.unregister, err = updater.Detect(version).RegisterExecutor(parent, args[2])
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := c.request(lifecycleMessage{Kind: "registered"}); err != nil {
		c.unregister()
		_ = connection.Close()
		return nil, err
	}
	executor = c
	return args[3:], nil
}

func completeExecutorRun(err error) error {
	message := lifecycleMessage{Kind: "stop"}
	var restart *restartRequest
	var update *updateRequest
	if errors.As(err, &restart) {
		message.Kind, message.Args = "restart_done", restart.args
	} else if errors.As(err, &update) {
		message.Kind, message.Args, message.Update = "update_done", update.args, &update.result
	} else if err != nil {
		return err // Startup failure or an unexpected runtime error remains an error.
	}
	if notifyErr := executor.request(message); notifyErr != nil {
		return notifyErr
	}
	return nil
}

func executorDiagnostic(d processDiagnostic) error {
	return executor.request(lifecycleMessage{Kind: "diagnostics", Diagnostic: &d})
}

func admitExecutor(cfgDirectory, version string, args []string) error {
	if executor == nil {
		return nil
	}
	cwd, err := canonicalLaunchDirectory()
	if err != nil {
		return err
	}
	d := newProcessDiagnostic(cfgDirectory, version)
	return executor.request(lifecycleMessage{Kind: "ready", Args: args, CWD: cwd, Diagnostic: &d})
}

type supervisedExit struct {
	status int
	signal os.Signal
}

func (e supervisedExit) Error() string         { return "executor exited" }
func (e supervisedExit) ExitCode() int         { return e.status }
func (e supervisedExit) ExitSignal() os.Signal { return e.signal }

type supervisorAttempt struct {
	mu                    sync.Mutex
	hello, ready, stopped bool
	action                string
	args                  []string
	cwd                   string
	diagnostic            *processDiagnostic
	update                *updater.Result
	admitted              time.Time
}

func runSupervisor(args []string, version string) error {
	defer preventJobSuspension()()
	manager := updater.Detect(version)
	unregister, err := manager.RegisterSupervisor()
	if err != nil {
		return fmt.Errorf("register native supervisor: %w", err)
	}
	defer unregister()
	owner, err := updater.ProcessRecord(os.Getpid())
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, updater.RestartSignal())
	defer signal.Stop(signals)
	var terminalState *term.State
	if term.IsTerminal(os.Stdin.Fd()) {
		terminalState, _ = term.GetState(os.Stdin.Fd())
	}
	restore := func() {
		if terminalState != nil {
			_ = term.Restore(os.Stdin.Fd(), terminalState)
			// Restore only modes the executor can own; never erase shell history.
			fmt.Fprint(os.Stdout, "\x1b[0m\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[?25h\x1b[?1049l")
		}
	}
	defer restore()
	cwd, err := canonicalLaunchDirectory()
	if err != nil {
		return err
	}
	for failures := 0; ; {
		select {
		case sig := <-signals:
			if sig != updater.RestartSignal() || queuedSupervisorStop(signals) {
				return nil
			}
			return replaceCurrentProcess(args)
		default:
		}
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			return err
		}
		unix.CloseOnExec(pair[0])
		unix.CloseOnExec(pair[1])
		parentFile, childFile := os.NewFile(uintptr(pair[0]), "supervisor-control"), os.NewFile(uintptr(pair[1]), "executor-control")
		control, err := net.FileConn(parentFile)
		_ = parentFile.Close()
		if err != nil {
			_ = childFile.Close()
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			_ = control.Close()
			_ = childFile.Close()
			return err
		}
		stable := updater.RestartExecutable(executable)
		currentInfo, currentErr := os.Stat(executable)
		stableInfo, stableErr := os.Stat(stable)
		if stable != executable && (currentErr != nil || stableErr != nil || !os.SameFile(currentInfo, stableInfo)) {
			_ = control.Close()
			_ = childFile.Close()
			return replaceCurrentProcess(args)
		}
		command := exec.Command(executable, append([]string{"__executor", strconv.Itoa(os.Getpid()), owner.Generation}, args...)...)
		command.Dir, command.Stdin, command.Stdout = cwd, os.Stdin, os.Stdout
		command.Env = cleanExecutorEnvironment(os.Environ())
		command.ExtraFiles = []*os.File{childFile}
		reader, writer, err := os.Pipe()
		if err != nil {
			_ = control.Close()
			_ = childFile.Close()
			return err
		}
		command.Stderr = writer
		if err := command.Start(); err != nil {
			_ = control.Close()
			_ = childFile.Close()
			_ = reader.Close()
			_ = writer.Close()
			return err
		}
		_ = childFile.Close()
		_ = writer.Close()
		attempt := &supervisorAttempt{}
		capture := make(chan []byte, 1)
		go func() { bytes, _ := captureProcessStderr(reader, os.Stderr); capture <- bytes }()
		var writeMu sync.Mutex
		send := func(kind string) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = control.SetWriteDeadline(time.Now().Add(2 * time.Second))
			return json.NewEncoder(control).Encode(lifecycleMessage{Kind: kind})
		}
		controlDone := make(chan struct{})
		go func() {
			defer close(controlDone)
			readLifecycle(control, func(message lifecycleMessage) bool {
				attempt.mu.Lock()
				defer attempt.mu.Unlock()
				if !attempt.hello {
					if message.Kind != "hello" || message.Protocol != supervisorProtocol || message.PID != command.Process.Pid || message.Generation != owner.Generation {
						return false
					}
					attempt.hello = true
				} else {
					switch message.Kind {
					case "registered":
						if err := updater.SetExecutor(command.Process.Pid, false, false); err != nil {
							return false
						}
					case "diagnostics":
						if message.Diagnostic == nil || message.Diagnostic.PID != command.Process.Pid {
							return false
						}
						attempt.diagnostic = message.Diagnostic
						publishLauncherDiagnostic(*message.Diagnostic)
					case "ready":
						if message.Diagnostic == nil || message.Diagnostic.PID != command.Process.Pid || message.CWD == "" || len(message.Args) == 0 {
							return false
						}
						if err := updater.SetExecutor(command.Process.Pid, true, false); err != nil {
							return false
						}
						attempt.ready, attempt.args, attempt.cwd, attempt.diagnostic = true, message.Args, message.CWD, message.Diagnostic
						attempt.admitted = time.Now()
						publishLauncherDiagnostic(*message.Diagnostic)
					case "stop":
						attempt.stopped = true
					case "restart_intent", "update_intent":
						attempt.action = message.Kind
					case "restart_done", "update_done":
						attempt.action, attempt.args, attempt.update = message.Kind, message.Args, message.Update
					default:
						return false
					}
				}
				return send("ack") == nil
			})
		}()
		wait := make(chan error, 1)
		go func() { wait <- command.Wait() }()
		controlClosed := controlDone
		var waitErr error
		stopping := false
		var stopTimer <-chan time.Time
		for running := true; running; {
			select {
			case waitErr = <-wait:
				running = false
			case <-controlClosed:
				controlClosed = nil
				// Intentional EOF on ordinary exit still waits for the child.
				if !stopping {
					stopTimer = time.After(10 * time.Second)
				}
			case sig := <-signals:
				if sig == updater.RestartSignal() && !stopping {
					_ = send("restart")
				} else if sig != updater.RestartSignal() && !stopping {
					stopping = true
					_ = updater.SetExecutor(0, false, true)
					_ = send("shutdown")
					stopTimer = time.After(10 * time.Second)
				}
			case <-stopTimer:
				stopping = true
				_ = command.Process.Kill()
				stopTimer = nil
			}
		}
		_ = control.Close()
		<-controlDone
		var stderr []byte
		select {
		case stderr = <-capture:
		case <-time.After(2 * time.Second):
			_ = reader.Close()
			stderr = <-capture
		}
		_ = reader.Close()
		restore()
		if err := updater.RetireExecutor(command.Process.Pid); err != nil {
			fmt.Fprintln(os.Stderr, "spynel: executor registration could not be retired")
		}
		_ = updater.SetExecutor(0, false, stopping)
		retainExecutorExit(attempt.diagnostic, command.ProcessState, stderr)
		if stopping || attempt.stopped {
			return executorExit(waitErr, command.ProcessState)
		}
		if attempt.ready {
			if err := os.Chdir(attempt.cwd); err != nil {
				return err
			}
		}
		if queuedSupervisorStop(signals) {
			return executorExit(waitErr, command.ProcessState)
		}
		if attempt.action == "restart_done" {
			return replaceCurrentProcess(attempt.args)
		}
		if attempt.action == "update_done" {
			if attempt.update == nil {
				return errors.New("update handoff missing")
			}
			request := &updateRequest{args: attempt.args, standalone: manager.InstallRoot != "", result: *attempt.update, manager: manager}
			if request.standalone {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					for {
						select {
						case <-done:
							return
						case sig := <-signals:
							if sig != updater.RestartSignal() {
								cancel()
								return
							}
						}
					}
				}()
				err := completeRunContext(ctx, request, replaceCurrentProcess)
				close(done)
				stopped := ctx.Err() != nil
				cancel()
				if stopped {
					return nil
				}
				return err
			}
			return request
		}
		if waitErr == nil || !attempt.ready || attempt.action != "" {
			return executorExit(waitErr, command.ProcessState)
		}
		if !attempt.admitted.IsZero() && time.Since(attempt.admitted) >= time.Minute {
			failures = 0
		}
		if failures >= 3 {
			return executorExit(waitErr, command.ProcessState)
		}
		failures++
		args, cwd = attempt.args, attempt.cwd
		fmt.Fprintln(os.Stderr, "spynel: executor exited unexpectedly; restarting after bounded delay")
		timer := time.NewTimer(time.Duration(failures) * time.Second)
		select {
		case sig := <-signals:
			timer.Stop()
			if sig == updater.RestartSignal() && !queuedSupervisorStop(signals) {
				return replaceCurrentProcess(args)
			}
			return nil
		case <-timer.C:
		}
	}
}

// At handoff, stop wins over already queued restart requests.
func queuedSupervisorStop(signals <-chan os.Signal) bool {
	stopped := false
	for {
		select {
		case observed := <-signals:
			stopped = stopped || observed != updater.RestartSignal()
		default:
			return stopped
		}
	}
}

func cleanExecutorEnvironment(environment []string) []string {
	var clean []string
	for _, value := range environment {
		if !strings.HasPrefix(value, "SPYNEL_DIAGNOSTICS_FD=") {
			clean = append(clean, value)
		}
	}
	return clean
}

func publishLauncherDiagnostic(d processDiagnostic) {
	if os.Getenv("SPYNEL_DIAGNOSTICS_FD") != "3" {
		return
	}
	_ = os.Unsetenv("SPYNEL_DIAGNOSTICS_FD")
	d.PID = os.Getpid()
	file := os.NewFile(3, "launcher-diagnostics")
	if json.NewEncoder(file).Encode(d) != nil {
		diagnosticsWarning()
	}
	_ = file.Close()
}

func retainExecutorExit(d *processDiagnostic, state *os.ProcessState, stderr []byte) {
	if d == nil || state == nil {
		return
	}
	r := app.NewRuntimeAt(d.Directory, d.Instance+"-supervisor")
	defer r.Close()
	status, observed := state.ExitCode(), ""
	if wait, ok := state.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		observed = wait.Signal().String()
	}
	r.LogEvent("info", "process", "executor_exit", fmt.Sprintf("%s boundary=native_supervisor status=%d signal=%s", d.identity(), status, observed))
	if strings.TrimSpace(string(stderr)) != "" {
		r.LogEvent("info", "process", "native_stderr", d.identity()+" boundary=native_supervisor\n"+string(stderr))
	}
	r.Close()
	warnDiagnosticStorage(r, d.Instance+"-supervisor")
}

func executorExit(err error, state *os.ProcessState) error {
	if err == nil {
		return nil
	}
	if state == nil {
		return err
	}
	status := state.ExitCode()
	var observed os.Signal
	if wait, ok := state.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		status = 128 + int(wait.Signal())
		observed = wait.Signal()
	}
	return supervisedExit{status: status, signal: observed}
}
