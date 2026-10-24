package channel

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
)

const progressLimit = 3000
const progressInterval = time.Second

// ProgressStreams coalesces user-facing assistant deltas into one editable,
// bounded preview per response. It never queues tool, status or background events.
// Native send callbacks must recheck authorization and honor cancellation.
type ProgressStreams struct {
	Mode    func() string
	mu      sync.Mutex
	streams map[string]*progressStream
}

type progressStream struct {
	mu         sync.Mutex
	text, id   string
	dirty      bool
	deltaBytes int
	cancel     context.CancelFunc
	done       chan struct{}
}

type ProgressSend func(context.Context, string, string) (string, error)

func (p *ProgressStreams) enabled() bool {
	return p.Mode != nil && core.ShowProgress(p.Mode(), "remote")
}

func progressTail(text string) string {
	// Bound before conversion as a delta itself can exceed the preview window.
	if len(text) > progressLimit*4 {
		text = text[len(text)-progressLimit*4:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	runes := []rune(text)
	if len(runes) > progressLimit {
		return "…" + string(runes[len(runes)-progressLimit+1:])
	}
	return text
}

// Push is nonblocking with respect to transport I/O, including a slow edit.
func (p *ProgressStreams) Push(ctx context.Context, key string, event core.Event, send ProgressSend) {
	if event.Local || (event.Kind != core.EventDelta && !((event.Kind == core.EventFinal || event.Kind == core.EventError) && event.Continues)) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stream := p.streams[key]
	if ctx.Err() != nil {
		return
	}
	if stream == nil {
		if p.streams == nil {
			p.streams = make(map[string]*progressStream)
		}
		runCtx, cancel := context.WithCancel(ctx)
		stream = &progressStream{cancel: cancel, done: make(chan struct{})}
		p.streams[key] = stream
		go p.run(runCtx, key, stream, send)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	enabled := p.enabled()
	if !enabled {
		stream.text, stream.dirty = "", false
	}
	if event.Kind == core.EventError {
		stream.deltaBytes = 0
		return
	}
	if event.Kind == core.EventFinal {
		text := UnstreamedFinal(event.Text, event.FinalText, stream.deltaBytes)
		if enabled && text != "" {
			if stream.deltaBytes == 0 {
				stream.text = ""
			}
			stream.text, stream.dirty = progressTail(stream.text+text), true
		}
		stream.deltaBytes = 0
		return
	}
	stream.deltaBytes += len(event.Text)
	if !enabled {
		return
	}
	stream.text = progressTail(stream.text + progressTail(event.Text))
	stream.dirty = true
}

func (p *ProgressStreams) run(ctx context.Context, key string, stream *progressStream, send ProgressSend) {
	defer close(stream.done)
	defer func() {
		p.mu.Lock()
		if p.streams[key] == stream {
			delete(p.streams, key)
		}
		p.mu.Unlock()
	}()
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		stream.mu.Lock()
		if !p.enabled() {
			stream.text, stream.dirty = "", false
		}
		text, id, dirty := stream.text, stream.id, stream.dirty
		stream.dirty = false
		stream.mu.Unlock()
		if !dirty || text == "" || ctx.Err() != nil || !p.enabled() {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		nextID, err := send(sendCtx, id, text)
		cancel()
		if err == nil && nextID != "" {
			stream.mu.Lock()
			stream.id = nextID
			stream.mu.Unlock()
		}
	}
}

// UnstreamedFinal excludes deltas already observed, including hidden ones,
// from a cumulative continuing snapshot. A new final item still gets through.
func UnstreamedFinal(text string, final *string, deltaBytes int) string {
	start := 0
	if final != nil && strings.HasSuffix(text, *final) {
		start = len(text) - len(*final)
	}
	return text[min(len(text), max(start, deltaBytes)):]
}

// Stop fences queued/in-flight progress before the terminal's independent send.
// A failed or cancelled preview never prevents the ordinary final delivery.
func (p *ProgressStreams) Stop(key string) string {
	p.mu.Lock()
	stream := p.streams[key]
	delete(p.streams, key)
	p.mu.Unlock()
	if stream == nil {
		return ""
	}
	stream.cancel()
	<-stream.done
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.id
}
