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

// watchTaskNotifications follows only runtime-authored proactive assistant
// entries. Ordinary turn history is rendered by the TUI's response stream and
// is intentionally ignored here.
func watchTaskNotifications(ctx context.Context, path string) <-chan channel.Notification {
	output := make(chan channel.Notification, 16)
	var offset int64
	if info, err := os.Stat(path); err == nil {
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
				offset += int64(len(scanner.Bytes()) + 1)
				var entry history.Entry
				if json.Unmarshal(scanner.Bytes(), &entry) == nil && (entry.Role == "assistant" || entry.Role == "notification_pending") && entry.Sender == "Spy" {
					select {
					case output <- channel.Notification{ID: entry.EventID, Text: entry.Content}:
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
