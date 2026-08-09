package channel

import (
	"strings"
	"testing"
)

func TestErrorResponsePreservesCompleteTextWithoutIndentation(t *testing.T) {
	if got := ErrorResponse("first line\nsecond line"); got != "Error first line\nsecond line" {
		t.Fatalf("ErrorResponse() = %q", got)
	}
	if got := ErrorResponse(""); got != "Error The harness turn failed." {
		t.Fatalf("empty ErrorResponse() = %q", got)
	}
}

func TestReplyReferenceNormalizesAndTruncatesUnicodeSafely(t *testing.T) {
	preview := "  first\n\tsecond  " + string(make([]rune, 0))
	if got := ReplyReference(" 123 ", preview); got != "123 first second" {
		t.Fatalf("ReplyReference() = %q", got)
	}
	long := "🙂" + string([]rune("界界界"))
	for len([]rune(long)) < 101 {
		long += "界"
	}
	got := ReplyReference("id", long)
	if runes := []rune(strings.TrimPrefix(got, "id ")); len(runes) != replyPreviewRunes || runes[len(runes)-1] != '…' {
		t.Fatalf("truncated reply preview = %q (%d runes)", got, len(runes))
	}
	if got := ReplyReference("id-only", ""); got != "id-only" {
		t.Fatalf("ID-only reply = %q", got)
	}
	if got := ReplyReference("", "hidden"); got != "" {
		t.Fatalf("missing ID exposed preview %q", got)
	}
}
