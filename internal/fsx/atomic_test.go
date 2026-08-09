package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicCreateFileNeverReplacesExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instruction.md")
	if err := AtomicCreateFile(path, []byte("owner edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCreateFile(path, []byte("default"), 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "owner edit" {
		t.Fatalf("existing content = %q, %v", data, err)
	}
}
