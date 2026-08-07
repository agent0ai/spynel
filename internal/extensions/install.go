package extensions

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func Install(ctx context.Context, directory, repository, name string) (string, error) {
	if repository == "" {
		return "", errors.New("repository URL is required")
	}
	if name == "" {
		name = inferName(repository)
	}
	if !validName.MatchString(name) {
		return "", errors.New("extension name may contain only letters, numbers, dot, dash, and underscore")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(directory, name)
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("extension %s already exists", name)
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", repository, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(output)))
	}
	manifestPath := filepath.Join(target, ManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		_ = os.RemoveAll(target)
		return "", fmt.Errorf("repository does not contain %s", ManifestName)
	}
	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil || manifest.Name == "" {
		_ = os.RemoveAll(target)
		return "", fmt.Errorf("repository has an invalid %s", ManifestName)
	}
	return target, nil
}

func List(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := os.Stat(filepath.Join(directory, entry.Name(), ManifestName)); err == nil {
				names = append(names, entry.Name())
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func Remove(directory, name string) error {
	if !validName.MatchString(name) {
		return errors.New("invalid extension name")
	}
	target := filepath.Join(directory, name)
	rel, err := filepath.Rel(directory, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return errors.New("extension target escapes extension directory")
	}
	if _, err := os.Stat(filepath.Join(target, ManifestName)); err != nil {
		return fmt.Errorf("%s is not an installed extension", name)
	}
	return os.RemoveAll(target)
}

func inferName(repository string) string {
	parsed, err := url.Parse(repository)
	path := repository
	if err == nil && parsed.Path != "" {
		path = parsed.Path
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	name := filepath.Base(path)
	if validName.MatchString(name) {
		return name
	}
	return "extension"
}
