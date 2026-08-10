package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsCompleteHierarchy(t *testing.T) {
	root := t.TempDir()
	writeDOX(t, root, ".", "| [child/AGENTS.md](child/AGENTS.md) | Child. |")
	writeDOX(t, root, "child", "No child DOX files.")
	if problems := validate(root, []string{".", "child"}); len(problems) != 0 {
		t.Fatalf("validate() problems = %v", problems)
	}
}

func TestValidateReportsCoverageAndIndexDefects(t *testing.T) {
	root := t.TempDir()
	writeDOX(t, root, ".", "No child DOX files.\n| [ghost/AGENTS.md](ghost/AGENTS.md) | Ghost. |")
	problems := validate(root, []string{".", "child"})
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"child has no AGENTS.md", "falsely declares no child", "missing direct-child link child/AGENTS.md", "extra or broken direct-child link ghost/AGENTS.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q:\n%s", want, joined)
		}
	}
}

func TestValidateReportsMalformedAndDuplicateIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, agentsName), []byte("not an H1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDOX(t, root, "child", "No child DOX files.")
	problems := strings.Join(validate(root, []string{".", "child"}), "\n")
	for _, want := range []string{"must start with one H1", "missing ## Core Contract", "missing ## Child DOX Index"} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems missing %q:\n%s", want, problems)
		}
	}
	links, duplicates := agentsLinks("[x](child/AGENTS.md) [x](child/AGENTS.md)")
	if len(links) != 1 || len(duplicates) != 1 {
		t.Fatalf("agentsLinks() = %v, %v", links, duplicates)
	}
}

func TestValidateReportsCyclicIndex(t *testing.T) {
	root := t.TempDir()
	writeDOX(t, root, ".", "| [child/AGENTS.md](child/AGENTS.md) | Child. |")
	writeDOX(t, root, "child", "| [root](../AGENTS.md) | Invalid parent link. |")
	problems := strings.Join(validate(root, []string{".", "child"}), "\n")
	if !strings.Contains(problems, "cyclic child index") {
		t.Fatalf("problems missing cycle:\n%s", problems)
	}
}

func writeDOX(t *testing.T, root, dir, index string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# Test DOX\n\n## Purpose\n\n- Test.\n\n## Local Contracts\n\n- Test.\n\n## Child DOX Index\n\n" + index + "\n"
	if dir == "." {
		body = "# Root DOX\n\n## Core Contract\n\n- Test.\n\n## Child DOX Index\n\n" + index + "\n"
	}
	if err := os.WriteFile(filepath.Join(path, agentsName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
