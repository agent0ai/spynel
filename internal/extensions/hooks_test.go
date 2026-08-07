package extensions

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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

func TestHarnessHookFallsBackToLegacyRecipientManifestKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	extension := filepath.Join(root, "legacy")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: legacy\nhooks:\n  recipient.before: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\nread input\nprintf '%s\\n' '{\"payload\":{\"prompt\":\"legacy hook ran\"}}'\n"
	if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Directory: root, Timeout: time.Second}).Run(context.Background(), "harness.before", map[string]any{"prompt": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Payload["prompt"] != "legacy hook ran" {
		t.Fatalf("legacy hook payload = %#v", result.Payload)
	}
}
