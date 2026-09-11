package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Uninstall stops the installation after its startup owner has removed any
// restart registrations. Workspace data is never an installation-owned path.
func (m *Manager) Uninstall(ctx context.Context, removeStartup func() error) error {
	root := m.InstallRoot
	if root == "" {
		root = m.PackageRoot
	}
	if !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return errors.New("uninstall requires an absolute installation directory")
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("refusing to uninstall an installation-directory symlink or file")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved == root {
		return errors.New("refusing to uninstall the home directory")
	}
	if m.InstallRoot != "" {
		m.InstallRoot = root
	} else {
		m.PackageRoot = root
		modules := filepath.Dir(root)
		if filepath.Base(modules) != "node_modules" || filepath.Base(filepath.Dir(modules)) != "lib" {
			return errors.New("unsupported global npm installation layout")
		}
	}
	if m.InstallRoot != "" {
		marker, err := os.Lstat(filepath.Join(root, ".spynel-install"))
		if err != nil || !marker.Mode().IsRegular() || marker.Size() != int64(len(ownershipMarker)) || !ownedRoot(root) {
			return errors.New("refusing to uninstall an unmanaged directory")
		}
		unlock, err := lockInstall(root)
		if err != nil {
			return err
		}
		defer unlock()
	} else if !uninstallNPMRoot(root) {
		return errors.New("refusing to uninstall an unmanaged npm directory")
	}
	if err := removeStartup(); err != nil {
		return err
	}
	if err := m.stopProcesses(ctx, root); err != nil {
		return err
	}
	if m.InstallRoot == "" {
		// npm owns its package and launcher links. Pin the prefix rather than
		// allowing another npm configuration to select a different installation.
		modules := filepath.Dir(root)
		command := exec.CommandContext(ctx, "npm", "uninstall", "--global", "--prefix", filepath.Dir(filepath.Dir(modules)), "spynel")
		var output limitedOutput
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			return fmt.Errorf("npm uninstall: %w: %s", err, output.text)
		}
		return nil
	}
	paths, err := installationLauncherDirectories(root, home)
	if err != nil {
		return err
	}
	for _, directory := range paths {
		if !filepath.IsAbs(directory) {
			continue
		}
		path := filepath.Join(directory, "spynel")
		if installationLink(path, root) {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "releases" || strings.HasPrefix(name, ".stage-") || strings.HasPrefix(name, ".download-") {
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"current", "spynel", "env", ".bin-dir", ".spynel-install", ".install.lock"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Unrelated files may keep the directory nonempty.
	if err := os.Remove(root); err != nil && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

// A failed npm postinstall still leaves an npm-owned package to remove.
func uninstallNPMRoot(root string) bool {
	file, err := os.Open(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	var metadata packageMetadata
	return err == nil && len(data) <= 65536 && json.Unmarshal(data, &metadata) == nil && metadata.Name == "spynel"
}

func installationLink(path, root string) bool {
	target, err := os.Readlink(path)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	return err == nil && rootErr == nil && parent == resolvedRoot && filepath.Base(target) == "spynel"
}

func installationLauncherDirectories(root, home string) ([]string, error) {
	paths := append(filepath.SplitList(os.Getenv("PATH")), filepath.Join(home, ".local", "bin"), os.Getenv("SPYNEL_BIN_DIR"))
	if info, err := os.Lstat(filepath.Join(root, ".bin-dir")); err == nil && info.Mode().IsRegular() && info.Size() <= 4096 {
		data, err := os.ReadFile(filepath.Join(root, ".bin-dir"))
		if err != nil {
			return nil, err
		}
		paths = append(paths, strings.TrimSuffix(string(data), "\n"))
	}
	return paths, nil
}

// NeedsAdministrator checks the locations this installation owns before any
// cleanup starts, so the CLI can request one native sudo authorization.
func (m *Manager) NeedsAdministrator() bool {
	root := m.InstallRoot
	if root == "" {
		root = m.PackageRoot
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return false
	}
	if !installationWritable(root) || !installationWritable(filepath.Dir(root)) {
		return true
	}
	if m.InstallRoot == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	paths, err := installationLauncherDirectories(root, home)
	if err != nil {
		return true
	}
	for _, directory := range paths {
		if installationLink(filepath.Join(directory, "spynel"), root) && !installationWritable(directory) {
			return true
		}
	}
	return false
}

func (m *Manager) ownsProcessPath(root, path string) bool {
	path = strings.TrimSuffix(path, " (deleted)")
	if m.InstallRoot == "" {
		return npmRootFromExecutable(path) == root
	}
	relative, err := filepath.Rel(filepath.Join(root, "releases"), path)
	return err == nil && filepath.Base(relative) == "spynel" && len(strings.Split(relative, string(filepath.Separator))) == 2 && !strings.HasPrefix(relative, "..")
}

func (m *Manager) stopProcesses(ctx context.Context, root string) error {
	ids, err := installationProcessIDs()
	if err != nil {
		return err
	}
	var records []ProcessRegistration
	for _, pid := range ids {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		path, err := installationProcessPath(pid)
		if err == nil && m.ownsProcessPath(root, path) {
			records = append(records, ProcessRegistration{PID: pid, Executable: strings.TrimSuffix(path, " (deleted)")})
		}
	}
	return stopProcessRecords(ctx, records)
}
