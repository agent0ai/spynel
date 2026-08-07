package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/frdel/spynel/internal/config"
)

const (
	whisperModelBaseURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	maxModelBytes       = int64(6 * 1024 * 1024 * 1024)
	maxTranscriptBytes  = 1024 * 1024
)

// Whisper serializes local speech-to-text work across every transport. Audio
// and intermediate PCM live on disk; only a bounded transcript is retained in
// memory. Configuration is read at call time so form changes apply immediately.
type Whisper struct {
	settings       *config.Store
	modelDir       string
	workDir        string
	client         *http.Client
	log            io.Writer
	serial         chan struct{}
	runtimeBaseURL string
	runtimeGOOS    string
	runtimeGOARCH  string
	runtimeAssets  map[string]whisperRuntimeAsset
	lookPath       func(string) (string, error)
}

func NewWhisper(settings *config.Store, modelDir, workDir string, log io.Writer) *Whisper {
	return &Whisper{
		settings: settings, modelDir: modelDir, workDir: workDir,
		client: http.DefaultClient, log: log, serial: make(chan struct{}, 1),
		runtimeBaseURL: whisperRuntimeBaseURL, runtimeGOOS: runtime.GOOS, runtimeGOARCH: runtime.GOARCH,
		runtimeAssets: whisperRuntimeAssets, lookPath: exec.LookPath,
	}
}

func (w *Whisper) Transcribe(ctx context.Context, audioPath string) (string, error) {
	select {
	case w.serial <- struct{}{}:
		defer func() { <-w.serial }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	cfg := w.settings.Snapshot().Speech
	if !cfg.Enabled {
		return "", errors.New("speech transcription is disabled")
	}
	command, err := w.command(ctx, cfg.Command)
	if err != nil {
		return "", err
	}
	if _, err := exec.LookPath(cfg.FFmpegCommand); err != nil {
		return "", fmt.Errorf("find FFmpeg executable %q: %w", cfg.FFmpegCommand, err)
	}
	info, err := os.Stat(audioPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("voice attachment is not a regular file")
	}
	if info.Size() > int64(cfg.MaxFileMB)*1024*1024 {
		return "", fmt.Errorf("voice attachment exceeds speech.max_file_mb (%d MB)", cfg.MaxFileMB)
	}
	modelPath, err := w.model(ctx, cfg)
	if err != nil {
		return "", err
	}
	chunks, cleanup, err := w.audioChunks(ctx, cfg, audioPath)
	if err != nil {
		return "", err
	}
	defer cleanup()
	parts := make([]string, 0, len(chunks))
	characters := 0
	for index, chunk := range chunks {
		text, err := w.transcribeChunk(ctx, cfg, command, modelPath, chunk, index)
		if err != nil {
			return "", err
		}
		characters += len(text)
		if characters > maxTranscriptBytes {
			return "", errors.New("generated transcription exceeds the 1 MB safety limit")
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return "", errors.New("Whisper produced an empty transcription")
	}
	return strings.Join(parts, "\n"), nil
}

func (w *Whisper) model(ctx context.Context, cfg config.Speech) (string, error) {
	if strings.TrimSpace(cfg.ModelPath) != "" {
		path := cfg.ModelPath
		if !filepath.IsAbs(path) {
			path = w.settings.Snapshot().Resolve(path)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("configured Whisper model %q is unavailable", path)
		}
		return path, nil
	}
	model := whisperModelName(cfg.Model)
	if model == "" {
		return "", fmt.Errorf("unsupported Whisper model %q", cfg.Model)
	}
	if err := os.MkdirAll(w.modelDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(w.modelDir, "ggml-"+model+".bin")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return path, nil
	}
	w.logf("Downloading local Whisper model %s (first use)", model)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, whisperModelBaseURL+"ggml-"+model+".bin", nil)
	if err != nil {
		return "", err
	}
	response, err := w.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download Whisper model: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Whisper model: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxModelBytes {
		return "", errors.New("Whisper model exceeds the 6 GB safety limit")
	}
	temporary, err := os.CreateTemp(w.modelDir, ".whisper-model-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	written, err := copyContext(ctx, temporary, io.LimitReader(response.Body, maxModelBytes+1))
	if err != nil {
		return "", err
	}
	if written > maxModelBytes {
		return "", errors.New("Whisper model exceeds the 6 GB safety limit")
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	w.logf("Whisper model ready: %s", path)
	return path, nil
}

func whisperModelName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tiny", "base", "small", "medium", "large-v1", "large-v2", "large-v3", "large-v3-turbo":
		return strings.ToLower(strings.TrimSpace(value))
	case "large":
		return "large-v3"
	case "turbo":
		return "large-v3-turbo"
	default:
		return ""
	}
}

func (w *Whisper) audioChunks(ctx context.Context, cfg config.Speech, audioPath string) ([]string, func(), error) {
	if err := os.MkdirAll(w.workDir, 0o700); err != nil {
		return nil, func() {}, err
	}
	directory, err := os.MkdirTemp(w.workDir, "voice-*")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	pattern := filepath.Join(directory, "chunk-%04d.wav")
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-i", audioPath,
		"-t", fmt.Sprintf("%d", cfg.MaxDurationSec), "-f", "segment",
		"-segment_time", fmt.Sprintf("%d", cfg.ChunkSeconds),
		"-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", pattern,
	}
	command := exec.CommandContext(ctx, cfg.FFmpegCommand, args...)
	stderr := newCommandBuffer(64 * 1024)
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("convert voice message with %s: %w: %s", cfg.FFmpegCommand, err, strings.TrimSpace(stderr.String()))
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	var chunks []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "chunk-") && strings.HasSuffix(entry.Name(), ".wav") {
			chunks = append(chunks, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Strings(chunks)
	if len(chunks) == 0 {
		cleanup()
		return nil, func() {}, errors.New("ffmpeg produced no audio chunks")
	}
	return chunks, cleanup, nil
}

func (w *Whisper) transcribeChunk(ctx context.Context, cfg config.Speech, commandPath, modelPath, chunk string, index int) (string, error) {
	prefix := filepath.Join(filepath.Dir(chunk), fmt.Sprintf("transcript-%04d", index))
	args := []string{"-m", modelPath, "-f", chunk, "-nt", "-otxt", "-of", prefix}
	if language := strings.TrimSpace(cfg.Language); language != "" && !strings.EqualFold(language, "auto") {
		args = append(args, "-l", language)
	}
	command := exec.CommandContext(ctx, commandPath, args...)
	output := newCommandBuffer(1024 * 1024)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("run %s: %w: %s", commandPath, err, strings.TrimSpace(output.String()))
	}
	transcriptPath := prefix + ".txt"
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("inspect Whisper transcript: %w", err)
	}
	if info.Size() > maxTranscriptBytes {
		return "", errors.New("Whisper chunk transcript exceeds the 1 MB safety limit")
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("read Whisper transcript: %w", err)
	}
	return string(data), nil
}

func (w *Whisper) logf(format string, values ...any) {
	if w.log != nil {
		_, _ = fmt.Fprintf(w.log, format+"\n", values...)
	}
}

type commandBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newCommandBuffer(limit int) *commandBuffer { return &commandBuffer{limit: limit} }

func (b *commandBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, data...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(data), nil
}

func (b *commandBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
