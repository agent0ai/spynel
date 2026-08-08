package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/media/miniaudio"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	parakeetSampleRate   = 16000
	maxTranscriptBytes   = 1024 * 1024
	parakeetEnglishModel = "english"
	parakeetMultiModel   = "multilingual"
)

type audioDecoder interface {
	ReadPCMFrames([]float32) (uint64, error)
	Close()
}

type speechRecognizer interface {
	Transcribe([]float32) (string, error)
	Close()
}

type recognizerFactory func(parakeetFiles, int) (speechRecognizer, error)

// Parakeet serializes local speech-to-text work across every transport. It
// decodes WAV, FLAC, and MP3 with miniaudio, keeps only one bounded PCM chunk
// in memory, and reuses the selected sherpa-onnx recognizer between messages.
type Parakeet struct {
	settings       *config.Store
	modelDir       string
	legacyModelDir string
	cacheInitErr   error
	client         *http.Client
	modelBaseURL   string
	log            io.Writer
	serial         chan struct{}
	models         map[string]parakeetModel
	openAudio      func(string) (audioDecoder, error)
	newRecognizer  recognizerFactory
	cachedModel    string
	cachedThreads  int
	cached         speechRecognizer
}

func NewParakeet(settings *config.Store, modelDir, legacyModelDir string, cacheInitErr error, log io.Writer) *Parakeet {
	return &Parakeet{
		settings: settings, modelDir: modelDir, legacyModelDir: legacyModelDir, cacheInitErr: cacheInitErr, client: http.DefaultClient, log: log,
		modelBaseURL: parakeetModelBaseURL,
		serial:       make(chan struct{}, 1), models: defaultParakeetModels(),
		openAudio: openAudioFile, newRecognizer: newSherpaRecognizer,
	}
}

func (p *Parakeet) Transcribe(ctx context.Context, audioPath string) (string, error) {
	select {
	case p.serial <- struct{}{}:
		defer func() { <-p.serial }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	cfg := p.settings.Snapshot().Speech
	if !cfg.Enabled {
		return "", errors.New("speech transcription is disabled")
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

	decoder, err := p.openAudio(audioPath)
	if err != nil {
		return "", fmt.Errorf("decode voice attachment (supported formats: WAV, FLAC, MP3, Ogg/Opus): %w", err)
	}
	defer decoder.Close()

	files, err := p.model(ctx, cfg)
	if err != nil {
		return "", err
	}
	recognizer, err := p.recognizer(files, cfg.NumThreads)
	if err != nil {
		return "", err
	}

	chunkFrames := cfg.ChunkSeconds * parakeetSampleRate
	remainingFrames := cfg.MaxDurationSec * parakeetSampleRate
	parts := make([]string, 0, (cfg.MaxDurationSec+cfg.ChunkSeconds-1)/cfg.ChunkSeconds)
	characters := 0
	for remainingFrames > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		framesToRead := min(chunkFrames, remainingFrames)
		pcm := make([]float32, framesToRead)
		framesRead, readErr := decoder.ReadPCMFrames(pcm)
		if framesRead > uint64(len(pcm)) {
			return "", errors.New("miniaudio returned an invalid PCM frame count")
		}
		pcm = pcm[:framesRead]
		if len(pcm) > 0 {
			text, transcribeErr := recognizer.Transcribe(pcm)
			if transcribeErr != nil {
				return "", transcribeErr
			}
			text = strings.TrimSpace(text)
			characters += len(text)
			if characters > maxTranscriptBytes {
				return "", errors.New("generated transcription exceeds the 1 MB safety limit")
			}
			if text != "" {
				parts = append(parts, text)
			}
			remainingFrames -= len(pcm)
		}
		if readErr != nil && !isAudioEnd(readErr) {
			return "", fmt.Errorf("decode voice attachment: %w", readErr)
		}
		if len(pcm) < framesToRead || readErr != nil {
			break
		}
	}
	if len(parts) == 0 {
		return "", errors.New("Parakeet produced an empty transcription")
	}
	return strings.Join(parts, "\n"), nil
}

func (p *Parakeet) recognizer(files parakeetFiles, threads int) (speechRecognizer, error) {
	key := files.Directory
	if p.cached != nil && p.cachedModel == key && p.cachedThreads == threads {
		return p.cached, nil
	}
	recognizer, err := p.newRecognizer(files, threads)
	if err != nil {
		return nil, err
	}
	previous := p.cached
	p.cachedModel = key
	p.cachedThreads = threads
	p.cached = recognizer
	if previous != nil {
		previous.Close()
	}
	return recognizer, nil
}

func openMiniaudio(path string) (audioDecoder, error) {
	return miniaudio.Open(path)
}

func isAudioEnd(err error) bool {
	return errors.Is(err, io.EOF)
}

type sherpaRecognizer struct {
	recognizer *sherpa.OfflineRecognizer
}

func newSherpaRecognizer(files parakeetFiles, threads int) (speechRecognizer, error) {
	recognizerConfig := sherpa.OfflineRecognizerConfig{}
	recognizerConfig.FeatConfig.SampleRate = parakeetSampleRate
	recognizerConfig.FeatConfig.FeatureDim = 80
	recognizerConfig.ModelConfig.Transducer.Encoder = files.Encoder
	recognizerConfig.ModelConfig.Transducer.Decoder = files.Decoder
	recognizerConfig.ModelConfig.Transducer.Joiner = files.Joiner
	recognizerConfig.ModelConfig.Tokens = files.Tokens
	recognizerConfig.ModelConfig.NumThreads = max(1, threads)
	recognizerConfig.ModelConfig.Provider = "cpu"
	recognizerConfig.ModelConfig.ModelType = "nemo_transducer"
	recognizerConfig.DecodingMethod = "greedy_search"
	recognizer := sherpa.NewOfflineRecognizer(&recognizerConfig)
	if recognizer == nil {
		return nil, errors.New("initialize sherpa-onnx Parakeet recognizer")
	}
	return &sherpaRecognizer{recognizer: recognizer}, nil
}

func (s *sherpaRecognizer) Transcribe(samples []float32) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}
	stream := sherpa.NewOfflineStream(s.recognizer)
	if stream == nil {
		return "", errors.New("create sherpa-onnx transcription stream")
	}
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(parakeetSampleRate, samples)
	s.recognizer.Decode(stream)
	return stream.GetResult().Text, nil
}

func (s *sherpaRecognizer) Close() {
	if s.recognizer != nil {
		sherpa.DeleteOfflineRecognizer(s.recognizer)
		s.recognizer = nil
	}
}

func (p *Parakeet) logf(format string, values ...any) {
	if p.log != nil {
		_, _ = fmt.Fprintf(p.log, format+"\n", values...)
	}
}

func resolveModelDirectory(store *config.Store, configured string) string {
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	return store.Snapshot().Resolve(configured)
}
