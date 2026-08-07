package media

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/agent0ai/spynel/internal/config"
)

const (
	parakeetModelBaseURL     = "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/"
	maxParakeetExpandedBytes = int64(1024 * 1024 * 1024)
)

var requiredParakeetFiles = []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"}

type parakeetModel struct {
	ID           string
	Archive      string
	SHA256       string
	ArchiveBytes int64
	Notice       string
}

type parakeetFiles struct {
	Directory string
	Encoder   string
	Decoder   string
	Joiner    string
	Tokens    string
}

func defaultParakeetModels() map[string]parakeetModel {
	return map[string]parakeetModel{
		parakeetEnglishModel: {
			ID:           "sherpa-onnx-nemo-parakeet-unified-en-0.6b-int8-non-streaming",
			Archive:      "sherpa-onnx-nemo-parakeet-unified-en-0.6b-int8-non-streaming.tar.bz2",
			SHA256:       "99f63605b3a85a54c250c0869670a687b7d6598a47bf2421515e1f839a76e150",
			ArchiveBytes: 501350460,
			Notice:       "NVIDIA Parakeet Unified EN 0.6B\nGoverning terms: NVIDIA Open Model License Agreement\nhttps://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-license/\n\nDownloaded as the k2-fsa sherpa-onnx INT8 ONNX conversion.\n",
		},
		parakeetMultiModel: {
			ID:           "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8",
			Archive:      "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8.tar.bz2",
			SHA256:       "5793d0fd397c5778d2cf2126994d58e9d56b1be7c04d13c7a15bb1b4eafb16bf",
			ArchiveBytes: 487170055,
			Notice:       "NVIDIA Parakeet TDT 0.6B v3\nCopyright NVIDIA Corporation\nLicensed under CC BY 4.0: https://creativecommons.org/licenses/by/4.0/\n\nDownloaded as the k2-fsa converted and INT8-quantized ONNX form; Spynel does not modify the weights.\n",
		},
	}
}

func modelKind(language string) string {
	if strings.EqualFold(strings.TrimSpace(language), "en") {
		return parakeetEnglishModel
	}
	return parakeetMultiModel
}

func (p *Parakeet) model(ctx context.Context, cfg config.Speech) (parakeetFiles, error) {
	if configured := strings.TrimSpace(cfg.ModelDir); configured != "" {
		return inspectParakeetModel(resolveModelDirectory(p.settings, configured))
	}
	spec, ok := p.models[modelKind(cfg.Language)]
	if !ok {
		return parakeetFiles{}, fmt.Errorf("no Parakeet model configured for language %q", cfg.Language)
	}
	if err := os.MkdirAll(p.modelDir, 0o700); err != nil {
		return parakeetFiles{}, err
	}
	destination := filepath.Join(p.modelDir, spec.ID)
	if _, statErr := os.Stat(destination); statErr == nil {
		files, inspectErr := inspectParakeetModel(destination)
		if inspectErr != nil {
			return parakeetFiles{}, fmt.Errorf("managed Parakeet model is incomplete: %w", inspectErr)
		}
		return files, nil
	} else if !os.IsNotExist(statErr) {
		return parakeetFiles{}, fmt.Errorf("inspect managed Parakeet model: %w", statErr)
	}

	p.logf("Downloading Parakeet model %s (first use)", spec.ID)
	archive, cleanup, err := p.downloadModel(ctx, spec)
	if err != nil {
		return parakeetFiles{}, err
	}
	defer cleanup()
	temporary, err := os.MkdirTemp(p.modelDir, "."+spec.ID+"-*")
	if err != nil {
		return parakeetFiles{}, err
	}
	defer os.RemoveAll(temporary)
	if err := extractParakeetModel(ctx, archive, temporary); err != nil {
		return parakeetFiles{}, fmt.Errorf("extract Parakeet model: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "MODEL_NOTICE.txt"), []byte(spec.Notice), 0o600); err != nil {
		return parakeetFiles{}, fmt.Errorf("write Parakeet model notice: %w", err)
	}
	files, err := inspectParakeetModel(temporary)
	if err != nil {
		return parakeetFiles{}, fmt.Errorf("verify extracted Parakeet model: %w", err)
	}
	if existing, existingErr := inspectParakeetModel(destination); existingErr == nil {
		return existing, nil
	}
	if err := os.Rename(temporary, destination); err != nil {
		if existing, existingErr := inspectParakeetModel(destination); existingErr == nil {
			return existing, nil
		}
		return parakeetFiles{}, fmt.Errorf("install Parakeet model: %w", err)
	}
	files, err = inspectParakeetModel(destination)
	if err != nil {
		return parakeetFiles{}, err
	}
	p.logf("Parakeet model ready: %s", destination)
	return files, nil
}

func inspectParakeetModel(directory string) (parakeetFiles, error) {
	paths := make(map[string]string, len(requiredParakeetFiles))
	for _, name := range requiredParakeetFiles {
		file := filepath.Join(directory, name)
		info, err := os.Stat(file)
		if err != nil {
			return parakeetFiles{}, err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return parakeetFiles{}, fmt.Errorf("required model file %q is missing or empty", file)
		}
		paths[name] = file
	}
	return parakeetFiles{
		Directory: directory,
		Encoder:   paths["encoder.int8.onnx"], Decoder: paths["decoder.int8.onnx"],
		Joiner: paths["joiner.int8.onnx"], Tokens: paths["tokens.txt"],
	}, nil
}

func (p *Parakeet) downloadModel(ctx context.Context, spec parakeetModel) (string, func(), error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.modelBaseURL+spec.Archive, nil)
	if err != nil {
		return "", func() {}, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return "", func() {}, fmt.Errorf("download Parakeet model: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", func() {}, fmt.Errorf("download Parakeet model: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > spec.ArchiveBytes {
		return "", func() {}, fmt.Errorf("Parakeet model archive exceeds the expected %d-byte size", spec.ArchiveBytes)
	}
	file, err := os.CreateTemp(p.modelDir, "."+spec.ID+"-*.partial")
	if err != nil {
		return "", func() {}, err
	}
	partial := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(partial)
		return "", func() {}, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(partial)
	}
	hash := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(file, hash), io.LimitReader(response.Body, spec.ArchiveBytes+1))
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if written > spec.ArchiveBytes {
		cleanup()
		return "", func() {}, fmt.Errorf("Parakeet model archive exceeds the expected %d-byte size", spec.ArchiveBytes)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), spec.SHA256) {
		cleanup()
		return "", func() {}, errors.New("Parakeet model archive checksum mismatch")
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return partial, cleanup, nil
}

func extractParakeetModel(ctx context.Context, archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	return extractParakeetTar(ctx, tar.NewReader(bzip2.NewReader(file)), destination)
}

func extractParakeetTar(ctx context.Context, reader *tar.Reader, destination string) error {
	wanted := make(map[string]bool, len(requiredParakeetFiles))
	for _, name := range requiredParakeetFiles {
		wanted[name] = true
	}
	var expanded int64
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
		if err := safeModelArchivePath(header.Name); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if header.Size < 0 {
			return errors.New("Parakeet model archive contains a negative file size")
		}
		expanded += header.Size
		if expanded > maxParakeetExpandedBytes {
			return errors.New("expanded Parakeet model exceeds the 1 GB safety limit")
		}
		name := path.Base(path.Clean(strings.ReplaceAll(header.Name, "\\", "/")))
		if !wanted[name] {
			continue
		}
		if err := writeModelFile(ctx, filepath.Join(destination, name), reader, header.Size); err != nil {
			return err
		}
		delete(wanted, name)
	}
	if len(wanted) != 0 {
		missing := make([]string, 0, len(wanted))
		for _, name := range requiredParakeetFiles {
			if wanted[name] {
				missing = append(missing, name)
			}
		}
		return fmt.Errorf("Parakeet model archive is missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func safeModelArchivePath(name string) error {
	normalized := strings.ReplaceAll(name, "\\", "/")
	if normalized == "" || path.IsAbs(normalized) || (len(normalized) >= 2 && normalized[1] == ':') {
		return fmt.Errorf("unsafe Parakeet model archive path %q", name)
	}
	for _, component := range strings.Split(normalized, "/") {
		if component == ".." {
			return fmt.Errorf("unsafe Parakeet model archive path %q", name)
		}
	}
	if clean := path.Clean(normalized); clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("unsafe Parakeet model archive path %q", name)
	}
	return nil
}

func writeModelFile(ctx context.Context, destination string, source io.Reader, size int64) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := copyContext(ctx, file, io.LimitReader(source, size))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != size {
		return io.ErrUnexpectedEOF
	}
	return nil
}
