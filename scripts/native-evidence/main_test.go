package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceSerializationIsBoundedAndPathMinimal(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "private workspace", "archive.tar.gz")
	secret := "sk-private-credential"
	record := evidence{
		SchemaVersion: 1, RecordedAt: "2026-08-07T00:00:00Z",
		EvidenceClassification: classification, SyntheticFixturesOnly: true,
		SourceRef: "v1.2.3", SourceCommit: strings.Repeat("a", 40), OS: "linux",
		Architecture: "arm64", GoVersion: "go version go1.26.5 linux/arm64",
		Archive: archiveEvidence{Name: filepath.Base(privatePath), SHA256: strings.Repeat("b", 64)},
		Results: []result{{Name: "packaged-help", Command: "spynel --help", Status: "pass", Assertion: "help launches"}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= maxEvidenceSize {
		t.Fatalf("evidence size = %d", len(data))
	}
	serialized := string(data)
	for _, forbidden := range []string{filepath.Dir(privatePath), secret, "environment", "home_directory"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("serialized evidence exposed %q: %s", forbidden, serialized)
		}
	}
	if !strings.Contains(serialized, filepath.Base(privatePath)) || !strings.Contains(serialized, classification) {
		t.Fatalf("serialized evidence omitted safe identity fields: %s", serialized)
	}
}

func TestSmokeEnvironmentUsesAnAllowlist(t *testing.T) {
	t.Setenv("SPYNEL_TEST_SECRET", "do-not-copy")
	t.Setenv("OPENAI_API_KEY", "do-not-copy-either")
	environment := strings.Join(smokeEnvironment(filepath.Join(t.TempDir(), "home"), t.TempDir()), "\n")
	if strings.Contains(environment, "SPYNEL_TEST_SECRET") || strings.Contains(environment, "OPENAI_API_KEY") || strings.Contains(environment, "do-not-copy") {
		t.Fatalf("smoke environment copied a credential: %s", environment)
	}
	if !strings.Contains(environment, "HOME=") || !strings.Contains(environment, "PATH=") {
		t.Fatalf("smoke environment omitted isolation variables: %s", environment)
	}
}

func TestSafeDestinationRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../secret", "nested/../../secret"} {
		if _, err := safeDestination(root, name); err == nil {
			t.Fatalf("safeDestination(%q) accepted traversal", name)
		}
	}
	if target, err := safeDestination(root, "folder/file"); err != nil || target != filepath.Join(root, "folder", "file") {
		t.Fatalf("safe destination = %q, %v", target, err)
	}
}

func TestContainsOutputLineToleratesNativeRuntimeDiagnostics(t *testing.T) {
	output := "native runtime warning\r\nspynel 1.2.3\r\n"
	if !containsOutputLine(output, "spynel 1.2.3") {
		t.Fatal("version line was not found after a native runtime diagnostic")
	}
	if containsOutputLine(output, "spynel 9.9.9") {
		t.Fatal("wrong version line matched")
	}
}

func TestSafeIdentifierRejectsPathsAndUnboundedValues(t *testing.T) {
	for _, value := range []string{"v1.2.3", "local-working-tree", "release+meta"} {
		if !safeIdentifier(value) {
			t.Fatalf("safeIdentifier(%q) rejected a release reference", value)
		}
	}
	for _, value := range []string{"", "../private", "tag with spaces", strings.Repeat("a", 129)} {
		if safeIdentifier(value) {
			t.Fatalf("safeIdentifier(%q) accepted private or unbounded data", value)
		}
	}
}

func TestWriteEvidenceUsesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "evidence.json")
	record := evidence{SchemaVersion: 1, EvidenceClassification: classification}
	if err := writeEvidence(path, record); err != nil {
		t.Fatal(err)
	}
	record.Results = append(record.Results, result{Name: "second-write", Status: "pass"})
	if err := writeEvidence(path, record); err != nil {
		t.Fatalf("replace evidence: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o", info.Mode().Perm())
	}
}
