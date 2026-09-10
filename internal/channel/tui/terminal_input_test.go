package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestTerminalFramingAcrossEveryPacketBoundary(t *testing.T) {
	packets := []string{"\x1b[<0;7;3M", "\x1b[<32;17;9M", "\x1b[<0;17;9m", "\x1b[<68;5;3M", "\x1b[M !!", "\x1b[1;6D", "\x1b[I", "\x1b[O", "界e\u0301👩🏽‍💻"}
	for _, packet := range packets {
		for split := 1; split < len(packet); split++ {
			f := terminalFrames{}
			var result []byte
			for _, part := range []string{packet[:split], packet[split:]} {
				f.pending = append(f.pending, part...)
				for {
					next := f.next(256, false)
					if next == nil {
						break
					}
					result = append(result, next...)
				}
			}
			result = append(result, f.next(256, true)...)
			if string(result) != packet || len(f.pending) != 0 {
				t.Fatalf("split %d %q => %q pending=%q", split, packet, result, f.pending)
			}
		}
	}
	coalesced := strings.Join(packets, "")
	f := terminalFrames{pending: []byte(coalesced)}
	var result []byte
	for {
		next := f.next(256, false)
		if next == nil {
			break
		}
		result = append(result, next...)
	}
	if string(result) != coalesced {
		t.Fatal("coalesced protocol changed")
	}
}

func TestKeyboardFramesRemainAtomic(t *testing.T) {
	// Representative shapes from Tea's pinned key table, including keys that
	// violate standard CSI grammar and their Escape-prefixed Alt variants.
	keys := []string{
		"\x1b[7$", "\x1b[8$", "\x1b[[A", "\x1b[[E", "\x1b[OA", "\x1b[OD",
		"\x1b[D", "\x1b[1;2H", "\x1b[1;5D", "\x1b[1;6F", "\x1bOP", "\x1bOD",
		"\x1b[5^", "\x1b[6;5~", "\x1b[7@", "\x1b[23~", "\x1b[1;2P", "\x1b[19;2~",
	}
	for _, sequence := range keys {
		keys = append(keys, "\x1b"+sequence)
	}
	keys = append(keys, "\x1b[Oa", "\x1b[1;3D", "\x1b[1;8H", "\x1b[24;3~")
	for _, sequence := range keys {
		packet := sequence + "X"
		for split := 0; split <= len(packet); split++ {
			f := terminalFrames{}
			var got []string
			for _, chunk := range []string{packet[:split], packet[split:]} {
				f.pending = append(f.pending, chunk...)
				for frame := f.next(256, false); frame != nil; frame = f.next(256, false) {
					got = append(got, string(frame))
				}
			}
			if len(got) != 2 || got[0] != sequence || got[1] != "X" || len(f.pending) != 0 {
				t.Fatalf("key %q split %d: frames=%q pending=%q", sequence, split, got, f.pending)
			}
		}
	}
	for _, sequence := range []string{"\x1b\x1b", "\x1b[O"} {
		f := terminalFrames{pending: []byte(sequence)}
		if f.next(256, false) != nil || string(f.next(256, true)) != sequence {
			t.Fatalf("ambiguous prefix %q did not wait then expire", sequence)
		}
	}
	// Unknown Alt-prefixed CSI stays a single unprefixed control frame for
	// Tea to ignore; malformed reports are discarded, never retyped.
	f := terminalFrames{pending: []byte("\x1b\x1b[99~\x1b\x1b[\x00A\x1b[[Z\x1b\x1b]52;c;ignored\asafe")}
	if string(f.next(256, false)) != "\x1b[99~" {
		t.Fatal("unknown control kept an unsafe Alt prefix")
	}
	var got strings.Builder
	for frame := f.next(256, false); frame != nil; frame = f.next(256, false) {
		got.Write(frame)
	}
	if got.String() != "safe" {
		t.Fatalf("unsupported reports leaked: %q", got.String())
	}
}

func TestPasteFramingAndUnsupportedControls(t *testing.T) {
	payload := "literal [<64;1;1M and \x1b[<0;2;2M\n\t界" + strings.Repeat("x", 600)
	packet := "\x1b[200~" + payload + "\x1b[201~"
	f := terminalFrames{}
	var result []byte
	for _, b := range []byte(packet) {
		f.pending = append(f.pending, b)
		for {
			next := f.next(256, false)
			if next == nil {
				break
			}
			result = append(result, next...)
		}
	}
	if string(result) != packet || f.paste {
		t.Fatalf("paste interpreted protocol: %q", result)
	}
	for _, packet := range []string{"\x1b[<;1;2M", "\x1b[<99999999999999999999999;1;2M", "\x1b[<0;0;2M", "\x1b[<" + strings.Repeat("1", 200) + ";2;3M", "\x1b]52;c;ignored\a", "\x1bPignored\x1b\\"} {
		f := terminalFrames{pending: []byte(packet + "safe")}
		var out []byte
		for {
			next := f.next(256, false)
			if next == nil {
				break
			}
			out = append(out, next...)
		}
		if string(out) != "safe" {
			t.Fatalf("unsupported %q leaked: %q", packet, out)
		}
	}
	f = terminalFrames{pending: []byte{27}}
	if !bytes.Equal(f.next(256, true), []byte{27}) {
		t.Fatal("Escape latency")
	}
	f.pending = []byte("[<0;7;3M")
	if got := string(f.next(256, false)); got != "\x1b[<0;7;3M" {
		t.Fatalf("delayed ESC prefix leaked: %q", got)
	}
}

func TestMalformedControlResynchronizationAcrossReads(t *testing.T) {
	for _, control := range []string{
		"\x1b[\x00A", "\x1b[ 1A", "\x1bO1;5D", "\x1bOX",
		"\x1b[" + strings.Repeat("1", 200) + "\x1b[<32;7;3M",
		"\x1bO" + strings.Repeat("1", 200) + "\x1b[<0;7;3m",
	} {
		for split := 1; split < len(control); split++ {
			f := terminalFrames{}
			var result strings.Builder
			for _, part := range []string{control[:split], control[split:] + "safe"} {
				f.pending = append(f.pending, part...)
				for {
					frame := f.next(256, false)
					if frame == nil {
						break
					}
					if !bytes.HasPrefix(frame, []byte("\x1b[<")) {
						result.Write(frame)
					}
				}
			}
			if result.String() != "safe" || len(f.pending) != 0 {
				t.Fatalf("split %d of %q leaked %q, pending=%q", split, control, result.String(), f.pending)
			}
		}
	}
}

func TestTerminalCopyReturnFrames(t *testing.T) {
	keys := []string{"\x1b", "\r", "\n", " ", "\x7f", "\x08", "\t", "\x1b[3~", "\x1bOP", "\x1b[[E", "\x1b[17~", "\x1b[24~", "\x1b[34~", "\x1b[1;2P", "\x1b\x1b[23~"}
	for _, key := range keys {
		for split := 0; split <= len(key); split++ {
			f := terminalFrames{}
			var frames [][]byte
			for _, chunk := range []string{key[:split], key[split:]} {
				f.pending = append(f.pending, chunk...)
				for frame := f.next(256, false); frame != nil; frame = f.next(256, false) {
					frames = append(frames, frame)
				}
			}
			if frame := f.next(256, true); frame != nil {
				frames = append(frames, frame)
			}
			if len(frames) != 1 || !terminalCopyExit(frames[0]) {
				t.Fatalf("return key %q split %d: %q", key, split, frames)
			}
		}
	}
	for _, frame := range []string{"", "x", "\x03", "\x1b[A", "\x1b[6~", "\x1b[I", "\x1b[O", "\x1b[<64;3;3M", "\x1b[M !!", "\x1b[200~ \r\x1bOP\x1b[201~"} {
		if terminalCopyExit([]byte(frame)) {
			t.Fatalf("non-return input %q", frame)
		}
	}
}
