package media

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	whisperRuntimeVersion       = "v1.9.2"
	whisperRuntimeBaseURL       = "https://github.com/ggml-org/whisper.cpp/releases/download/" + whisperRuntimeVersion + "/"
	maxWhisperRuntimeBytes      = int64(64 * 1024 * 1024)
	maxWhisperRuntimeFilesBytes = int64(128 * 1024 * 1024)
)

type whisperRuntimeAsset struct {
	Name   string
	SHA256 string
	Format string
}

var whisperRuntimeAssets = map[string]whisperRuntimeAsset{
	"linux/amd64":   {Name: "whisper-bin-ubuntu-x64.tar.gz", SHA256: "46811a3ecf584307480a220b9ef5ff81b7b22dc41577cbc274ce3afc61f753b1", Format: "tar.gz"},
	"linux/arm64":   {Name: "whisper-bin-ubuntu-arm64.tar.gz", SHA256: "7e26fa6a36d9174d5c0bf033ccbc026c3b5e569e2ee787058241346ef5392719", Format: "tar.gz"},
	"windows/386":   {Name: "whisper-bin-Win32.zip", SHA256: "de170719aebcb4794d695d449e179002db1fe03b862f21f5c34b2909a7cf8f22", Format: "zip"},
	"windows/amd64": {Name: "whisper-bin-x64.zip", SHA256: "49dcc16de826f20bd53d44f947a1ae49dfa81f86cad67a64d80820cb192d674a", Format: "zip"},
}

func (w *Whisper) command(ctx context.Context, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if path, err := w.lookPath(configured); err == nil {
		return path, nil
	} else if configured != "whisper-cli" {
		return "", fmt.Errorf("find Whisper executable %q: %w", configured, err)
	}
	asset, ok := w.runtimeAssets[w.runtimeGOOS+"/"+w.runtimeGOARCH]
	if !ok {
		return "", fmt.Errorf("find Whisper executable %q: not found; automatic whisper.cpp runtime provisioning is unavailable for %s/%s, install whisper-cli or set speech.command", configured, w.runtimeGOOS, w.runtimeGOARCH)
	}
	path, err := w.installRuntime(ctx, asset)
	if err != nil {
		return "", fmt.Errorf("provision Whisper runtime: %w", err)
	}
	return path, nil
}

func (w *Whisper) installRuntime(ctx context.Context, asset whisperRuntimeAsset) (string, error) {
	runtimeRoot := filepath.Join(w.modelDir, "runtime")
	versionRoot := filepath.Join(runtimeRoot, whisperRuntimeVersion)
	if path, err := findWhisperExecutable(versionRoot, w.runtimeGOOS); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return "", err
	}
	w.logf("Downloading official whisper.cpp runtime %s for %s/%s (first use)", whisperRuntimeVersion, w.runtimeGOOS, w.runtimeGOARCH)
	archivePath, cleanupArchive, err := w.downloadRuntime(ctx, runtimeRoot, asset)
	if err != nil {
		return "", err
	}
	defer cleanupArchive()
	temporaryRoot, err := os.MkdirTemp(runtimeRoot, ".whisper-runtime-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporaryRoot)
	if err := extractWhisperRuntime(ctx, archivePath, asset.Format, temporaryRoot); err != nil {
		return "", err
	}
	if _, err := findWhisperExecutable(temporaryRoot, w.runtimeGOOS); err != nil {
		return "", err
	}
	if path, err := findWhisperExecutable(versionRoot, w.runtimeGOOS); err == nil {
		return path, nil
	}
	if _, err := os.Stat(versionRoot); err == nil {
		return "", fmt.Errorf("managed runtime path %q already exists but is incomplete", versionRoot)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(temporaryRoot, versionRoot); err != nil {
		return "", err
	}
	path, err := findWhisperExecutable(versionRoot, w.runtimeGOOS)
	if err != nil {
		return "", err
	}
	w.logf("Whisper runtime ready: %s", path)
	return path, nil
}

func (w *Whisper) downloadRuntime(ctx context.Context, directory string, asset whisperRuntimeAsset) (string, func(), error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, w.runtimeBaseURL+asset.Name, nil)
	if err != nil {
		return "", func() {}, err
	}
	response, err := w.client.Do(request)
	if err != nil {
		return "", func() {}, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", func() {}, fmt.Errorf("download %s: HTTP %d", asset.Name, response.StatusCode)
	}
	if response.ContentLength > maxWhisperRuntimeBytes {
		return "", func() {}, errors.New("Whisper runtime archive exceeds the 64 MB safety limit")
	}
	temporary, err := os.CreateTemp(directory, ".whisper-runtime-archive-*")
	if err != nil {
		return "", func() {}, err
	}
	path := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(path)
	}
	hash := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(temporary, hash), io.LimitReader(response.Body, maxWhisperRuntimeBytes+1))
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if written > maxWhisperRuntimeBytes {
		cleanup()
		return "", func() {}, errors.New("Whisper runtime archive exceeds the 64 MB safety limit")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), asset.SHA256) {
		cleanup()
		return "", func() {}, errors.New("Whisper runtime archive checksum mismatch")
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

func extractWhisperRuntime(ctx context.Context, archivePath, format, destination string) error {
	switch format {
	case "tar.gz":
		return extractWhisperTarGz(ctx, archivePath, destination)
	case "zip":
		return extractWhisperZip(ctx, archivePath, destination)
	default:
		return fmt.Errorf("unsupported Whisper runtime archive format %q", format)
	}
}

func extractWhisperTarGz(ctx context.Context, archivePath, destination string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	var expanded int64
	var links []tar.Header
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := safeRuntimePath(destination, header.Name)
		if err != nil {
			return err
		}
		if !whisperRuntimeEntry(header.Name) {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			expanded += header.Size
			if expanded > maxWhisperRuntimeFilesBytes {
				return errors.New("expanded Whisper runtime exceeds the 128 MB safety limit")
			}
			if err := writeRuntimeFile(ctx, path, reader, header.Size, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			copy := *header
			links = append(links, copy)
		}
	}
	for _, header := range links {
		path, err := safeRuntimePath(destination, header.Name)
		if err != nil {
			return err
		}
		if filepath.IsAbs(filepath.FromSlash(header.Linkname)) {
			return fmt.Errorf("unsafe Whisper runtime link %q", header.Name)
		}
		target := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(header.Linkname)))
		if _, err := safeRuntimePath(destination, strings.TrimPrefix(target, destination+string(filepath.Separator))); err != nil {
			return fmt.Errorf("unsafe Whisper runtime link %q: %w", header.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.Symlink(filepath.FromSlash(header.Linkname), path); err != nil {
			return err
		}
	}
	return nil
}

func extractWhisperZip(ctx context.Context, archivePath, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	var expanded int64
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := safeRuntimePath(destination, entry.Name)
		if err != nil {
			return err
		}
		if !whisperRuntimeEntry(entry.Name) {
			continue
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			continue
		}
		expanded += int64(entry.UncompressedSize64)
		if expanded > maxWhisperRuntimeFilesBytes {
			return errors.New("expanded Whisper runtime exceeds the 128 MB safety limit")
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeRuntimeFile(ctx, path, source, int64(entry.UncompressedSize64), entry.Mode())
		closeErr := source.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func whisperRuntimeEntry(name string) bool {
	base := strings.ToLower(filepath.Base(filepath.FromSlash(name)))
	return base == "license" || base == "whisper-cli" || base == "whisper-cli.exe" ||
		strings.HasSuffix(base, ".dll") || strings.HasPrefix(base, "libwhisper.so") || strings.HasPrefix(base, "libggml")
}

func safeRuntimePath(root, name string) (string, error) {
	name = filepath.Clean(filepath.FromSlash(name))
	if name == "." || filepath.IsAbs(name) {
		return "", fmt.Errorf("unsafe Whisper runtime archive path %q", name)
	}
	path := filepath.Join(root, name)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe Whisper runtime archive path %q", name)
	}
	return path, nil
}

func writeRuntimeFile(ctx context.Context, path string, source io.Reader, size int64, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := copyContext(ctx, file, io.LimitReader(source, size))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != size {
		return io.ErrUnexpectedEOF
	}
	permissions := mode.Perm()
	if permissions == 0 {
		permissions = 0o600
	}
	return os.Chmod(path, permissions)
}

func findWhisperExecutable(root, goos string) (string, error) {
	name := "whisper-cli"
	if goos == "windows" {
		name += ".exe"
	}
	var result string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				result = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if result == "" {
		return "", errors.New("downloaded Whisper runtime does not contain whisper-cli")
	}
	return result, nil
}
