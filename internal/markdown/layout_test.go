package markdown

import (
	"strings"
	"testing"

	"github.com/agent0ai/spynel/internal/theme"
	"github.com/charmbracelet/x/ansi"
)

func TestLogicalLayoutPreservesHardBreaksAndWhitespace(t *testing.T) {
	source := "  first  line\n\n\t界e\u0301👩🏽‍💻 end  "
	for _, width := range []int{4, 9, 24, 80} {
		l := WrapLogical(source, width)
		if l.Text != source {
			t.Fatalf("width %d changed logical source %q", width, l.Text)
		}
		for _, row := range l.Rows {
			want := strings.ReplaceAll(string([]rune(source)[row.Start:row.End]), "\t", "    ")
			if got := ansi.Strip(row.ANSI); got != want {
				t.Fatalf("width %d row = %q want %q", width, got, want)
			}
			if ansi.StringWidth(row.ANSI) > width {
				t.Fatalf("row overflow: %q", row.ANSI)
			}
		}
	}
	l := TerminalLayout("A **bold** response with `inline` code.\nNext line.\n\n```go\n    x := 1  \n\n    y := 2\n```", 18, theme.Default())
	if strings.Contains(l.Text, "\x1b") || !strings.Contains(l.Text, "response with inline code.\nNext line.") || !strings.Contains(l.Text, "    x := 1  \n\n    y := 2") {
		t.Fatalf("logical Markdown=%q", l.Text)
	}
	if len(l.Rows) <= strings.Count(l.Text, "\n")+1 {
		t.Fatal("fixture did not soft wrap")
	}
	for _, row := range l.Rows {
		for _, cell := range row.Cells {
			if cell.Start == cell.End {
				continue
			}
			if got, want := ansi.Strip(ansi.Cut(row.ANSI, cell.X, cell.X+cell.Width)), string([]rune(l.Text)[cell.Start:cell.End]); got != want {
				t.Fatalf("styled cell=%q source=%q", got, want)
			}
		}
	}
}

func TestSemanticCodeIsLiteral(t *testing.T) {
	source := "```text\n\t\\\\server\\file\n\n  final  \n```"
	l := TerminalLayout(source, 20, theme.Default())
	if l.Text != "\t\\\\server\\file\n\n  final  " {
		t.Fatalf("code changed: %q", l.Text)
	}
	inline := TerminalLayout("`\\\\server`", 40, theme.Default())
	if inline.Text != "\\\\server" {
		t.Fatalf("inline code changed: %q", inline.Text)
	}
}

func TestCodeBoundaryWhitespaceSurvivesMarginCleanup(t *testing.T) {
	for _, body := range []string{"\n  hello\n\n", "\n\n", "  \n\thello  \n\t\n"} {
		code := "```text\n" + body + "```"
		want := strings.TrimSuffix(body, "\n")
		for _, width := range []int{6, 40} {
			for _, surrounded := range []bool{false, true} {
				source, expected := code, want
				if surrounded {
					source, expected = "before\n\n"+code+"\n\nafter", "before\n"+want+"\nafter"
				}
				l := TerminalLayout(source, width, theme.Default())
				if l.Text != expected {
					t.Fatalf("width=%d code=%q surrounded=%t: text=%q want=%q", width, body, surrounded, l.Text, expected)
				}
			}
		}
		adjacent := TerminalLayout(code+"\n\n"+code, 40, theme.Default())
		if adjacent.Text != want+"\n"+want {
			t.Fatalf("adjacent code boundary whitespace=%q", adjacent.Text)
		}
	}
}
