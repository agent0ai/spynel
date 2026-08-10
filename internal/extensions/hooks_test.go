package extensions

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const extensionProcessFixtureEnv = "SPYNEL_EXTENSION_PROCESS_FIXTURE"

func TestExtensionProcessFixture(t *testing.T) {
	if os.Getenv(extensionProcessFixtureEnv) != "fail-empty-stderr" {
		return
	}
	os.Exit(9)
}

func TestFailedHookWithoutStderrStillWritesProcessEvidence(t *testing.T) {
	root := t.TempDir()
	extension := filepath.Join(root, "silent-failure")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := yaml.Marshal(Manifest{Name: "silent-failure", Hooks: map[string][]string{
		"message.received": {os.Args[0], "-test.run=^TestExtensionProcessFixture$"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, ManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(extensionProcessFixtureEnv, "fail-empty-stderr")
	var diagnostic bytes.Buffer
	_, runErr := (Runner{Directory: root, Timeout: 5 * time.Second, Log: &diagnostic}).Run(context.Background(), "message.received", map[string]any{"text": "not logged"})
	if runErr == nil {
		t.Fatal("silent nonzero hook unexpectedly succeeded")
	}
	got := diagnostic.String()
	if !strings.Contains(got, "process_failed=exit status 9") || !strings.Contains(got, "stderr_present=false") {
		t.Fatalf("silent process evidence = %q", got)
	}
	if strings.Contains(got, "not logged") {
		t.Fatalf("hook input leaked into diagnostic: %q", got)
	}
}

func TestBoundedHookBufferAcceptsWithoutGrowingPastLimit(t *testing.T) {
	buffer := boundedBuffer{limit: 8}
	data := []byte("0123456789abcdef")
	written, err := buffer.Write(data)
	if err != nil || written != len(data) || buffer.String() != "01234567" || !buffer.truncated {
		t.Fatalf("bounded write = %d, %v, %q, truncated=%t", written, err, buffer.String(), buffer.truncated)
	}
	var output bytes.Buffer
	_, _ = output.Write(buffer.Bytes())
	if output.Len() != 8 {
		t.Fatalf("bounded output length = %d", output.Len())
	}
}

func TestHookCanRewritePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	extension := filepath.Join(root, "rewrite")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: rewrite\nhooks:\n  message.received: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\nread input\nprintf '%s\\n' '{\"payload\":{\"text\":\"rewritten\"}}'\n"
	if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Directory: root, Timeout: time.Second}).Run(context.Background(), "message.received", map[string]any{"text": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Payload["text"] != "rewritten" {
		t.Fatalf("hook payload = %#v", result.Payload)
	}
}

func TestDiscoveryRejectsUnsupportedHooks(t *testing.T) {
	root := t.TempDir()
	extension := filepath.Join(root, "stale")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: stale\nhooks:\n  update.before: [\"./hook.sh\"]\n"
	if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (Runner{Directory: root}).Run(context.Background(), "message.received", nil)
	if err == nil || !strings.Contains(err.Error(), `unsupported hook "update.before"`) {
		t.Fatalf("unsupported hook error = %v", err)
	}
}

func TestTrackedHookSkipsOnlyHooksWithDurableCompletionReceipts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		extension := filepath.Join(root, name)
		if err := os.MkdirAll(extension, 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := "name: " + name + "\nhooks:\n  task.completed: [\"./hook.sh\"]\n"
		script := "#!/bin/sh\nprintf x >> completed.count\nprintf '%s\\n' '{}'\n"
		if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	completed := map[string]bool{"first": true}
	var receipts []string
	_, err := (Runner{Directory: root, Timeout: time.Second}).RunTracked(context.Background(), "task.completed", map[string]any{"event_id": "stable"}, completed, func(id string) error {
		receipts = append(receipts, id)
		completed[id] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(receipts, ",") != "second" {
		t.Fatalf("completion receipts = %v", receipts)
	}
	if _, err := os.Stat(filepath.Join(root, "first", "completed.count")); !os.IsNotExist(err) {
		t.Fatalf("already-completed hook ran again: %v", err)
	}
	count, err := os.ReadFile(filepath.Join(root, "second", "completed.count"))
	if err != nil || string(count) != "x" {
		t.Fatalf("incomplete hook count = %q, %v", count, err)
	}
}

func TestTrackedHookRetriesWhenCompletionReceiptCannotPersist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	extension := filepath.Join(root, "retry")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: retry\nhooks:\n  task.completed: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\ninput=$(cat)\nprintf '%s\\n' \"$input\" >> events\nprintf '%s\\n' '{}'\n"
	if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	completed := map[string]bool{}
	runner := Runner{Directory: root, Timeout: time.Second}
	payload := map[string]any{"event_id": "stable-completion"}
	if _, err := runner.RunTracked(context.Background(), "task.completed", payload, completed, func(string) error {
		return os.ErrPermission
	}); err == nil {
		t.Fatal("missing completion-receipt error")
	}
	if _, err := runner.RunTracked(context.Background(), "task.completed", payload, completed, func(id string) error {
		completed[id] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(extension, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "stable-completion") != 2 {
		t.Fatalf("retried event payloads = %q", data)
	}
}
