package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// ReadDisplayEntry reads one complete append-only record with bounded body
// text and exact metadata. Incomplete records must be retried at the same offset.
func ReadDisplayEntry(reader *bufio.Reader) (Entry, int, error) {
	entry, size, err := readProjectedEntry(reader, true)
	if err == nil && entry.IsContentExcerpt() {
		entry.Content += "\n[Body excerpt; inspect the full history file for complete text.]"
	}
	return entry, size, err
}

// Correlation checks need exact metadata, but body text is context. Project long
// body strings before decoding, validating even the omitted string suffix.
func readCorrelationEntry(reader *bufio.Reader) (Entry, int, error) {
	return readProjectedEntry(reader, false)
}

func readProjectedEntry(reader *bufio.Reader, requireNewline bool) (Entry, int, error) {
	var raw []byte
	depth, size := 0, 0
	key, expectKey, omitted := "", false, false
	read := func() (byte, error) {
		b, err := reader.ReadByte()
		if err == nil {
			size++
		}
		return b, err
	}
	for {
		b, err := read()
		if err == io.EOF && size == 0 {
			return Entry{}, 0, io.EOF
		}
		if err != nil && err != io.EOF {
			return Entry{}, size, err
		}
		if err == io.EOF || b == '\n' {
			if len(bytes.TrimSpace(raw)) == 0 {
				if err == io.EOF {
					return Entry{}, 0, io.EOF
				}
				raw = raw[:0]
				continue
			}
			if err == io.EOF && requireNewline {
				return Entry{}, size, io.ErrUnexpectedEOF
			}
			var entry Entry
			if json.Unmarshal(raw, &entry) != nil {
				return Entry{}, size, ErrRecoveryCorrupt
			}
			entry.compact = omitted
			return entry, size, nil
		}
		if b == '"' {
			start := len(raw)
			raw = append(raw, b)
			body := depth == 1 && !expectKey && (key == "content" || key == "final_text")
			kept := 0
			for {
				b, err = read()
				if err != nil || b < 0x20 {
					return Entry{}, size, ErrRecoveryCorrupt
				}
				if b == '"' {
					raw = append(raw, b)
					break
				}
				unit := []byte{b}
				if b == '\\' {
					escape, err := read()
					if err != nil {
						return Entry{}, size, ErrRecoveryCorrupt
					}
					unit = append(unit, escape)
					if escape == 'u' {
						for i := 0; i < 4; i++ {
							hex, err := read()
							if err != nil || !(hex >= '0' && hex <= '9' || hex >= 'a' && hex <= 'f' || hex >= 'A' && hex <= 'F') {
								return Entry{}, size, ErrRecoveryCorrupt
							}
							unit = append(unit, hex)
						}
					} else if escape != '"' && escape != '\\' && escape != '/' && escape != 'b' && escape != 'f' && escape != 'n' && escape != 'r' && escape != 't' {
						return Entry{}, size, ErrRecoveryCorrupt
					}
				} else if b >= utf8.RuneSelf {
					// Keep a complete UTF-8 rune at the excerpt boundary.
					for !utf8.FullRune(unit) {
						next, err := read()
						if err != nil || next < 0x80 || next > 0xbf {
							return Entry{}, size, ErrRecoveryCorrupt
						}
						unit = append(unit, next)
					}
				}
				if !body || kept < 32*1024 {
					raw = append(raw, unit...)
					kept += len(unit)
				} else {
					omitted = true
				}
				if len(raw) > maxHistoryEntryBytes {
					return Entry{}, size, ErrRecoveryByteBound
				}
			}
			if depth == 1 && expectKey {
				if json.Unmarshal(raw[start:], &key) != nil {
					return Entry{}, size, ErrRecoveryCorrupt
				}
			}
		} else {
			raw = append(raw, b)
			switch b {
			case '{', '[':
				depth++
				if depth == 1 {
					expectKey = true
				}
			case '}', ']':
				depth--
			case ':':
				if depth == 1 {
					expectKey = false
				}
			case ',':
				if depth == 1 {
					expectKey = true
				}
			}
		}
		if len(raw) > maxHistoryEntryBytes {
			return Entry{}, size, ErrRecoveryByteBound
		}
	}
}

// IsContentExcerpt reports that full body text must be read from durable history.
func (e Entry) IsContentExcerpt() bool { return e.compact }
