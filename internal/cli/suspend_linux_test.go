package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestJobSuspensionGuard(t *testing.T) {
	if os.Getenv("SPYNEL_SUSPEND_PROBE") == "1" {
		restore := func() {}
		fmt.Println("unguarded")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if scanner.Text() == "guard" {
				restore = preventJobSuspension()
			}
			if scanner.Text() == "restore" {
				restore()
			}
			fmt.Println(scanner.Text())
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobSuspensionGuard$")
	cmd.Env = append(os.Environ(), "SPYNEL_SUSPEND_PROBE=1")
	// The parent stays in this session, in a different group: SIGTSTP really
	// stops the unguarded child, unlike an orphan-group fixture.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(out)
	expect := func(want string) {
		t.Helper()
		if !scanner.Scan() || scanner.Text() != want {
			t.Fatalf("probe: got %q, want %q", scanner.Text(), want)
		}
	}
	expect("unguarded")
	_ = cmd.Process.Signal(syscall.SIGTSTP)
	for {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
		if err != nil || ctx.Err() != nil {
			t.Fatalf("unguarded signal did not stop probe: %v %v", err, ctx.Err())
		}
		if strings.Contains(string(status), "State:\tT") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGCONT)
	fmt.Fprintln(in, "guard")
	expect("guard")
	_ = cmd.Process.Signal(syscall.SIGTSTP)
	time.Sleep(50 * time.Millisecond)
	fmt.Fprintln(in, "alive")
	expect("alive")
	fmt.Fprintln(in, "restore")
	expect("restore")
	_ = in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
