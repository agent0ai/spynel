package media

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cpuVendorWarning = "onnxruntime cpuid_info warning: Unknown CPU vendor. cpuinfo_vendor value: 0\n"

func TestNativeStartupProbe(t *testing.T) {
	if os.Getenv("SPYNEL_NATIVE_STARTUP_PROBE") == "" {
		t.Skip("subprocess only")
	}
	if r, err := newSherpaRecognizer(parakeetFiles{}, 1); err == nil {
		r.Close()
		t.Fatal("invalid native recognizer was accepted")
	}
	_, _ = os.Stderr.WriteString("ordinary stderr remains visible\n")
}

func TestNativeStartupQuietAndRecognizerErrorsVisible(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeStartupProbe$")
	cmd.Env = append(os.Environ(), "SPYNEL_NATIVE_STARTUP_PROBE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native startup: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte(cpuVendorWarning)) || !bytes.Contains(out, []byte("ordinary stderr remains visible")) || !bytes.Contains(out, []byte("Errors in config")) {
		t.Fatalf("native startup/error diagnostics: %s", out)
	}
}

func TestNativeStartupFilterMatchesOnlyExactDiagnostic(t *testing.T) {
	dir := t.TempDir()
	// A real shared-library constructor runs before the executable's reset.
	// Keep this separate from Go initialization, where the defect cannot occur.
	dependency := `#include <stdio.h>
#include <string.h>
static void put(FILE *f, const char *s) { fwrite(s, 1, strlen(s), f); }
static void diagnostic(FILE *f, const char *s) {
 put(f, "onnxruntime cpuid_info warning: "); put(f, s); put(f, "\n");
}
void probe(void) { diagnostic(stderr, "Unknown CPU vendor. cpuinfo_vendor value: 0"); }
__attribute__((constructor)) static void start(void) {
 probe(); probe();
 diagnostic(stdout, "Unknown CPU vendor. cpuinfo_vendor value: 0");
 diagnostic(stderr, "Failed to initialize cpuinfo");
 diagnostic(stderr, "Unknown CPU vendor. cpuinfo_vendor value: 15");
 put(stderr, "onnxruntime cpuid_info warning: ");
 put(stderr, "Unknown CPU vendor. cpuinfo_vendor value: 0");
 put(stderr, " changed\n");
 put(stderr, "onnxruntime cpuid_info warning: ");
}
`
	for name, source := range map[string]string{"dependency.c": dependency, "main.c": "extern void probe(void); int main(void) { probe(); return 0; }"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"-shared", "-fPIC", "-fno-builtin-fwrite", "-o", filepath.Join(dir, "libprobe.so"), filepath.Join(dir, "dependency.c")},
		{"-o", filepath.Join(dir, "probe"), filepath.Join(dir, "main.c"), "native_startup_linux_arm64.c", "-L" + dir, "-lprobe", "-Wl,-rpath," + dir},
	} {
		if out, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
			t.Fatalf("compile native fixture: %v\n%s", err, out)
		}
	}
	cmd := exec.Command(filepath.Join(dir, "probe"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	prefix := "onnxruntime cpuid_info warning: "
	want := prefix + "Failed to initialize cpuinfo\n" + prefix + "Unknown CPU vendor. cpuinfo_vendor value: 15\n" + strings.TrimSuffix(cpuVendorWarning, "\n") + " changed\n" + prefix + cpuVendorWarning
	if stdout.String() != cpuVendorWarning || stderr.String() != want {
		t.Fatalf("stdout=%q stderr=%q want=%q", stdout.String(), stderr.String(), want)
	}
}
