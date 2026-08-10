package media

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

const parakeetManifestVersion = 1

type parakeetAsset struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type parakeetManifest struct {
	Version       int                      `json:"version"`
	ArchiveSHA256 string                   `json:"archive_sha256"`
	Files         map[string]parakeetAsset `json:"files"`
}

type parakeetModel struct {
	ID           string
	Archive      string
	SHA256       string
	ArchiveBytes int64
	Notice       string
	Files        map[string]parakeetAsset
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
			Files: map[string]parakeetAsset{
				"encoder.int8.onnx": {SHA256: "6716910b7a0833997fec7a410494c995d70124001a0e9b66d6370d6aced577e0", Bytes: 654040552},
				"decoder.int8.onnx": {SHA256: "a5e223392c90e75f8144cdb5eb95af7625db389e39edef2bd1a9c872b3298fe6", Bytes: 7257753},
				"joiner.int8.onnx":  {SHA256: "869f43f7d24595c55581ad3bf249a935fb8a71389fbdaa7504b9f46f93140f8a", Bytes: 1735860},
				"tokens.txt":        {SHA256: "dc0b4584ab2e4ddbf888425c076c61b736e7356a015250db7d307e6f1a8188ff", Bytes: 8952},
			},
			Notice: "NVIDIA Parakeet Unified EN 0.6B\nGoverning terms: NVIDIA Open Model License Agreement\nhttps://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-license/\n\nDownloaded as the k2-fsa sherpa-onnx INT8 ONNX conversion.\n",
		},
		parakeetMultiModel: {
			ID:           "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8",
			Archive:      "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8.tar.bz2",
			SHA256:       "5793d0fd397c5778d2cf2126994d58e9d56b1be7c04d13c7a15bb1b4eafb16bf",
			ArchiveBytes: 487170055,
			Files: map[string]parakeetAsset{
				"encoder.int8.onnx": {SHA256: "acfc2b4456377e15d04f0243af540b7fe7c992f8d898d751cf134c3a55fd2247", Bytes: 652184281},
				"decoder.int8.onnx": {SHA256: "179e50c43d1a9de79c8a24149a2f9bac6eb5981823f2a2ed88d655b24248db4e", Bytes: 11845275},
				"joiner.int8.onnx":  {SHA256: "3164c13fc2821009440d20fcb5fdc78bff28b4db2f8d0f0b329101719c0948b3", Bytes: 6355277},
				"tokens.txt":        {SHA256: "d58544679ea4bc6ac563d1f545eb7d474bd6cfa467f0a6e2c1dc1c7d37e3c35d", Bytes: 93939},
			},
			Notice: "NVIDIA Parakeet TDT 0.6B v3\nCopyright NVIDIA Corporation\nLicensed under CC BY 4.0: https://creativecommons.org/licenses/by/4.0/\n\nDownloaded as the k2-fsa converted and INT8-quantized ONNX form; Spynel does not modify the weights.\n",
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
	if p.cacheInitErr != nil {
		return parakeetFiles{}, p.cacheInitErr
	}
	if err := os.MkdirAll(p.modelDir, 0o700); err != nil {
		return parakeetFiles{}, fmt.Errorf("create shared speech model cache %q: %w", p.modelDir, err)
	}
	lock, err := lockSpeechCache(ctx, filepath.Join(p.modelDir, ".install.lock"))
	if err != nil {
		return parakeetFiles{}, fmt.Errorf("coordinate shared speech cache %q: %w", p.modelDir, err)
	}
	defer unlockSpeechCache(lock)
	destination := filepath.Join(p.modelDir, spec.ID)
	if files, inspectErr := inspectManagedParakeetModel(destination, spec); inspectErr == nil {
		return files, nil
	}
	if _, statErr := os.Lstat(destination); statErr == nil {
		if err := os.RemoveAll(destination); err != nil {
			return parakeetFiles{}, fmt.Errorf("remove incomplete managed Parakeet model %q: %w", destination, err)
		}
	} else if !os.IsNotExist(statErr) {
		return parakeetFiles{}, fmt.Errorf("inspect managed Parakeet model: %w", statErr)
	}
	p.logf("Downloading Parakeet model %s (first use)", spec.ID)
	archive, cleanup, err := p.downloadModel(ctx, spec)
	if err != nil {
		return parakeetFiles{}, err
	}
	defer cleanup()
	temporary, err := os.MkdirTemp(p.modelDir, "."+spec.ID+"-*.partial")
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
	files, err := verifyParakeetAssets(temporary, spec.Files)
	if err != nil {
		return parakeetFiles{}, fmt.Errorf("verify extracted Parakeet model: %w", err)
	}
	if err := writeParakeetManifest(temporary, spec); err != nil {
		return parakeetFiles{}, err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return parakeetFiles{}, fmt.Errorf("install Parakeet model: %w", err)
	}
	files, err = inspectManagedParakeetModel(destination, spec)
	if err != nil {
		return parakeetFiles{}, err
	}
	p.logf("Parakeet model ready: %s", destination)
	return files, nil
}

func inspectManagedParakeetModel(directory string, spec parakeetModel) (parakeetFiles, error) {
	files, err := verifyParakeetAssets(directory, spec.Files)
	if err != nil {
		return parakeetFiles{}, err
	}
	data, err := os.ReadFile(filepath.Join(directory, "MODEL_SHA256"))
	var manifest parakeetManifest
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Version != parakeetManifestVersion ||
		!strings.EqualFold(manifest.ArchiveSHA256, spec.SHA256) || !equalParakeetAssets(manifest.Files, spec.Files) {
		return parakeetFiles{}, errors.New("managed model integrity marker is missing or incompatible")
	}
	return files, nil
}

func writeParakeetManifest(directory string, spec parakeetModel) error {
	manifest := parakeetManifest{Version: parakeetManifestVersion, ArchiveSHA256: spec.SHA256, Files: spec.Files}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode Parakeet model integrity marker: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(directory, "MODEL_SHA256"), data, 0o600); err != nil {
		return fmt.Errorf("write Parakeet model integrity marker: %w", err)
	}
	return nil
}

func verifyParakeetAssets(directory string, expected map[string]parakeetAsset) (parakeetFiles, error) {
	files, err := inspectParakeetModel(directory)
	if err != nil {
		return parakeetFiles{}, err
	}
	if len(expected) != len(requiredParakeetFiles) {
		return parakeetFiles{}, errors.New("model specification has incomplete installed-file integrity metadata")
	}
	for _, name := range requiredParakeetFiles {
		want, ok := expected[name]
		if !ok || want.Bytes <= 0 || len(want.SHA256) != sha256.Size*2 {
			return parakeetFiles{}, fmt.Errorf("model specification has invalid integrity metadata for %q", name)
		}
		file := filepath.Join(directory, name)
		info, err := os.Stat(file)
		if err != nil {
			return parakeetFiles{}, err
		}
		if info.Size() != want.Bytes {
			return parakeetFiles{}, fmt.Errorf("required model file %q has size %d, want %d", file, info.Size(), want.Bytes)
		}
		input, err := os.Open(file)
		if err != nil {
			return parakeetFiles{}, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, input)
		closeErr := input.Close()
		if copyErr != nil {
			return parakeetFiles{}, copyErr
		}
		if closeErr != nil {
			return parakeetFiles{}, closeErr
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), want.SHA256) {
			return parakeetFiles{}, fmt.Errorf("required model file %q checksum mismatch", file)
		}
	}
	return files, nil
}

func equalParakeetAssets(left, right map[string]parakeetAsset) bool {
	if len(left) != len(right) {
		return false
	}
	for name, expected := range right {
		actual, ok := left[name]
		if !ok || actual.Bytes != expected.Bytes || !strings.EqualFold(actual.SHA256, expected.SHA256) {
			return false
		}
	}
	return true
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
