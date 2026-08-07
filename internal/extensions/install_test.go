package extensions

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInstallAndRemoveGitExtension(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "fixture")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("name: fixture\nhooks:\n  message.received: [\"./hook\"]\n")
	if err := os.WriteFile(filepath.Join(repository, ManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"},
		{"add", ManifestName}, {"commit", "-m", "fixture"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	directory := filepath.Join(t.TempDir(), "extensions")
	installed, err := Install(context.Background(), directory, repository, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(installed, ManifestName)); err != nil {
		t.Fatal(err)
	}
	names, err := List(directory)
	if err != nil || len(names) != 1 || names[0] != "fixture" {
		t.Fatalf("installed extensions = %#v, %v", names, err)
	}
	if err := Remove(directory, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("extension was not removed: %v", err)
	}
}
