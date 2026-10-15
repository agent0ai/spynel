//go:build linux || darwin

package harness

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func lockProcessOwnership(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open harness ownership lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		return nil, errors.New("harness ownership lock must be a private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errProcessOwnershipBusy
		}
		return nil, fmt.Errorf("lock harness ownership: %w", err)
	}
	return file, nil
}

// RunProvider is the private provider guardian; protocol bytes pass through unchanged.
func RunProvider(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("missing provider command")
	}
	lock, life := os.NewFile(3, "provider-ownership"), os.NewFile(4, "provider-lifetime")
	defer lock.Close()
	defer life.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("invalid provider ownership descriptor")
	}
	if info, err := life.Stat(); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("invalid provider lifetime descriptor")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)
	command := exec.Command(arguments[0], arguments[1:]...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	// Observe exit without reaping: the reserved leader PID fences group signals.
	go func() { done <- waitProviderExit(command.Process.Pid) }()
	finish := func(observation error) error {
		waitErr := command.Wait()
		if observation != nil {
			return errors.Join(observation, waitErr)
		}
		return waitErr
	}
	lost := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, life); close(lost) }()
	select {
	case err := <-done:
		if err != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
		return finish(err)
	case <-lost:
	case <-stop:
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return finish(err)
	case <-timer.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return finish(<-done)
	}
}
