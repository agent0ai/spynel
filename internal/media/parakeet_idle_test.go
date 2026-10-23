package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent0ai/spynel/internal/config"
)

type idleRecognizer struct {
	frames, calls int
	closes        atomic.Int32
	transcribe    func() error
}

func (r *idleRecognizer) Transcribe(pcm []float32) (string, error) {
	if r.closes.Load() != 0 {
		panic("use after close")
	}
	r.calls++
	r.frames += len(pcm)
	if r.transcribe != nil {
		if err := r.transcribe(); err != nil {
			return "", err
		}
	}
	return "complete chunk", nil
}

func (r *idleRecognizer) Close() {
	if r.closes.Add(1) != 1 {
		panic("double close")
	}
}

func idleWorker(t *testing.T) (*Parakeet, string, *[]*idleRecognizer) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root, cfg.Path = root, config.PathForRoot(root)
	cfg.Speech.ModelDir = testParakeetModel(t, filepath.Join(root, "model"))
	cfg.Speech.IdleTimeoutSec = 2
	cfg.Speech.ChunkSeconds = 1
	worker := NewParakeet(config.NewStore(cfg), "", nil, nil)
	audio := filepath.Join(root, "voice.wav")
	if err := os.WriteFile(audio, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	worker.openAudio = func(string) (audioDecoder, error) {
		return &sliceDecoder{samples: make([]float32, 24000)}, nil
	}
	var created []*idleRecognizer
	worker.newRecognizer = func(parakeetFiles, int) (speechRecognizer, error) {
		r := &idleRecognizer{}
		created = append(created, r)
		return r, nil
	}
	return worker, audio, &created
}

func transcribeIdleFixture(t *testing.T, worker *Parakeet, audio string) {
	t.Helper()
	text, err := worker.Transcribe(context.Background(), audio)
	if err != nil || text != "complete chunk\ncomplete chunk" {
		t.Fatalf("transcription = %q, %v", text, err)
	}
}

func TestParakeetIdleExpiryBurstAndReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		worker, audio, created := idleWorker(t)
		defer worker.Close()
		transcribeIdleFixture(t, worker, audio)
		time.Sleep(time.Second)
		transcribeIdleFixture(t, worker, audio)
		time.Sleep(time.Second)
		synctest.Wait()
		if len(*created) != 1 || (*created)[0].closes.Load() != 0 {
			t.Fatal("burst did not retain recognizer")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if (*created)[0].closes.Load() != 1 || worker.cached != nil {
			t.Fatal("idle recognizer was not unloaded")
		}
		transcribeIdleFixture(t, worker, audio)
		if len(*created) != 2 || (*created)[0].frames != 48000 || (*created)[1].frames != 24000 {
			t.Fatal("reload did not transcribe every chunk")
		}
		worker.Close()
		worker.Close()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if (*created)[1].closes.Load() != 1 || worker.idleTimer != nil {
			t.Fatal("shutdown left native resources or timer")
		}
		if _, err := worker.Transcribe(context.Background(), audio); err == nil {
			t.Fatal("closed worker accepted work")
		}
	})
}

func setIdleTimeout(t *testing.T, worker *Parakeet, seconds int) {
	t.Helper()
	if _, err := worker.settings.Update(func(cfg *config.Config) error {
		cfg.Speech.IdleTimeoutSec = seconds
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker.RefreshIdleTimeout()
}

func TestParakeetLiveIdleTimeoutAndExpiredCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		worker, audio, created := idleWorker(t)
		defer worker.Close()
		transcribeIdleFixture(t, worker, audio)
		// Hold native ownership until the old callback is already waiting for it.
		worker.serial <- struct{}{}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		setIdleTimeout(t, worker, 5)
		<-worker.serial
		synctest.Wait()
		if (*created)[0].closes.Load() != 0 {
			t.Fatal("expired old callback ignored the extended timeout")
		}
		time.Sleep(time.Second)
		setIdleTimeout(t, worker, 1)
		synctest.Wait()
		if (*created)[0].closes.Load() != 1 {
			t.Fatal("shortened timeout did not expire from last completion")
		}
		transcribeIdleFixture(t, worker, audio)
		if len(*created) != 2 {
			t.Fatal("live change prevented reload")
		}
	})
}

func TestParakeetActiveTranscriptionAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			worker, audio, created := idleWorker(t)
			defer worker.Close()
			transcribeIdleFixture(t, worker, audio)
			release := make(chan struct{})
			(*created)[0].transcribe = func() error { <-release; return nil }
			result := make(chan error, 1)
			go func() { _, err := worker.Transcribe(context.Background(), audio); result <- err }()
			synctest.Wait()
			time.Sleep(5 * time.Second)
			setIdleTimeout(t, worker, 1)
			synctest.Wait()
			if (*created)[0].closes.Load() != 0 {
				t.Fatal("active recognizer closed")
			}
			closed := make(chan struct{})
			if shutdown {
				go func() { worker.Close(); close(closed) }()
				synctest.Wait()
				select {
				case <-closed:
					t.Fatal("shutdown did not wait for native work")
				default:
				}
			}
			close(release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if !shutdown {
				if (*created)[0].closes.Load() != 0 {
					t.Fatal("timeout measured model lifetime instead of idle time")
				}
				time.Sleep(time.Second)
				synctest.Wait()
			} else {
				<-closed
			}
			if (*created)[0].closes.Load() != 1 || (*created)[0].frames != 48000 {
				t.Fatal("active completion/cleanup lost frames or resources")
			}
		})
	}
}

func TestParakeetIdleCleanupAfterFailureOrCancellation(t *testing.T) {
	for _, kind := range []string{"recognition", "decode", "cancel", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				worker, audio, created := idleWorker(t)
				defer worker.Close()
				transcribeIdleFixture(t, worker, audio)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch kind {
				case "recognition":
					(*created)[0].transcribe = func() error { return errors.New("recognition failed") }
				case "decode":
					worker.openAudio = func(string) (audioDecoder, error) { return nil, errors.New("decode failed") }
				case "cancel":
					(*created)[0].transcribe = func() error { cancel(); return nil }
				case "replacement":
					_, err := worker.settings.Update(func(cfg *config.Config) error { cfg.Speech.NumThreads++; return nil })
					if err != nil {
						t.Fatal(err)
					}
					worker.newRecognizer = func(parakeetFiles, int) (speechRecognizer, error) { return nil, errors.New("load failed") }
				}
				if _, err := worker.Transcribe(ctx, audio); err == nil {
					t.Fatal("failure was hidden")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if (*created)[0].closes.Load() != 1 || worker.cached != nil {
					t.Fatal("failed request retained resources")
				}
			})
		})
	}
}

func TestParakeetReplacementRejectsExpiredCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		worker, audio, created := idleWorker(t)
		defer worker.Close()
		transcribeIdleFixture(t, worker, audio)
		if _, err := worker.settings.Update(func(cfg *config.Config) error { cfg.Speech.NumThreads++; return nil }); err != nil {
			t.Fatal(err)
		}
		// Queue replacement ahead of the expiring callback at the native boundary.
		worker.serial <- struct{}{}
		result := make(chan error, 1)
		go func() { _, err := worker.Transcribe(context.Background(), audio); result <- err }()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		<-worker.serial
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(*created) != 2 || (*created)[0].closes.Load() != 1 || (*created)[1].closes.Load() != 0 || (*created)[1].frames != 24000 {
			t.Fatal("expired callback closed replacement or lost input")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if (*created)[1].closes.Load() != 1 {
			t.Fatal("replacement not unloaded")
		}
	})
}
