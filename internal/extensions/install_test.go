package extensions

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInstallProcessHelper(t *testing.T) {
	if os.Getenv("SPYNEL_INSTALL_PROCESS_HELPER") == "" {
		return
	}
	_, _ = os.Stderr.WriteString("fatal: unable to access 'https://clone-user:LEAKED-URL-PASSWORD@example.com/repo.git/': failure\nclone helper failed")
	code, _ := strconv.Atoi(os.Getenv("SPYNEL_INSTALL_PROCESS_EXIT"))
	os.Exit(code)
}

func TestInstallCapturesBoundedAttributedFailureEvidence(t *testing.T) {
	t.Setenv("SPYNEL_INSTALL_PROCESS_HELPER", "1")
	t.Setenv("SPYNEL_INSTALL_PROCESS_EXIT", "23")
	factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=TestInstallProcessHelper")
	}
	var log bytes.Buffer
	_, err := install(context.Background(), filepath.Join(t.TempDir(), "extensions"), "fixture", "fixture", factory, &log)
	if err == nil {
		t.Fatal("install succeeded")
	}
	entry := log.String()
	for _, want := range []string{"process=git", "operation=clone", "stream=stderr", "truncated=false", "LEAKED-URL-PASSWORD", "example.com/repo.git", "clone helper failed", "event=exit", "status=failed", "exit_code=23"} {
		if !strings.Contains(entry, want) {
			t.Fatalf("clone evidence missing %q (length %d)", want, len(entry))
		}
	}
	bounded := &installOutput{}
	_, _ = bounded.Write([]byte(strings.Repeat("y", maxInstallOutput+1024)))
	if bounded.Len() != maxInstallOutput || !bounded.truncated {
		t.Fatalf("bounded output = %d bytes, truncated=%t", bounded.Len(), bounded.truncated)
	}
}

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
