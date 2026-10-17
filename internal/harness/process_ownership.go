package harness

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const providerWriteTimeout = 5 * time.Second

type providerInput struct{ *os.File }

func providerStdin(command *exec.Cmd) (io.WriteCloser, error) {
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	file, ok := input.(*os.File)
	if !ok {
		_ = input.Close()
		return nil, errors.New("provider input requires a deadline-capable pipe")
	}
	return &providerInput{file}, nil
}

func (input *providerInput) Write(data []byte) (int, error) {
	if err := input.SetWriteDeadline(time.Now().Add(providerWriteTimeout)); err != nil {
		_ = input.Close()
		return 0, err
	}
	n, err := input.File.Write(data)
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A partial protocol frame cannot safely be followed by another request.
		_ = input.Close()
	}
	return n, err
}

var errProcessOwnershipBusy = errors.New("a previous Spynel or provider process still holds harness ownership; reconnecting automatically")

// Live process locks belong on the local per-user filesystem, not shared mounts.
func ProcessOwnershipPath(workspace string) (string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "spynel", "harness", fmt.Sprintf("%x.lock", sha256.Sum256([]byte(root)))), nil
}

// Provider guardians retain the lock until their direct provider has exited.
type processOwnership struct {
	path                string
	file                *os.File
	executable          string
	lifeRead, lifeWrite *os.File
}

func (o *processOwnership) acquire() error {
	if o.path == "" || o.file != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0700); err != nil {
		return err
	}
	var err error
	o.file, err = lockProcessOwnership(o.path)
	if err != nil {
		return err
	}
	o.executable, err = os.Executable()
	if err == nil {
		o.lifeRead, o.lifeWrite, err = os.Pipe()
	}
	if err != nil {
		_ = o.release()
	}
	return err
}

func (o *processOwnership) release() error {
	if o.file == nil {
		return nil
	}
	file := o.file
	o.file = nil
	_ = o.lifeWrite.Close()
	_ = o.lifeRead.Close()
	// Closing, rather than LOCK_UN, preserves locks held by surviving providers.
	return file.Close()
}

func startProvider(command *exec.Cmd, owner *processOwnership) error {
	if owner != nil && owner.file != nil {
		arguments := append([]string{owner.executable, "__provider", command.Path}, command.Args[1:]...)
		command.Path, command.Args = owner.executable, arguments
		command.ExtraFiles = append(command.ExtraFiles, owner.file, owner.lifeRead)
		command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
		command.WaitDelay = 0 // The guardian retains ownership until provider exit is confirmed.
	}
	return command.Start()
}

func waitProviderProcesses(group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	return waitProviderProcess(done)
}

func waitProviderProcess(done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(8 * time.Second):
		return errors.New("provider shutdown was not confirmed; recovery remains fenced")
	}
}
