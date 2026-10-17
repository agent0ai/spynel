package history

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AppendOnce commits a consumer effect and its receipt in the same history
// record. The elected consumer must hold its exact ownership fence through
// this call; Store's mutex alone does not serialize different processes.
// Replaying a matching receipt confirms durability without another append.
func (s *Store) AppendOnce(channel, conversation, receipt string, entry Entry) (bool, error) {
	if receipt == "" || len(receipt) > 256 || !utf8.ValidString(receipt) || strings.IndexFunc(receipt, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false, errors.New("invalid history receipt identity")
	}
	if entry.ReceiptID != "" || entry.ReceiptHash != "" || entry.Role != "assistant" && entry.Role != "error" && entry.Role != "correlation" {
		return false, errors.New("invalid receipted history effect")
	}
	// Retry timestamps vary; every other persisted effect field must agree.
	canonical := entry
	canonical.At = time.Time{}
	data, err := json.Marshal(canonical)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(data)
	entry.ReceiptID, entry.ReceiptHash = receipt, hex.EncodeToString(digest[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.OpenFile(s.Path(channel, conversation), os.O_RDWR, 0)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil {
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return false, err
		}
		if info.Size() > 0 {
			var tail [1]byte
			if _, err := file.ReadAt(tail[:], info.Size()-1); err != nil {
				return false, err
			}
			if tail[0] != '\n' {
				return false, ErrRecoveryCorrupt
			}
		}
		// shortcut: receipt lookup scans retained history, index only if measured replay cost requires it.
		reader := bufio.NewReaderSize(file, historyReadChunk)
		found := false
		for {
			previous, _, err := readCorrelationEntry(reader)
			if err == io.EOF {
				break
			}
			if err != nil {
				return false, err
			}
			if previous.ReceiptID == receipt {
				if previous.ReceiptHash != entry.ReceiptHash {
					return false, errors.New("history receipt effect changed")
				}
				found = true
			}
		}
		if found {
			// A prior Sync may have failed after the complete record was written.
			return false, file.Sync()
		}
	}
	_, err = s.appendLocked(channel, conversation, entry)
	return err == nil, err
}
