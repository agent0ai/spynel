package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

const ownershipMarker = "spynel-github-v1\n"

type bundleMetadata struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func readBundle(directory string) (bundleMetadata, error) {
	var marker bundleMetadata
	data, err := os.ReadFile(filepath.Join(directory, ".bundle.json"))
	if err != nil {
		return marker, err
	}
	if len(data) > 1024 {
		return marker, errors.New("invalid bundle marker")
	}
	err = json.Unmarshal(data, &marker)
	if err == nil && (!stableVersion(marker.Version) || marker.OS != runtime.GOOS || marker.Arch != runtime.GOARCH) {
		err = errors.New("invalid bundle identity")
	}
	return marker, err
}

func ownedRoot(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, ".spynel-install"))
	return err == nil && string(data) == ownershipMarker
}

func scriptRootFromExecutable(executable, version string) string {
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil || filepath.Base(executable) != "spynel" {
		return ""
	}
	bundle := filepath.Dir(executable)
	releases := filepath.Dir(bundle)
	root := filepath.Dir(releases)
	if filepath.Base(releases) != "releases" || !ownedRoot(root) {
		return ""
	}
	marker, err := readBundle(bundle)
	if err != nil || (version != "" && marker.Version != strings.TrimPrefix(version, "v")) {
		return ""
	}
	return root
}

// RestartExecutable follows the installation's stable entry point, rather than
// os.Executable's resolved path to an older retained bundle. Unmanaged binaries
// retain their ordinary restart behavior.
func RestartExecutable(executable string) string {
	if processNPMRoot != "" && processMatches(ProcessRegistration{Executable: processExecutable, Installation: processNPMRoot}, executable) {
		return filepath.Join(processNPMRoot, "npm", "vendor", "spynel")
	}
	if root := scriptRootFromExecutable(executable, ""); root != "" {
		return filepath.Join(root, "spynel")
	}
	return executable
}

func privateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("installation directory must be a real directory writable only by its owner: %s", directory)
	}
	return nil
}

// InstallArchive is also the verified bootstrap binary's installation boundary.
// It never adopts a nonempty unmanaged directory, replaces an unrelated entry
// point, or mutates a previously published bundle.
func InstallArchive(ctx context.Context, root, archive, checksums, version string) (string, error) {
	version = strings.TrimPrefix(version, "v")
	if !stableVersion(version) {
		return "", errors.New("installation requires a stable semantic version")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", errors.New("standalone installation supports only Linux and macOS")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "", errors.New("unsupported standalone architecture")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := privateDirectory(root); err != nil {
		return "", err
	}
	unlock, err := lockInstall(root)
	if err != nil {
		return "", err
	}
	defer unlock()
	if !ownedRoot(root) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if entry.Name() != ".install.lock" {
				return "", errors.New("refusing to adopt a nonempty unmanaged installation directory")
			}
		}
		if err := fsx.AtomicCreateFile(filepath.Join(root, ".spynel-install"), []byte(ownershipMarker), 0600); err != nil {
			return "", err
		}
	}
	if err := privateDirectory(filepath.Join(root, "releases")); err != nil {
		return "", err
	}
	launcher := filepath.Join(root, "spynel")
	if target, err := os.Readlink(launcher); err == nil {
		if target != "current/spynel" {
			return "", errors.New("refusing to replace an unrelated launcher")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("refusing to replace an unrelated executable")
	}
	current := filepath.Join(root, "current")
	if target, err := os.Readlink(current); err == nil {
		if filepath.Dir(target) != "releases" {
			return "", errors.New("invalid current bundle link")
		}
		marker, err := readBundle(filepath.Join(root, target))
		if err != nil {
			return "", err
		}
		if compareVersions(marker.Version, version) > 0 {
			return "", errors.New("refusing to downgrade the current installation")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("current bundle must be a symbolic link")
	}
	file, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := verifyChecksum(file, checksums, archiveName(version)); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := extractBundle(ctx, file, stage); err != nil {
		return "", err
	}
	if err := verifyBundle(ctx, stage, version); err != nil {
		return "", err
	}
	marker, _ := json.Marshal(bundleMetadata{Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err := fsx.AtomicCreateFile(filepath.Join(stage, ".bundle.json"), append(marker, '\n'), 0600); err != nil {
		return "", err
	}
	// ponytail: retain old bundles for running processes; prune only with all
	// installations' processes stopped, until measured disk use warrants tracking.
	destination := filepath.Join(root, "releases", version+"-"+strings.TrimPrefix(filepath.Base(stage), ".stage-"))
	if err := os.Rename(stage, destination); err != nil {
		return "", err
	}
	if err := os.Symlink("current/spynel", launcher); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	link := filepath.Join(root, ".current-"+filepath.Base(destination))
	defer os.Remove(link)
	if err := os.Symlink(filepath.Join("releases", filepath.Base(destination)), link); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(link, current); err != nil {
		return "", err
	}
	return launcher, nil
}

func verifyChecksum(file *os.File, checksumPath, name string) error {
	checksums, err := os.Open(checksumPath)
	if err != nil {
		return err
	}
	defer checksums.Close()
	data, err := io.ReadAll(io.LimitReader(checksums, maxChecksumBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxChecksumBytes {
		return errors.New("checksum file exceeds byte limit")
	}
	expected := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		if expected != "" {
			return errors.New("duplicate release checksum")
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("invalid release checksum")
		}
		expected = strings.ToLower(fields[0])
	}
	if expected == "" {
		return errors.New("missing release checksum")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if n > maxArchiveBytes {
		return errors.New("archive exceeds byte limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("release checksum mismatch")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func extractBundle(ctx context.Context, input io.Reader, destination string) error {
	compressed, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer compressed.Close()
	const expandedLimit = 2 << 30
	bounded := &io.LimitedReader{R: compressed, N: expandedLimit + 1}
	archive := tar.NewReader(bounded)
	seen := make(map[string]bool)
	for entries := 0; ; entries++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if entries >= 4096 {
			return errors.New("archive contains too many entries")
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if (header.Name == "." || header.Name == "./") && header.Typeflag == tar.TypeDir {
			continue
		}
		if name == "" || len(name) > 1024 || name != path.Clean(name) || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\:") {
			return errors.New("archive contains an unsafe path")
		}
		for _, r := range name {
			if r < 32 || r == 127 {
				return errors.New("archive path contains controls")
			}
		}
		if seen[name] {
			return errors.New("archive contains duplicate paths")
		}
		seen[name] = true
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > expandedLimit || bounded.N <= header.Size {
				return errors.New("archive exceeds expanded byte limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, archive, header.Size)
			syncErr := file.Sync()
			closeErr := file.Close()
			if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
				return err
			}
		default:
			return errors.New("archive contains a link or special file")
		}
	}
	// Consume the gzip trailer too: tar EOF alone does not verify compression CRC
	// or detect a truncated download after the tar end marker.
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return err
	}
	if bounded.N <= 0 {
		return errors.New("archive exceeds expanded byte limit")
	}
	return nil
}

func verifyBundle(ctx context.Context, directory, version string) error {
	required := []string{"spynel", "LICENSE", "THIRD_PARTY_NOTICES.md", "licenses/sherpa-onnx/LICENSE", "licenses/onnxruntime/LICENSE", "licenses/miniaudio/LICENSE", "licenses/pion-opus/LICENSE", "licenses/bubbletea/LICENSE", "licenses/bubbles-textarea/LICENSE"}
	if runtime.GOOS == "darwin" {
		required = append(required, "lib/libsherpa-onnx-c-api.dylib", "lib/libonnxruntime.1.27.0.dylib")
	} else {
		required = append(required, "lib/libsherpa-onnx-c-api.so", "lib/libonnxruntime.so")
	}
	for _, name := range required {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("release bundle is missing required file %s", name)
		}
	}
	binary := filepath.Join(directory, "spynel")
	if err := os.Chmod(binary, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--version")
	// A bounded writer keeps a faulty candidate from consuming unbounded memory.
	var output limitedOutput
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("candidate executable check failed: %w", err)
	}
	if strings.TrimSpace(output.text) != "spynel "+version {
		return errors.New("candidate executable version does not match release")
	}
	return nil
}

type limitedOutput struct{ text string }

func (b *limitedOutput) Write(data []byte) (int, error) {
	if len(b.text)+len(data) > 1024 {
		return 0, errors.New("candidate output exceeds limit")
	}
	b.text += string(data)
	return len(data), nil
}
