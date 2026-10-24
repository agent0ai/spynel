//go:build !windows

package fsx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicCreateOpenFileRetainsPublishedInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "descriptor.json")
	file, err := AtomicCreateOpenFile(path, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	owned, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.Stat(path)
	if err != nil || !os.SameFile(owned, published) || published.Mode().Perm() != 0o600 {
		t.Fatalf("published identity or mode: %v, %v", published, err)
	}
	if other, err := AtomicCreateOpenFile(path, []byte("clobber"), 0o600); !errors.Is(err, os.ErrExist) || other != nil {
		if other != nil {
			other.Close()
		}
		t.Fatalf("existing file accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Stat(path)
	if err != nil || os.SameFile(owned, replacement) {
		t.Fatalf("original inode reused while retained: %v", err)
	}
	data, err := io.ReadAll(io.NewSectionReader(file, 0, owned.Size()))
	if err != nil || string(data) != "original" {
		t.Fatalf("lost original handle: %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("temporary files leaked: %v, %v", entries, err)
	}
}
