//go:build !windows

package localapi

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestSocketLifecycle(t *testing.T) {
	_, server, _, stop, done := startTestServer(t, t.TempDir())
	defer func() { stop(); <-done; _ = server.Service.Close() }()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "api.sock")
	listener, err := ListenSocket(path, server.Service.Config.Root, server.Token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	closed := make(chan error, 1)
	go func() { closed <- server.Serve(ctx, listener) }()
	for _, name := range []string{path, path + ".json"} {
		info, err := os.Lstat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private mode: %v %v", info, err)
		}
	}
	client, err := NewSocketClient(path)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, stopRequest := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopRequest()
	if err := client.Handle(requestCtx, core.Message{Channel: "cli", Conversation: "socket", Text: "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if other, err := ListenSocket(path, "another-workspace", strings.Repeat("b", 64)); err == nil {
		_ = other.Close()
		t.Fatal("second workspace replaced socket")
	}
	if err := os.Chmod(path+".json", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSocketClient(path); err == nil {
		t.Fatal("public token file accepted")
	}
	_ = os.Chmod(path+".json", 0o600)
	cancel()
	<-closed
	for _, name := range []string{path, path + ".json"} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("server left socket artifact: %v", err)
		}
	}
	// Unrelated files, symlinks and stale sockets are refused without deletion.
	for _, kind := range []string{"regular", "symlink", "stale"} {
		switch kind {
		case "regular":
			_ = os.WriteFile(path, []byte("unrelated"), 0o600)
		case "symlink":
			_ = os.Symlink("missing-target", path)
		case "stale":
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			l.SetUnlinkOnClose(false)
			_ = l.Close()
		}
		before, _ := os.Lstat(path)
		if l, err := ListenSocket(path, "other", server.Token); err == nil {
			_ = l.Close()
			t.Fatalf("overwrote %s", kind)
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("removed unrelated %s", kind)
		}
		_ = os.Remove(path)
	}
	_ = os.Chmod(directory, 0o755)
	if l, err := ListenSocket(path, "other", server.Token); err == nil {
		_ = l.Close()
		t.Fatal("public socket directory accepted")
	}
}

func TestSocketCleanupPreservesReplacement(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "api.sock")
	l, err := ListenSocket(path, "workspace", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	owned := l.(*privateSocket)
	for _, name := range []string{path, path + ".json"} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A retained handle makes descriptor inode reuse impossible, without relying on allocator luck.
	pinned, err := owned.authFile.Stat()
	if err != nil || !os.SameFile(pinned, owned.authInfo) {
		t.Fatalf("descriptor ownership lost: %v", err)
	}
	replacement, err := os.Stat(path + ".json")
	if err != nil || os.SameFile(pinned, replacement) {
		t.Fatalf("descriptor inode reused before cleanup: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.authFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor handle not released: %v", err)
	}
	for _, name := range []string{path, path + ".json"} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != "replacement" {
			t.Fatal("cleanup deleted a replacement")
		}
	}
}

func TestSocketConcurrentClosePreservesSuccessor(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "api.sock")
	listener, err := ListenSocket(path, "workspace", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			if err := listener.Close(); err != nil {
				t.Errorf("concurrent close: %v", err)
			}
		})
	}
	callers.Wait()
	successor, err := ListenSocket(path, "workspace", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	descriptor, err := readSocketDescriptor(path)
	if err != nil || descriptor.Token != strings.Repeat("b", 64) {
		t.Fatalf("repeated close damaged successor: %v", err)
	}
}
