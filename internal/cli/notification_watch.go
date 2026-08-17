package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/history"
)

// watchTaskNotifications follows runtime-authored proactive assistant entries
// plus stalled-message recovery terminals. Ordinary requested turns are
// rendered by their response stream and remain intentionally ignored here.
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
			_, _ = file.Seek(offset, io.SeekStart)
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
			for scanner.Scan() {
				// Scanner returns an unterminated final token at EOF. Do not
				// advance past it: a concurrent history append may still be
				// completing that JSONL record, and the next poll must retry it.
				nextOffset := offset + int64(len(scanner.Bytes())+1)
				if nextOffset > info.Size() {
					break
				}
				offset = nextOffset
				var entry history.Entry
				if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Sender == "Spy" &&
					(entry.Role == "notification_pending" || entry.Role == "assistant" || entry.Recovery) {
					select {
					case output <- channel.Notification{ID: entry.EventID, Text: entry.Content, Recovery: entry.Recovery, Error: entry.Recovery && entry.Role == "error"}:
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
