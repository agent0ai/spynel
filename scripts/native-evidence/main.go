// native-evidence records deterministic harness and packaged-archive smoke
// results on the machine that actually runs the target binary. It never starts
// a real coding provider or records the runner environment.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	classification   = "observed-native"
	maxCommandOutput = 64 << 10
	maxEvidenceSize  = 32 << 10
)

type evidence struct {
	SchemaVersion          int             `json:"schema_version"`
	RecordedAt             string          `json:"recorded_at"`
	EvidenceClassification string          `json:"evidence_classification"`
	SyntheticFixturesOnly  bool            `json:"synthetic_fixtures_only"`
	SourceRef              string          `json:"source_ref"`
	SourceCommit           string          `json:"source_commit"`
	OS                     string          `json:"os"`
	Architecture           string          `json:"architecture"`
	GoVersion              string          `json:"go_version"`
	Archive                archiveEvidence `json:"archive"`
	Results                []result        `json:"results"`
}

type archiveEvidence struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type result struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Status    string `json:"status"`
	Assertion string `json:"assertion"`
}

type options struct {
	archiveDir string
	output     string
	sourceRef  string
	goBinary   string
}

func main() {
	var opts options
	flag.StringVar(&opts.archiveDir, "archive-dir", "release", "directory containing the archive for this native target")
	flag.StringVar(&opts.output, "output", "", "evidence JSON destination")
	flag.StringVar(&opts.sourceRef, "ref", "", "source tag or explicit local reference")
	defaultGo := os.Getenv("SPYNEL_GO_BINARY")
	if defaultGo == "" {
		defaultGo = "go"
	}
	flag.StringVar(&opts.goBinary, "go", defaultGo, "Go executable")
	flag.Parse()
	if opts.output == "" || opts.sourceRef == "" {
		fmt.Fprintln(os.Stderr, "usage: native-evidence --archive-dir DIR --output FILE --ref TAG [--go GO]")
		os.Exit(2)
	}
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "native evidence failed:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if !safeIdentifier(opts.sourceRef) {
		return errors.New("source ref must be a bounded path-safe tag or local reference")
	}
	root, err := projectRoot()
	if err != nil {
		return err
	}
	archivePath, version, err := findNativeArchive(opts.archiveDir, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	checksum, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	commitOutput, err := runCommand(root, nil, opts.goBinary, "version")
	if err != nil {
		return fmt.Errorf("read Go version: %w", err)
	}
	commit, err := gitCommit(root)
	if err != nil {
		return err
	}
	record := evidence{
		SchemaVersion: 1, RecordedAt: time.Now().UTC().Format(time.RFC3339),
		EvidenceClassification: classification, SyntheticFixturesOnly: true,
		SourceRef: opts.sourceRef, SourceCommit: commit, OS: runtime.GOOS,
		Architecture: runtime.GOARCH, GoVersion: strings.TrimSpace(commitOutput),
		Archive: archiveEvidence{Name: filepath.Base(archivePath), SHA256: checksum},
	}
	add := func(name, command, assertion string, commandErr error) error {
		status := "pass"
		if commandErr != nil {
			status = "fail"
		}
		record.Results = append(record.Results, result{Name: name, Command: command, Status: status, Assertion: assertion})
		if writeErr := writeEvidence(opts.output, record); writeErr != nil {
			return writeErr
		}
		return commandErr
	}

	_, err = runCommand(root, nil, opts.goBinary, "test", "-count=1", "./internal/harness")
	if err = add("deterministic-harness-contract", "go test -count=1 ./internal/harness", "synthetic provider contract suite passes on this native runner", err); err != nil {
		return err
	}

	tempRoot, err := os.MkdirTemp("", "spynel-native-evidence-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempRoot)
	extractRoot := filepath.Join(tempRoot, "archive smoke Ω")
	if err := os.MkdirAll(extractRoot, 0o700); err != nil {
		return err
	}
	if err := extractArchive(archivePath, extractRoot); err != nil {
		_ = add("archive-extraction", "extract packaged archive", "archive extracts safely into a path containing spaces and non-ASCII characters", err)
		return err
	}
	if err := add("archive-extraction", "extract packaged archive", "archive extracts safely into a path containing spaces and non-ASCII characters", nil); err != nil {
		return err
	}
	binary, err := findPackagedBinary(extractRoot)
	if err != nil {
		return err
	}
	home := filepath.Join(tempRoot, "isolated home Ω")
	workspace := filepath.Join(tempRoot, "workspace smoke Ω")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	env := smokeEnvironment(home, filepath.Dir(binary))

	output, commandErr := runCommand(extractRoot, env, binary, "--version")
	if commandErr == nil && !containsOutputLine(output, "spynel "+version) {
		commandErr = errors.New("packaged version does not match archive identity")
	}
	if err := add("packaged-version", "spynel --version", "packaged executable launches and its version matches the archive identity", commandErr); err != nil {
		return err
	}
	output, commandErr = runCommand(extractRoot, env, binary, "--help")
	if commandErr == nil && (!strings.Contains(output, "Spynel -") || !strings.Contains(output, "spynel doctor")) {
		commandErr = errors.New("packaged help contract was incomplete")
	}
	if err := add("packaged-help", "spynel --help", "harness-independent help launches from the extracted archive", commandErr); err != nil {
		return err
	}
	output, commandErr = runCommand(extractRoot, env, binary, "init", "--no-start", "--dir", workspace)
	if commandErr == nil && !strings.Contains(output, "Initialized Spynel") {
		commandErr = errors.New("initialization confirmation was missing")
	}
	if err := add("provider-free-initialization", "spynel init --no-start --dir <awkward-path>", "initialization succeeds without starting or discovering a provider", commandErr); err != nil {
		return err
	}
	output, commandErr = runCommand(workspace, env, binary, "doctor")
	expectedGuidance := "no coding harness is selected; run /harness"
	if commandErr == nil || !strings.Contains(output, expectedGuidance) {
		commandErr = errors.New("provider-free diagnostic did not fail with installation guidance")
	} else {
		commandErr = nil
	}
	if err := add("provider-free-detection-guidance", "spynel doctor", "missing providers fail cleanly with harness-selection guidance", commandErr); err != nil {
		return err
	}
	return writeEvidence(opts.output, record)
}

func containsOutputLine(output, expected string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == expected {
			return true
		}
	}
	return false
}

func projectRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot resolve native-evidence source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", errors.New("cannot locate repository root")
	}
	return root, nil
}

func findNativeArchive(directory, goos, goarch string) (string, string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", "", fmt.Errorf("read archive directory: %w", err)
	}
	extension := ".tar.gz"
	if goos == "windows" {
		extension = ".zip"
	}
	suffix := "_" + goos + "_" + goarch + extension
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "spynel_") && strings.HasSuffix(entry.Name(), suffix) {
			matches = append(matches, filepath.Join(directory, entry.Name()))
		}
	}
	if len(matches) != 1 {
		return "", "", fmt.Errorf("expected one archive for native target %s/%s, found %d", goos, goarch, len(matches))
	}
	base := filepath.Base(matches[0])
	version := strings.TrimSuffix(strings.TrimPrefix(base, "spynel_"), suffix)
	if !safeIdentifier(version) {
		return "", "", errors.New("archive version is not a bounded path-safe identifier")
	}
	return matches[0], version, nil
}

func safeIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._+-", character) {
			continue
		}
		return false
	}
	return true
}

func gitCommit(root string) (string, error) {
	output, err := runCommand(root, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read source commit: %w", err)
	}
	commit := strings.TrimSpace(output)
	if len(commit) != 40 {
		return "", errors.New("source commit is not a full Git object ID")
	}
	return commit, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
	left   int
}

func (w *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > w.left {
		data = data[:w.left]
	}
	if len(data) > 0 {
		_, _ = w.buffer.Write(data)
		w.left -= len(data)
	}
	return original, nil
}

func runCommand(directory string, environment []string, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	output := &limitedBuffer{left: maxCommandOutput}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	return output.buffer.String(), err
}

func smokeEnvironment(home, binaryDir string) []string {
	allowed := []string{"SystemRoot", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR"}
	environment := make([]string, 0, len(allowed)+6)
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	environment = append(environment,
		"HOME="+home,
		"USERPROFILE="+home,
		"PATH="+binaryDir,
		"LD_LIBRARY_PATH="+filepath.Join(binaryDir, "lib"),
		"DYLD_LIBRARY_PATH="+filepath.Join(binaryDir, "lib"),
	)
	return environment
}

func extractArchive(archivePath, destination string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destination)
	}
	return extractTarGzip(archivePath, destination)
}

func safeDestination(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." {
		return root, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("archive contains an unsafe path")
	}
	return filepath.Join(root, clean), nil
}

func extractZip(path, destination string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, entry := range reader.File {
		target, err := safeDestination(destination, entry.Name)
		if err != nil {
			return err
		}
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return errors.New("archive contains a symbolic link")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		mode := entry.Mode().Perm()
		if mode == 0 {
			mode = 0o600
		}
		err = copyFile(target, source, mode)
		_ = source.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGzip(path, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeDestination(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := copyFile(target, reader, fs.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		default:
			return errors.New("archive contains an unsupported entry type")
		}
	}
}

func copyFile(path string, source io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o600
	}
	target, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func findPackagedBinary(root string) (string, error) {
	name := "spynel"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one packaged executable, found %d", len(matches))
	}
	return matches[0], nil
}

func writeEvidence(path string, record evidence) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxEvidenceSize {
		return errors.New("evidence record exceeds the bounded size")
	}
	return fsx.AtomicWriteFile(path, data, 0o600)
}
