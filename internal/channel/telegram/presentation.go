package telegram

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
)

type responsePresentation struct {
	mu         sync.Mutex
	refs       int
	request    string
	replyTo    int64
	previewing bool
	prefixes   map[string]string
	cleanup    func() bool
	temporary  bool
}

// Serialize boundaries and terminal delivery per conversation, including their
// native calls. Timer I/O is joined by Stop before a boundary can be accepted.
func (b *Bot) lockResponse(ctx context.Context, conversation string, temporary bool) (*responsePresentation, func()) {
	b.responseMu.Lock()
	r := b.responses[conversation]
	if r == nil {
		if b.responses == nil {
			b.responses = make(map[string]*responsePresentation)
		}
		r = &responsePresentation{temporary: temporary}
		b.responses[conversation] = r
		cleanupCtx := ctx
		if temporary {
			cleanupCtx = context.WithoutCancel(ctx)
		}
		r.cleanup = b.responseCleanup(cleanupCtx, conversation, r)
	}
	r.refs++
	b.responseMu.Unlock()
	r.mu.Lock()
	if r.temporary && !temporary {
		r.cleanup()
		r.cleanup = b.responseCleanup(ctx, conversation, r)
		r.temporary = false
	}
	return r, func() { b.releaseResponse(conversation, r) }
}

func (b *Bot) responseCleanup(ctx context.Context, conversation string, r *responsePresentation) func() bool {
	return context.AfterFunc(ctx, func() {
		b.responseMu.Lock()
		if b.responses[conversation] != r {
			b.responseMu.Unlock()
			return
		}
		r.refs++
		b.responseMu.Unlock()
		r.mu.Lock()
		b.progress.Stop(conversation)
		r.request, r.previewing, r.prefixes = "", false, nil
		b.releaseResponse(conversation, r)
	})
}

func (b *Bot) releaseResponse(conversation string, r *responsePresentation) {
	b.responseMu.Lock()
	r.refs--
	if r.refs == 0 && r.request == "" && !r.previewing && len(r.prefixes) == 0 {
		delete(b.responses, conversation)
		r.cleanup()
	}
	b.responseMu.Unlock()
	r.mu.Unlock()
}

// freezePreview preserves only text confirmed by Telegram, so a later final can
// supply any deltas that had not reached the preview before an intervention.
func (b *Bot) freezePreview(conversation string, r *responsePresentation) {
	_, text := b.progress.StopWithText(conversation)
	if r.request != "" && text != "" && len(telegramChunks(text, 4096)) == 1 && !(strings.HasPrefix(text, "…") && len([]rune(text)) >= 3000) {
		if r.prefixes == nil {
			r.prefixes = make(map[string]string)
		}
		if len(r.prefixes[r.request])+len(text) <= 128*1024 && (len(r.prefixes) < 64 || r.prefixes[r.request] != "") {
			r.prefixes[r.request] += text
		}
	}
	r.previewing = false
}

func (b *Bot) presentEvent(ctx context.Context, conversation, chatID, eventID string, event core.Event, replyTo int64) error {
	r, unlock := b.lockResponse(ctx, conversation, false)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	if chatID == "" {
		return errors.New("missing Telegram delivery destination")
	}
	if event.Kind == core.EventAccepted {
		b.freezePreview(conversation, r)
		if r.request == "" {
			r.request = event.RequestID
		}
		r.replyTo = replyTo
		if r.replyTo == 0 {
			r.replyTo = sourceReplyID(chatID, event.RequestID)
		}
		return nil
	}
	terminal := event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError)
	if !terminal {
		if event.Local && event.Done && event.RequestID == r.request && !r.previewing {
			r.request = ""
		}
		if !event.Local && (event.Kind == core.EventDelta || event.Continues) {
			if event.RequestID != r.request {
				b.freezePreview(conversation, r)
				r.request = event.RequestID
			}
			r.previewing = true
			if r.replyTo > 0 {
				replyTo = r.replyTo
			} else if replyTo == 0 {
				replyTo = sourceReplyID(chatID, event.RequestID)
			}
			b.progress.Push(ctx, conversation, event, func(ctx context.Context, id, text string) (string, error) {
				if _, err := b.deliveryChatID(conversation); err != nil {
					return "", err
				}
				return b.sendProgress(ctx, chatID, id, text, replyTo)
			})
		}
		return nil
	}
	if event.ResponseID != "" {
		eventID = event.ResponseID
	}
	receipt := fmt.Sprintf("telegram:%x", sha256.Sum256([]byte(eventID)))
	if b.history != nil && eventID != "" && !event.Local {
		_, found, err := b.history.Receipt("telegram", conversation, receipt)
		if err != nil || found {
			return err
		}
	}
	previewID := ""
	if !event.Local && event.RequestID == r.request {
		previewID = b.progress.Stop(conversation)
		r.request, r.previewing = "", false
		if r.replyTo > 0 {
			replyTo = r.replyTo
		}
	} else {
		b.freezePreview(conversation, r)
		if event.Local && event.RequestID == r.request {
			r.request = ""
		}
	}
	if replyTo == 0 {
		replyTo = sourceReplyID(chatID, event.RequestID)
	}
	text := event.Text
	if event.Kind == core.EventFinal && event.FinalText != nil {
		text = *event.FinalText
	}
	if event.Kind == core.EventError {
		text = channel.ErrorResponse(text)
	} else if prefix := r.prefixes[event.RequestID]; prefix != "" && strings.HasPrefix(text, prefix) {
		text = strings.TrimPrefix(text, prefix)
	}
	delete(r.prefixes, event.RequestID)
	for _, attachment := range event.Attachments {
		if err := b.sendAttachment(ctx, chatID, attachment, replyTo); err != nil {
			return err
		}
		replyTo = 0
	}
	if _, err := b.sendWithIDs(ctx, chatID, text, replyTo, false, previewID); err != nil {
		return err
	}
	if b.history == nil || eventID == "" || event.Local {
		return nil
	}
	_, err := b.history.AppendOnce("telegram", conversation, receipt, history.Entry{Role: "correlation", Outcome: "telegram_response_sent"})
	return err
}

func sourceReplyID(chatID, source string) int64 {
	prefix := "telegram:" + chatID + ":"
	if !strings.HasPrefix(source, prefix) {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(source, prefix), 10, 64)
	return max(0, id)
}
