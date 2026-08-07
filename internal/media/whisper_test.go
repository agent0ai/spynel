package media

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/frdel/spynel/internal/config"
)

func TestWhisperSplitsToDiskAndSerializesTranscription(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell speech fixtures")
	}
	root := t.TempDir()
	ffmpeg := filepath.Join(root, "fake-ffmpeg")
	whisper := filepath.Join(root, "fake-whisper")
	lock := filepath.Join(root, "speech-lock")
	overlap := filepath.Join(root, "overlap")
	ffmpegScript := `#!/bin/sh
input=
last=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-i" ]; then shift; input=$1; fi
  last=$1
  shift
done
output=$(printf '%s' "$last" | sed 's/%04d/0000/')
cp "$input" "$output"
`
	whisperScript := `#!/bin/sh
prefix=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-of" ]; then shift; prefix=$1; fi
  shift
done
if ! mkdir "` + lock + `" 2>/dev/null; then touch "` + overlap + `"; fi
sleep 0.05
printf 'generated transcript' > "$prefix.txt"
rmdir "` + lock + `" 2>/dev/null || true
`
	if err := os.WriteFile(ffmpeg, []byte(ffmpegScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(whisper, []byte(whisperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(root, "ggml-base.bin")
	audio := filepath.Join(root, "voice.ogg")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Speech.Enabled = true
	cfg.Speech.Command = whisper
	cfg.Speech.FFmpegCommand = ffmpeg
	cfg.Speech.ModelPath = model
	cfg.Speech.Language = "auto"
	worker := NewWhisper(config.NewStore(cfg), filepath.Join(root, "models"), filepath.Join(root, "work"), nil)

	var group sync.WaitGroup
	errorsChannel := make(chan error, 2)
	results := make(chan string, 2)
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			text, err := worker.Transcribe(context.Background(), audio)
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- text
		}()
	}
	group.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatal(err)
	}
	close(results)
	for text := range results {
		if strings.TrimSpace(text) != "generated transcript" {
			t.Fatalf("transcript = %q", text)
		}
	}
	if _, err := os.Stat(overlap); !os.IsNotExist(err) {
		t.Fatal("speech workers overlapped")
	}
	entries, err := os.ReadDir(filepath.Join(root, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary speech artifacts were retained: %#v", entries)
	}
}

func TestWhisperProvisionsVerifiedOfficialRuntimeForDefaultCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell speech fixtures")
	}
	root := t.TempDir()
	archive := whisperRuntimeFixture(t, `#!/bin/sh
prefix=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-of" ]; then shift; prefix=$1; fi
  shift
done
printf 'runtime-provisioned transcript' > "$prefix.txt"
`)
	hash := sha256.Sum256(archive)
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downloads++
		_, _ = writer.Write(archive)
	}))
	defer server.Close()
	ffmpeg := filepath.Join(root, "fake-ffmpeg")
	ffmpegScript := `#!/bin/sh
input=
last=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-i" ]; then shift; input=$1; fi
  last=$1
  shift
done
output=$(printf '%s' "$last" | sed 's/%04d/0000/')
cp "$input" "$output"
`
	if err := os.WriteFile(ffmpeg, []byte(ffmpegScript), 0o700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(root, "ggml-small.bin")
	audio := filepath.Join(root, "voice.ogg")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Speech.FFmpegCommand = ffmpeg
	cfg.Speech.ModelPath = model
	worker := NewWhisper(config.NewStore(cfg), filepath.Join(root, "models"), filepath.Join(root, "work"), nil)
	worker.lookPath = func(string) (string, error) { return "", errors.New("not installed") }
	worker.runtimeBaseURL = server.URL + "/"
	worker.runtimeGOOS = "linux"
	worker.runtimeGOARCH = "fixture"
	worker.runtimeAssets = map[string]whisperRuntimeAsset{
		"linux/fixture": {Name: "runtime.tar.gz", SHA256: hex.EncodeToString(hash[:]), Format: "tar.gz"},
	}
	for attempt := 0; attempt < 2; attempt++ {
		text, err := worker.Transcribe(context.Background(), audio)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(text) != "runtime-provisioned transcript" {
			t.Fatalf("transcript = %q", text)
		}
	}
	if downloads != 1 {
		t.Fatalf("runtime downloads = %d, want one cached download", downloads)
	}
	if _, err := findWhisperExecutable(filepath.Join(root, "models", "runtime", whisperRuntimeVersion), "linux"); err != nil {
		t.Fatalf("provisioned executable: %v", err)
	}
}

func TestWhisperRejectsRuntimeChecksumMismatch(t *testing.T) {
	archive := whisperRuntimeFixture(t, "runtime")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(archive)
	}))
	defer server.Close()
	root := t.TempDir()
	worker := NewWhisper(config.NewStore(config.Default()), filepath.Join(root, "models"), filepath.Join(root, "work"), nil)
	worker.runtimeBaseURL = server.URL + "/"
	_, cleanup, err := worker.downloadRuntime(context.Background(), root, whisperRuntimeAsset{Name: "runtime.tar.gz", SHA256: strings.Repeat("0", 64), Format: "tar.gz"})
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum error = %v", err)
	}
}

func TestWhisperTranscribesRealAudio(t *testing.T) {
	audio := os.Getenv("SPYNEL_TEST_VOICE")
	model := os.Getenv("SPYNEL_TEST_WHISPER_MODEL")
	configPath := os.Getenv("SPYNEL_TEST_CONFIG")
	if audio == "" || (model == "" && configPath == "") {
		t.Skip("set SPYNEL_TEST_VOICE with SPYNEL_TEST_WHISPER_MODEL or SPYNEL_TEST_CONFIG for the real Whisper integration test")
	}
	var cfg config.Config
	var err error
	if configPath != "" {
		cfg, err = config.Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		cfg = config.Default()
		cfg.Root = t.TempDir()
	}
	if model != "" {
		cfg.Speech.ModelPath = model
	}
	worker := NewWhisper(config.NewStore(cfg), cfg.StatePath("models", "whisper"), cfg.StatePath("runtime", "speech"), os.Stderr)
	text, err := worker.Transcribe(context.Background(), audio)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("real Whisper integration produced no transcription")
	}
	t.Logf("transcription: %s", strings.TrimSpace(text))
}

func whisperRuntimeFixture(t *testing.T, executable string) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	data := []byte(executable)
	if err := writer.WriteHeader(&tar.Header{Name: "whisper-runtime/whisper-cli", Mode: 0o700, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
