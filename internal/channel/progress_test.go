package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
)

func TestProgressCoalescesBoundsTogglesAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mode atomic.Value
		mode.Store("on")
		p := ProgressStreams{Mode: func() string { return mode.Load().(string) }}
		var sentMu sync.Mutex
		var sent []string
		snapshot := func() []string { sentMu.Lock(); defer sentMu.Unlock(); return append([]string(nil), sent...) }
		send := func(_ context.Context, id, text string) (string, error) {
			sentMu.Lock()
			sent = append(sent, text)
			sentMu.Unlock()
			return "native", nil
		}
		push := func(text string) { p.Push(t.Context(), "one", core.Event{Kind: core.EventDelta, Text: text}, send) }
		for range 1000 {
			push("界")
		}
		synctest.Wait()
		if sent := snapshot(); len(sent) != 0 {
			t.Fatal("per-token sends")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if sent := snapshot(); len(sent) != 1 || !strings.Contains(sent[0], "界") {
			t.Fatal(sent)
		}
		mode.Store("off")
		push("hidden")
		time.Sleep(time.Second)
		synctest.Wait()
		mode.Store("on")
		push(strings.Repeat("文", 100000))
		time.Sleep(time.Second)
		synctest.Wait()
		if sent := snapshot(); len(sent) != 2 || len([]rune(sent[1])) > progressLimit+20 || !utf8.ValidString(sent[1]) || strings.Contains(sent[1], "hidden") {
			t.Fatal("unbounded or replayed preview")
		}
		push("pending")
		if id := p.Stop("one"); id != "native" {
			t.Fatal(id)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if sent := snapshot(); len(sent) != 2 || len(p.streams) != 0 {
			t.Fatal("send after terminal")
		}
	})
}

func TestProgressSlowFailedAndCancelledSendDoesNotBlockInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := ProgressStreams{Mode: func() string { return "on" }}
		entered := make(chan struct{})
		p.Push(t.Context(), "one", core.Event{Kind: core.EventDelta, Text: "progress"}, func(ctx context.Context, _, _ string) (string, error) {
			close(entered)
			<-ctx.Done()
			return "", errors.New("cancelled preview")
		})
		synctest.Wait()
		time.Sleep(time.Second)
		<-entered
		p.Push(t.Context(), "one", core.Event{Kind: core.EventDelta, Text: "next"}, nil)
		if id := p.Stop("one"); id != "" {
			t.Fatal("failed progress acquired a final identity")
		}
		synctest.Wait()
	})
}

func TestProgressNonStreamingContinuationAfterToggle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mode atomic.Value
		mode.Store("on")
		p := ProgressStreams{Mode: func() string { return mode.Load().(string) }}
		sent := make(chan string, 2)
		send := func(_ context.Context, _, text string) (string, error) { sent <- text; return "native", nil }
		p.Push(t.Context(), "one", core.Event{Kind: core.EventDelta, Text: "old"}, send)
		mode.Store("off")
		p.Push(t.Context(), "one", core.Event{Kind: core.EventFinal, Text: "hidden", Continues: true}, send)
		mode.Store("on")
		visible := "new continuation"
		p.Push(t.Context(), "one", core.Event{Kind: core.EventFinal, Text: "hidden\n" + visible, FinalText: &visible, Continues: true}, send)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case got := <-sent:
			if got != visible {
				t.Fatal(got)
			}
		default:
			t.Fatal("continuation lost")
		}
		p.Stop("one")
	})
}

func TestProgressCancellationFencesReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := ProgressStreams{Mode: func() string { return "on" }}
		ctx, cancel := context.WithCancel(t.Context())
		sent := make(chan string, 2)
		send := func(_ context.Context, id, text string) (string, error) { sent <- id + text; return "preview", nil }
		p.Push(ctx, "one", core.Event{Kind: core.EventDelta, Text: "old queued"}, send)
		cancel()
		synctest.Wait()
		p.Push(t.Context(), "one", core.Event{Kind: core.EventDelta, Text: "new generation"}, send)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		p.Stop("one")
		select {
		case got := <-sent:
			if got != "new generation" {
				t.Fatal(got)
			}
		default:
			t.Fatal("new progress lost")
		}
		select {
		case extra := <-sent:
			t.Fatal("old generation replayed", extra)
		default:
		}
	})
}
