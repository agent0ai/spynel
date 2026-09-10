package tui

import (
	"bytes"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// terminalFrames owns byte-stream framing, before Bubble Tea decodes events.
// A short OS read is not an event boundary. Emit one complete escape/rune
// frame per read, including focus reports (Tea requires those to be isolated).
// Paste payloads bypass escape interpretation entirely.
// ponytail: Tea buffers an entire bracketed paste; a hard byte cap would
// require replacing that decoder, rather than splitting an atomic paste.
type terminalFrames struct {
	discardEscape bool
	pending       []byte
	paste         bool
	discard       byte
	escaped       bool
}

// Bubble Tea 1.3.10 adds an Escape-prefixed Alt variant only to these key
// shapes. Unknown CSI reports must stay unprefixed: Tea's unknown-CSI matcher
// cannot consume a leading Alt Escape and would expose their bytes as text.
// This records frame shapes only; Tea remains the key-to-event decoder.
var altKeyboardFrame = regexp.MustCompile(`^\x1b(?:O[ABCDPQRS]|\[(?:[ABCDZFHabcd]|O[ABCD]|1;[256][ABCDHF]|[1-6]~|[56](?:\^|;5~)|[78][~^$@]|\[[A-E]|(?:11|12|13|14|15|17|18|19|20|21|23|24|25|26|28|29|31|32|33|34)~|1;2[PQRS]|(?:15|17|18|19);2~))$`)

func (f *terminalFrames) next(limit int, escapeExpired bool) []byte {
	b := f.pending
	if len(b) == 0 || limit < utf8.UTFMax {
		return nil
	}
	take := func(n int) []byte { out := append([]byte(nil), b[:n]...); f.pending = b[n:]; return out }
	if f.paste {
		const end = "\x1b[201~"
		if bytes.HasPrefix(b, []byte(end)) {
			f.paste = false
			return take(len(end))
		}
		if bytes.HasPrefix([]byte(end), b) {
			return nil
		}
		n := min(len(b), limit-1)
		if i := bytes.IndexByte(b[:n], '\x1b'); i > 0 {
			n = i
		} else if i == 0 {
			n = 1
		}
		return take(n)
	}
	if f.discard != 0 {
		for i, c := range b {
			if f.discard == '[' && c == 27 {
				f.pending, f.discard = b[i:], 0
				return f.next(limit, false)
			}
			if f.discard == '[' && c >= 0x40 && c <= 0x7e || f.discard != '[' && (c == 7 || c == '\\' && f.discardEscape) {
				f.pending = b[i+1:]
				f.discard = 0
				f.discardEscape = false
				return f.next(limit, false)
			}
			f.discardEscape = c == 27
		}
		f.pending = nil
		return nil
	}
	// A delayed CSI prefix following a lone Escape still belongs to the
	// protocol, even when the Escape key's latency budget already expired.
	if f.escaped {
		f.escaped = false
		if b[0] == '[' {
			f.pending = append([]byte{27}, b...)
			b = f.pending
		}
	}
	if b[0] != 27 {
		if !utf8.FullRune(b) {
			return nil
		}
		_, n := utf8.DecodeRune(b)
		return take(n)
	}
	if len(b) == 1 {
		if escapeExpired {
			f.escaped = true
			return take(1)
		}
		return nil
	}
	alt := false
	if b[1] == 27 {
		if len(b) == 2 && !escapeExpired {
			return nil // Alt+Escape may still grow into an Alt+arrow/key.
		}
		if len(b) < 3 || !strings.ContainsRune("[O]P^_", rune(b[2])) {
			return take(2)
		}
		alt, b = true, b[1:]
	}
	keyFrame := func(frame []byte) []byte {
		if alt && altKeyboardFrame.Match(frame) {
			return append([]byte{27}, frame...)
		}
		return frame
	}
	switch b[1] {
	case '[':
		// These supported keys predate CSI final-byte grammar. ESC [ O is
		// also focus-out, so defer that ambiguous prefix until input or timeout.
		if len(b) >= 3 && (b[2] == '[' || b[2] == 'O') {
			if len(b) == 3 {
				if b[2] == 'O' && escapeExpired {
					return take(3)
				}
				return nil
			}
			if b[2] == '[' && b[3] >= 'A' && b[3] <= 'E' || b[2] == 'O' && strings.ContainsRune("ABCDabcd", rune(b[3])) {
				return keyFrame(take(4))
			}
			if b[2] == 'O' {
				return take(3) // Focus followed by a separate event.
			}
			// Discard an unsupported console-key report as one frame.
			take(3)
			f.discard = '['
			return f.next(limit, false)
		}
		if len(b) >= 4 && (b[2] == '7' || b[2] == '8') && b[3] == '$' {
			return keyFrame(take(4))
		}
		if len(b) >= 3 && b[2] == 'M' {
			end := 3
			for range 3 {
				if end >= len(b) || !utf8.FullRune(b[end:]) {
					return nil
				}
				_, n := utf8.DecodeRune(b[end:])
				end += n
			}
			frame := take(end)
			if end != 6 {
				return f.next(limit, false)
			} // unsupported UTF-8 X10
			return frame
		}
		for i := 2; i < len(b); i++ {
			if b[i] == 27 {
				f.pending = b[i:]
				return f.next(limit, false)
			}
			if b[i] < 0x40 || b[i] > 0x7e {
				continue
			}
			frame := take(i + 1)
			if bytes.Equal(frame, []byte("\x1b[200~")) {
				f.paste = true
			}
			if len(frame) > 128 {
				return f.next(limit, false)
			}
			// Tea recognizes only syntactically valid unknown CSI sequences.
			// Reject malformed parameters here so its Alt/rune fallback cannot
			// turn an unsupported control report into ordinary composer text.
			intermediate := false
			for _, c := range frame[2 : len(frame)-1] {
				if c >= 0x20 && c <= 0x2f {
					intermediate = true
				} else if c < 0x30 || c > 0x3f || intermediate {
					return f.next(limit, false)
				}
			}
			if frame[2] == '<' {
				parts := strings.Split(string(frame[3:len(frame)-1]), ";")
				valid := len(parts) == 3 && (b[i] == 'M' || b[i] == 'm')
				for j, part := range parts {
					n, err := strconv.Atoi(part)
					valid = valid && err == nil && n >= 0 && n <= 1<<20 && (j == 0 || n > 0)
				}
				if !valid {
					return f.next(limit, false)
				}
			}
			// Normalize supported extended encodings to existing Tea bindings.
			switch string(frame) {
			case "\x1b[2;2~":
				return []byte{22}
			case "\x1b[3;5~":
				return []byte("\x1bd")
			case "\x1b[127;5u", "\x1b[8;5u", "\x1b[27;5;127~":
				return []byte("\x1b\x7f")
			case "\x1b[13;2u", "\x1b[27;2;13~":
				return []byte{10}
			}
			return keyFrame(frame)
		}
		if len(b) > 128 {
			f.pending = nil
			f.discard = '['
		}
		return nil
	case ']', 'P', '^', '_':
		// OSC/DCS/APC replies are not keyboard input. Discard through ST/BEL.
		f.pending = b[2:]
		f.discard = b[1]
		f.discardEscape = false
		return f.next(limit, false)
	case 'O':
		for i := 2; i < len(b); i++ {
			if b[i] == 27 {
				f.pending = b[i:]
				return f.next(limit, false)
			}
			if b[i] >= 0x40 && b[i] <= 0x7e {
				frame := take(i + 1)
				if len(frame) == 3 && strings.ContainsRune("ABCDPQRS", rune(frame[2])) {
					return keyFrame(frame)
				}
				return f.next(limit, false)
			}
		}
		if len(b) > 128 {
			f.pending, f.discard = nil, '['
		}
		return nil
	default:
		if !utf8.FullRune(b[1:]) {
			return nil
		}
		_, n := utf8.DecodeRune(b[1:])
		return take(n + 1)
	}
}

var _ io.Reader = (*terminalInput)(nil)
