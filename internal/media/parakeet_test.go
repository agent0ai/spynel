package media

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
)

func TestModelKindUsesEnglishOnlyForEnglish(t *testing.T) {
	if got := modelKind("en"); got != parakeetEnglishModel {
		t.Fatalf("English model kind = %q", got)
	}
	for _, language := range []string{"auto", "de", "fr", "uk"} {
		if got := modelKind(language); got != parakeetMultiModel {
			t.Fatalf("model kind for %q = %q", language, got)
		}
	}
}

func TestParakeetChunksAndSerializesTranscription(t *testing.T) {
	root := t.TempDir()
	modelDir := testParakeetModel(t, filepath.Join(root, "model"))
	audio := filepath.Join(root, "voice.mp3")
	if err := os.WriteFile(audio, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Speech.ModelDir = modelDir
	cfg.Speech.ChunkSeconds = 1
	cfg.Speech.MaxDurationSec = 2
	store := config.NewStore(cfg)
	recognizer := &recordingRecognizer{delay: 20 * time.Millisecond}
	worker := NewParakeet(store, filepath.Join(root, "managed"), nil)
	worker.openAudio = func(string) (audioDecoder, error) {
		return &sliceDecoder{samples: make([]float32, parakeetSampleRate+8000)}, nil
	}
	factoryCalls := 0
	worker.newRecognizer = func(parakeetFiles, int) (speechRecognizer, error) {
		factoryCalls++
		return recognizer, nil
	}

	var group sync.WaitGroup
	errorsChannel := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			text, err := worker.Transcribe(context.Background(), audio)
			if err != nil {
				errorsChannel <- err
				return
			}
			if text != "chunk 1\nchunk 2" && text != "chunk 3\nchunk 4" {
				errorsChannel <- fmt.Errorf("unexpected transcript %q", text)
			}
		}()
	}
	group.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatal(err)
	}
	if factoryCalls != 1 {
		t.Fatalf("recognizer factory calls = %d, want 1", factoryCalls)
	}
	recognizer.mu.Lock()
	defer recognizer.mu.Unlock()
	if recognizer.maxActive != 1 {
		t.Fatalf("concurrent recognitions = %d, want 1", recognizer.maxActive)
	}
	wantSizes := []int{16000, 8000, 16000, 8000}
	if fmt.Sprint(recognizer.sizes) != fmt.Sprint(wantSizes) {
		t.Fatalf("chunk sizes = %v, want %v", recognizer.sizes, wantSizes)
	}
}

func TestParakeetUsesMiniaudioForWAV(t *testing.T) {
	root := t.TempDir()
	modelDir := testParakeetModel(t, filepath.Join(root, "model"))
	audio := filepath.Join(root, "voice.wav")
	writeTestWAV(t, audio, 800)
	cfg := config.Default()
	cfg.Root = root
	cfg.Speech.ModelDir = modelDir
	worker := NewParakeet(config.NewStore(cfg), filepath.Join(root, "managed"), nil)
	worker.newRecognizer = func(parakeetFiles, int) (speechRecognizer, error) {
		return &fixedRecognizer{text: "decoded locally"}, nil
	}
	text, err := worker.Transcribe(context.Background(), audio)
	if err != nil {
		t.Fatal(err)
	}
	if text != "decoded locally" {
		t.Fatalf("transcript = %q", text)
	}
}

func TestParakeetDecodesTelegramStyleOggOpus(t *testing.T) {
	root := t.TempDir()
	modelDir := testParakeetModel(t, filepath.Join(root, "model"))
	audio := filepath.Join(root, "voice-without-trusted-extension.bin")
	encoded, err := base64.StdEncoding.DecodeString(testOggOpusBase64)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Speech.ModelDir = modelDir
	recognizer := &sampleRecognizer{}
	worker := NewParakeet(config.NewStore(cfg), filepath.Join(root, "managed"), nil)
	worker.newRecognizer = func(parakeetFiles, int) (speechRecognizer, error) {
		return recognizer, nil
	}
	text, err := worker.Transcribe(context.Background(), audio)
	if err != nil {
		t.Fatal(err)
	}
	if text != "decoded Ogg/Opus voice" {
		t.Fatalf("transcript = %q", text)
	}
	if len(recognizer.samples) < 3500 || len(recognizer.samples) > 4500 {
		t.Fatalf("decoded samples = %d, want approximately 4000", len(recognizer.samples))
	}
	nonzero := false
	for _, sample := range recognizer.samples {
		if sample != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Fatal("decoded Ogg/Opus voice contains only silence")
	}
}

func TestOggOpusRejectsCorruptPage(t *testing.T) {
	root := t.TempDir()
	audio := filepath.Join(root, "voice.ogg")
	encoded, err := base64.StdEncoding.DecodeString(testOggOpusBase64)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if err := os.WriteFile(audio, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	decoder, err := openAudioFile(audio)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	_, err = decoder.ReadPCMFrames(make([]float32, parakeetSampleRate))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("corrupt Ogg error = %v", err)
	}
}

func TestOggOpusRejectsImpossibleCommentCount(t *testing.T) {
	header := make([]byte, 16)
	copy(header, "OpusTags")
	binary.LittleEndian.PutUint32(header[12:16], ^uint32(0))
	if err := validateOggOpusTags(header); err == nil || !strings.Contains(err.Error(), "comment count") {
		t.Fatalf("comment-count error = %v", err)
	}
}

func TestOggOpusParsesSignedGainAndBoundsGranuleConversion(t *testing.T) {
	header := make([]byte, 19)
	copy(header, "OpusHead")
	header[8] = 1
	header[9] = 1
	binary.LittleEndian.PutUint16(header[16:18], uint16(0xff00))
	parsed, err := parseOggOpusHead(header)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.outputGainQ8 != -256 {
		t.Fatalf("signed output gain = %d, want -256", parsed.outputGainQ8)
	}
	if frames := playableOpusFrames(math.MaxUint64-1, 0); frames < 0 {
		t.Fatalf("maximum granule produced negative frame count: %d", frames)
	}
}

func TestRecognizerReplacementKeepsWorkingRecognizerOnLoadFailure(t *testing.T) {
	worker := NewParakeet(config.NewStore(config.Default()), t.TempDir(), nil)
	previous := &trackingRecognizer{}
	worker.cached = previous
	worker.cachedModel = "old"
	worker.cachedThreads = 2
	worker.newRecognizer = func(parakeetFiles, int) (speechRecognizer, error) {
		return nil, errors.New("cannot load replacement")
	}
	if _, err := worker.recognizer(parakeetFiles{Directory: "new"}, 2); err == nil {
		t.Fatal("replacement load failure was ignored")
	}
	if worker.cached != previous || previous.closed {
		t.Fatal("working recognizer was discarded before its replacement loaded")
	}
}

func TestParakeetRejectsUnsupportedAudioBeforeModelDownload(t *testing.T) {
	root := t.TempDir()
	audio := filepath.Join(root, "voice.m4a")
	if err := os.WriteFile(audio, []byte("not an AAC decoder fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := NewParakeet(config.NewStore(config.Default()), filepath.Join(root, "models"), nil)
	_, err := worker.Transcribe(context.Background(), audio)
	if err == nil || !strings.Contains(err.Error(), "supported formats: WAV, FLAC, MP3, Ogg/Opus") {
		t.Fatalf("unsupported format error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "models")); !os.IsNotExist(statErr) {
		t.Fatalf("model download started for unsupported audio: %v", statErr)
	}
}

func TestParakeetDownloadsVerifiedPartialArchive(t *testing.T) {
	payload := []byte("verified archive")
	hash := sha256.Sum256(payload)
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downloads++
		if request.URL.Path != "/model.tar.bz2" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	root := t.TempDir()
	worker := NewParakeet(config.NewStore(config.Default()), root, nil)
	worker.modelBaseURL = server.URL + "/"
	spec := parakeetModel{ID: "fixture", Archive: "model.tar.bz2", SHA256: hex.EncodeToString(hash[:]), ArchiveBytes: int64(len(payload))}
	archive, cleanup, err := worker.downloadModel(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archive, ".partial") {
		t.Fatalf("download path = %q", archive)
	}
	data, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("downloaded archive = %q, %v", data, err)
	}
	cleanup()
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("partial archive survived cleanup: %v", err)
	}
	if downloads != 1 {
		t.Fatalf("downloads = %d", downloads)
	}
}

func TestParakeetRejectsModelChecksumMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("archive"))
	}))
	defer server.Close()
	root := t.TempDir()
	worker := NewParakeet(config.NewStore(config.Default()), root, nil)
	worker.modelBaseURL = server.URL + "/"
	_, cleanup, err := worker.downloadModel(context.Background(), parakeetModel{
		ID: "fixture", Archive: "model.tar.bz2", SHA256: strings.Repeat("0", 64), ArchiveBytes: 7,
	})
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum error = %v", err)
	}
}

func TestExtractParakeetModelSelectsRequiredFilesAndRejectsTraversal(t *testing.T) {
	archive := parakeetTarFixture(t, "model")
	destination := t.TempDir()
	if err := extractParakeetTar(context.Background(), tar.NewReader(bytes.NewReader(archive)), destination); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectParakeetModel(destination); err != nil {
		t.Fatal(err)
	}
	unsafeArchive := tarFixture(t, map[string][]byte{"../encoder.int8.onnx": []byte("bad")})
	err := extractParakeetTar(context.Background(), tar.NewReader(bytes.NewReader(unsafeArchive)), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("traversal error = %v", err)
	}
	for _, unsafeName := range []string{"/encoder.int8.onnx", "C:/encoder.int8.onnx"} {
		unsafeArchive = tarFixture(t, map[string][]byte{unsafeName: []byte("bad")})
		err = extractParakeetTar(context.Background(), tar.NewReader(bytes.NewReader(unsafeArchive)), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("absolute path %q error = %v", unsafeName, err)
		}
	}
}

func TestParakeetRealIntegration(t *testing.T) {
	audio := os.Getenv("SPYNEL_TEST_VOICE")
	modelDir := os.Getenv("SPYNEL_TEST_PARAKEET_MODEL_DIR")
	if audio == "" || modelDir == "" {
		t.Skip("set SPYNEL_TEST_VOICE and SPYNEL_TEST_PARAKEET_MODEL_DIR for the real Parakeet integration test")
	}
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Speech.ModelDir = modelDir
	worker := NewParakeet(config.NewStore(cfg), filepath.Join(cfg.Root, "models"), os.Stderr)
	text, err := worker.Transcribe(context.Background(), audio)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("real Parakeet integration produced no transcription")
	}
	t.Logf("transcription: %s", strings.TrimSpace(text))
}

type sliceDecoder struct {
	samples []float32
	index   int
}

func (d *sliceDecoder) ReadPCMFrames(output []float32) (uint64, error) {
	if d.index >= len(d.samples) {
		return 0, io.EOF
	}
	written := copy(output, d.samples[d.index:])
	d.index += written
	if d.index == len(d.samples) {
		return uint64(written), io.EOF
	}
	return uint64(written), nil
}

func (*sliceDecoder) Close() {}

type recordingRecognizer struct {
	mu        sync.Mutex
	delay     time.Duration
	active    int
	maxActive int
	sizes     []int
	calls     int
}

func (r *recordingRecognizer) Transcribe(samples []float32) (string, error) {
	r.mu.Lock()
	r.active++
	r.calls++
	call := r.calls
	r.sizes = append(r.sizes, len(samples))
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.mu.Unlock()
	time.Sleep(r.delay)
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return fmt.Sprintf("chunk %d", call), nil
}

func (*recordingRecognizer) Close() {}

type fixedRecognizer struct{ text string }

func (r *fixedRecognizer) Transcribe([]float32) (string, error) { return r.text, nil }
func (*fixedRecognizer) Close()                                 {}

type sampleRecognizer struct{ samples []float32 }

func (r *sampleRecognizer) Transcribe(samples []float32) (string, error) {
	r.samples = append(r.samples, samples...)
	return "decoded Ogg/Opus voice", nil
}

func (*sampleRecognizer) Close() {}

type trackingRecognizer struct{ closed bool }

func (*trackingRecognizer) Transcribe([]float32) (string, error) { return "", nil }
func (r *trackingRecognizer) Close()                             { r.closed = true }

func testParakeetModel(t *testing.T, directory string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredParakeetFiles {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func parakeetTarFixture(t *testing.T, prefix string) []byte {
	t.Helper()
	files := map[string][]byte{prefix + "/README.md": []byte("ignored")}
	for _, name := range requiredParakeetFiles {
		files[prefix+"/"+name] = []byte(name)
	}
	return tarFixture(t, files)
}

func tarFixture(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, data := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func writeTestWAV(t *testing.T, destination string, sampleCount int) {
	t.Helper()
	var data bytes.Buffer
	dataSize := uint32(sampleCount * 2)
	data.WriteString("RIFF")
	_ = binary.Write(&data, binary.LittleEndian, uint32(36)+dataSize)
	data.WriteString("WAVEfmt ")
	_ = binary.Write(&data, binary.LittleEndian, uint32(16))
	_ = binary.Write(&data, binary.LittleEndian, uint16(1))
	_ = binary.Write(&data, binary.LittleEndian, uint16(1))
	_ = binary.Write(&data, binary.LittleEndian, uint32(parakeetSampleRate))
	_ = binary.Write(&data, binary.LittleEndian, uint32(parakeetSampleRate*2))
	_ = binary.Write(&data, binary.LittleEndian, uint16(2))
	_ = binary.Write(&data, binary.LittleEndian, uint16(16))
	data.WriteString("data")
	_ = binary.Write(&data, binary.LittleEndian, dataSize)
	for index := 0; index < sampleCount; index++ {
		_ = binary.Write(&data, binary.LittleEndian, int16(index%200-100))
	}
	if err := os.WriteFile(destination, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 250 ms mono Ogg/Opus fixture encoded by FFmpeg's libopus encoder in VOIP
// mode. Keeping a reference-produced stream here tests the decoder against the
// container and packet shape used by Telegram and WhatsApp voice notes.
const testOggOpusBase64 = "T2dnUwACAAAAAAAAAACubWPkAAAAAFTE4j4BE09wdXNIZWFkAQE4AYC7AAAAAABPZ2dTAAAAAAAAAAAAAK5tY+QBAAAAA5zEcwE+T3B1c1RhZ3MNAAAATGF2ZjU5LjI3LjEwMAEAAAAdAAAAZW5jb2Rlcj1MYXZjNTkuMzcuMTAwIGxpYm9wdXNPZ2dTAAQYMAAAAAAAAK5tY+QCAAAA1KjXmg1FLjdAOzc1NjstMTMpeIIBt2x+QOYAAAZLtOMkceUCsgQj3W4HM8v/ALF6J/iUPiMTFBnMd2r3tLqPb0qFWuIOL35kw/EH9bTWzbSle51OlAD5eKM/96yYhQNXTnCjb4MGJeMafOSIqKJxXQEG/Ws5N70sl3XndkZwdlv4a2tJRXiboxJTqkh90UoIhOsDka/RTkNfwUJiTIFSvKlytS38yMTsCnRGJdhJ9lzK+0uUXQ+wFcJ0sAV4m6MSVs4iKkUuiQHcqhbxldKtCPWBg+WHJRp02UFtPawekspCZ95A8x0JvrxlI1UWSSl1uWsM9d8pghRHT+JmeJujX3Wc/Ekzsz+qweWJz8wsDKtBH3/e/b9GuxC7owyFAHUE5Pu/MYj0LmdHQ4BPHGQv4S1OqYuMI0N4m6MXUV8mJS6l9uAvzbc4H6K9xBpHBQNnWhiMXtlrc+y3z5exJlGtCjIHkrBmsg6hPavlpBkReJujX3Wc/EXvX399MZARcTwPSY3mdH8noxl1iX2/9VMi5/EVpSKxChDr4/JI0UmLXMVppAt4m6MSUUT99xxQ8LNyxDLV/jJt8jW6dMmW+Hj834QYknQ/yiCBikGXX7DLYwmpYIRB416UKYt4m6MSVs4iGox4qwA/5afF+6GtgbQQZuFTT7bQgOPp82AOT/Y+/yNw9eHz4wR9NdzGLxAzBNo1Aal7UUibo191nPxJM7MhTjNkQmmFj0bB8WzDMc94ddTRU3bQSMqbloeJkYeHC8ZqREibfmCTT8OWofNkKEJS8Ur1FUqc2WRQNnonjIcFu6X+sr4IdgEDzjjmHyvJoOhwltRImysfdZz8Re9OGZMByaRd0uFBTvbBYzeKxxeWHfdsKh0cDmNxGi0h9lNJlRUPxLe7+mhInDPQB/KaGY9Eg62XdLP8rpgZHMeDdFIjQ4EuNQK9vn26GkmSnjR3SA=="
