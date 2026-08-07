package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreSanitizesLimitsAndKeepsUniqueAttachments(t *testing.T) {
	root := t.TempDir()
	store := Store{Directory: filepath.Join(root, "attachments"), MaxBytes: 5}
	first, err := store.Save(context.Background(), "../notes weird].txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(context.Background(), "notes weird].txt", strings.NewReader("again"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path || first.Name != "notes_weird_.txt" || second.Name != "notes_weird_-2.txt" {
		t.Fatalf("attachments = %#v, %#v", first, second)
	}
	if !strings.Contains(first.Token(), "[Attachment notes_weird_.txt]") || !strings.Contains(first.Token(), filepath.ToSlash(first.Path)) {
		t.Fatalf("token = %q", first.Token())
	}
	if _, err := store.Save(context.Background(), "large.bin", strings.NewReader("123456")); err == nil {
		t.Fatal("oversized attachment was accepted")
	}
	if _, err := os.Stat(filepath.Join(store.Directory, "large.bin")); !os.IsNotExist(err) {
		t.Fatalf("oversized attachment remained on disk: %v", err)
	}
}
