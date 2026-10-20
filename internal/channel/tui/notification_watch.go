package tui

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/history"
)

// watchTaskNotifications follows runtime-authored proactive assistant entries
// plus stalled-message and retained-worker conversation results. Requested
// controller turns remain stream-owned; worker request copies are reconciled
// by their private request/response identities in the model.
func watchTaskNotifications(ctx context.Context, path string, initialOffset ...int64) <-chan channel.Notification {
	output := make(chan channel.Notification, 16)
	var offset int64
	if len(initialOffset) > 0 {
		offset = initialOffset[0]
	} else if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	go func() {
		defer close(output)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			file, err := os.Open(path)
			if err != nil {
				continue
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				continue
			}
			if info.Size() < offset {
				offset = 0
			}
			reader := bufio.NewReader(io.NewSectionReader(file, offset, info.Size()-offset))
			for {
				entry, size, err := history.ReadDisplayEntry(reader)
				if err != nil {
					break
				}
				offset += int64(size)
				if entry.Sender == "Spy" &&
					(entry.Role == "notification_pending" || entry.Role == "assistant" || entry.Recovery) {
					select {
					case output <- channel.Notification{ID: entry.EventID, RequestID: entry.SourceMessageID,
						ResponseID: strings.TrimSuffix(strings.TrimSuffix(entry.ReceiptID, ":result"), ":partial"),
						Text:       entry.Content, Recovery: entry.Recovery, Error: entry.Recovery && entry.Role == "error", Continues: entry.Continues}:
					case <-ctx.Done():
						file.Close()
						return
					}
				}
			}
			file.Close()
		}
	}()
	return output
}
