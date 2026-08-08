package extensions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

const maxInstallOutput = 64 << 10

type installOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *installOutput) Write(data []byte) (int, error) {
	length := len(data)
	remaining := maxInstallOutput - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(data[:min(length, remaining)])
	}
	if length > remaining {
		b.truncated = true
	}
	return length, nil
}

func Install(ctx context.Context, directory, repository, name string, logWriters ...io.Writer) (string, error) {
	return install(ctx, directory, repository, name, exec.CommandContext, logWriters...)
}

func install(ctx context.Context, directory, repository, name string, commandContext func(context.Context, string, ...string) *exec.Cmd, logWriters ...io.Writer) (string, error) {
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
	cmd := commandContext(ctx, "git", "clone", "--depth", "1", "--", repository, target)
	stdout := &installOutput{}
	stderr := &installOutput{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	var logWriter io.Writer
	if len(logWriters) > 0 {
		logWriter = logWriters[0]
	}
	writeInstallOutput(logWriter, "stdout", stdout)
	writeInstallOutput(logWriter, "stderr", stderr)
	if err != nil {
		if logWriter != nil {
			_, _ = fmt.Fprintf(logWriter, "process=git operation=clone event=exit status=failed exit_code=%d error=%v\n", installExitCode(cmd), err)
		}
		return "", fmt.Errorf("git clone: %w", err)
	}
	if logWriter != nil {
		_, _ = fmt.Fprintln(logWriter, "process=git operation=clone event=exit status=success exit_code=0")
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

func installExitCode(command *exec.Cmd) int {
	if command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}

func writeInstallOutput(logWriter io.Writer, stream string, output *installOutput) {
	if logWriter == nil || output.Len() == 0 {
		return
	}
	_, _ = fmt.Fprintf(logWriter, "process=git operation=clone stream=%s truncated=%t output=%s\n", stream, output.truncated, strings.TrimSpace(output.String()))
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
