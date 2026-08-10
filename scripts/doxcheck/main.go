// Command doxcheck validates the repository's AGENTS.md ownership hierarchy.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const agentsName = "AGENTS.md"

func main() {
	root, err := repositoryRoot()
	if err != nil {
		fail(err)
	}
	dirs, err := trackedDirectories(root)
	if err != nil {
		fail(err)
	}
	if problems := validate(root, dirs); len(problems) != 0 {
		for _, problem := range problems {
			fmt.Fprintln(os.Stderr, "doxcheck:", problem)
		}
		os.Exit(1)
	}
	fmt.Printf("DOX coverage valid for %d tracked directories\n", len(dirs))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "doxcheck:", err)
	os.Exit(1)
}

func repositoryRoot() (string, error) {
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// trackedDirectories deliberately has no coverage exceptions. Every directory
// containing a tracked or not-yet-added, non-ignored file must own AGENTS.md.
// Git's ignore policy excludes build output, caches, private workspace state,
// and other disposable artifacts before this policy is applied.
func trackedDirectories(root string) ([]string, error) {
	command := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	command.Dir = root
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list repository files: %w", err)
	}
	deletedCommand := exec.Command("git", "ls-files", "--deleted", "-z")
	deletedCommand.Dir = root
	deleted, err := deletedCommand.Output()
	if err != nil {
		return nil, fmt.Errorf("list deleted repository files: %w", err)
	}
	return trackedDirectoriesFromGit(out, deleted), nil
}

func trackedDirectoriesFromGit(tracked, deleted []byte) []string {
	deletedPaths := make(map[string]bool)
	for _, raw := range bytes.Split(deleted, []byte{0}) {
		if len(raw) > 0 {
			deletedPaths[filepath.ToSlash(string(raw))] = true
		}
	}
	seen := map[string]bool{".": true}
	for _, raw := range bytes.Split(tracked, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		path := filepath.ToSlash(string(raw))
		if deletedPaths[path] {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		for dir != "." {
			seen[dir] = true
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func validate(root string, dirs []string) []string {
	problems := []string{}
	dirSet := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		dirSet[dir] = true
	}
	indexed := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		path := filepath.Join(root, filepath.FromSlash(dir), agentsName)
		body, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				problems = append(problems, fmt.Sprintf("%s has no AGENTS.md", dir))
			} else {
				problems = append(problems, fmt.Sprintf("read %s: %v", relative(root, path), err))
			}
			continue
		}
		text := string(body)
		if !strings.HasPrefix(text, "# ") || countLevelOneHeadings(text) != 1 {
			problems = append(problems, fmt.Sprintf("%s must start with one H1", relative(root, path)))
		}
		if dir == "." {
			for _, heading := range []string{"## Core Contract", "## Child DOX Index"} {
				if countHeading(text, heading) != 1 {
					problems = append(problems, fmt.Sprintf("%s is missing %s", relative(root, path), heading))
				}
			}
		} else {
			for _, heading := range []string{"## Purpose", "## Local Contracts", "## Child DOX Index"} {
				if countHeading(text, heading) != 1 {
					problems = append(problems, fmt.Sprintf("%s is missing %s", relative(root, path), heading))
				}
			}
		}
		section, ok := headingSection(text, "## Child DOX Index")
		if !ok {
			continue
		}
		links, duplicates := agentsLinks(section)
		for _, duplicate := range duplicates {
			problems = append(problems, fmt.Sprintf("%s indexes %s more than once", relative(root, path), duplicate))
		}
		indexed[dir] = links
		expected := directChildDocs(dir, dirs)
		if len(expected) > 0 && strings.Contains(section, "No child DOX files.") {
			problems = append(problems, fmt.Sprintf("%s falsely declares no child DOX files", relative(root, path)))
		}
		if len(expected) == 0 && !strings.Contains(section, "No child DOX files.") {
			problems = append(problems, fmt.Sprintf("%s must explicitly declare No child DOX files.", relative(root, path)))
		}
		for _, missing := range difference(expected, links) {
			problems = append(problems, fmt.Sprintf("%s is missing direct-child link %s", relative(root, path), missing))
		}
		for _, extra := range difference(links, expected) {
			problems = append(problems, fmt.Sprintf("%s has extra or broken direct-child link %s", relative(root, path), extra))
		}
	}
	for _, cycle := range indexCycles(indexed, dirSet) {
		problems = append(problems, "cyclic child index: "+strings.Join(cycle, " -> "))
	}
	sort.Strings(problems)
	return problems
}

func countHeading(text, heading string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if line == heading {
			count++
		}
	}
	return count
}

func countLevelOneHeadings(text string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "# ") {
			count++
		}
	}
	return count
}

func headingSection(text, heading string) (string, bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if line == heading {
			start = i + 1
			continue
		}
		if start >= 0 && strings.HasPrefix(line, "## ") {
			return strings.Join(lines[start:i], "\n"), true
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n"), true
	}
	return "", false
}

func agentsLinks(section string) ([]string, []string) {
	var links, duplicates []string
	seen := map[string]bool{}
	for rest := section; ; {
		end := strings.Index(rest, "/AGENTS.md)")
		if end < 0 {
			break
		}
		start := strings.LastIndex(rest[:end], "](")
		if start < 0 {
			rest = rest[end+len("/AGENTS.md)"):]
			continue
		}
		link := rest[start+2 : end+len("/AGENTS.md")]
		link = filepath.ToSlash(filepath.Clean(filepath.FromSlash(link)))
		if seen[link] {
			duplicates = append(duplicates, link)
		} else {
			seen[link] = true
			links = append(links, link)
		}
		rest = rest[end+len("/AGENTS.md)"):]
	}
	sort.Strings(links)
	return links, duplicates
}

func directChildDocs(parent string, dirs []string) []string {
	var children []string
	for _, dir := range dirs {
		if dir == "." || filepath.ToSlash(filepath.Dir(dir)) != parent {
			continue
		}
		name := filepath.Base(filepath.FromSlash(dir)) + "/" + agentsName
		children = append(children, filepath.ToSlash(name))
	}
	sort.Strings(children)
	return children
}

func difference(left, right []string) []string {
	have := map[string]bool{}
	for _, value := range right {
		have[value] = true
	}
	var result []string
	for _, value := range left {
		if !have[value] {
			result = append(result, value)
		}
	}
	return result
}

func indexCycles(indexed map[string][]string, dirs map[string]bool) [][]string {
	state := map[string]int{}
	var cycles [][]string
	var visit func(string, []string)
	visit = func(dir string, stack []string) {
		if state[dir] == 1 {
			for i, item := range stack {
				if item == dir {
					cycles = append(cycles, append(append([]string{}, stack[i:]...), dir))
					break
				}
			}
			return
		}
		if state[dir] == 2 {
			return
		}
		state[dir] = 1
		for _, link := range indexed[dir] {
			child := filepath.ToSlash(filepath.Clean(filepath.Join(dir, filepath.Dir(link))))
			if dirs[child] {
				visit(child, append(stack, dir))
			}
		}
		state[dir] = 2
	}
	for dir := range dirs {
		visit(dir, nil)
	}
	return cycles
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
