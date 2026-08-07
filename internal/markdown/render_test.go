package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const sample = `# Release

Use **bold**, *italic*, ~~removed~~, and ` + "`code`" + `.

- first
- [docs](https://example.com)

> quoted

` + "```go\nfmt.Println(\"ok\")\n```" + `
`

func TestTelegramHTMLUsesSupportedNativeFormatting(t *testing.T) {
	result := TelegramHTML(sample)
	for _, expected := range []string{
		"<b>Release</b>", "<b>bold</b>", "<i>italic</i>", "<s>removed</s>",
		"<code>code</code>", "• first", `<a href="https://example.com">docs</a>`,
		"<blockquote>quoted</blockquote>", `<pre><code class="language-go">`,
	} {
		if !strings.Contains(result, expected) {
			t.Fatalf("Telegram output missing %q:\n%s", expected, result)
		}
	}
}

func TestWhatsAppUsesNativeFormatting(t *testing.T) {
	result := WhatsApp(sample)
	for _, expected := range []string{
		"*Release*", "*bold*", "_italic_", "~removed~", "`code`", "- first",
		"docs (https://example.com)", "> quoted", "```\nfmt.Println",
	} {
		if !strings.Contains(result, expected) {
			t.Fatalf("WhatsApp output missing %q:\n%s", expected, result)
		}
	}
}

func TestTerminalRendersMarkdownInsteadOfShowingMarkers(t *testing.T) {
	result := Terminal("**bold** and `code`", 80)
	if strings.Contains(result, "**bold**") || result == "" {
		t.Fatalf("unexpected terminal output %q", result)
	}
}

func TestTerminalPreservesIntentionalPlainTextLines(t *testing.T) {
	result := ansi.Strip(Terminal("first line\nsecond line", 80))
	lines := strings.Split(result, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "first line" || strings.TrimSpace(lines[1]) != "second line" {
		t.Fatalf("plain-text lines were reflowed: %q", result)
	}
}

func TestTerminalUsesOneBlankLineBetweenParagraphs(t *testing.T) {
	result := ansi.Strip(Terminal("first paragraph\n\nsecond paragraph", 80))
	lines := strings.Split(result, "\n")
	blankRun := 0
	maxBlankRun := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blankRun++
			maxBlankRun = max(maxBlankRun, blankRun)
		} else {
			blankRun = 0
		}
	}
	if maxBlankRun != 1 {
		t.Fatalf("paragraph spacing has %d consecutive blank rows, want 1: %q", maxBlankRun, result)
	}
}

func TestCodeBlocksHaveNoBlankRowsAroundThem(t *testing.T) {
	input := "before\n\n```go\nfmt.Println(\"ok\")\n```\n\nafter"
	terminal := ansi.Strip(Terminal(input, 80))
	terminalLines := strings.Split(terminal, "\n")
	if len(terminalLines) != 3 || strings.TrimSpace(terminalLines[0]) != "before" || strings.TrimSpace(terminalLines[1]) != `fmt.Println("ok")` || strings.TrimSpace(terminalLines[2]) != "after" {
		t.Fatalf("terminal code block has surrounding blank rows: %q", terminal)
	}
	telegram := TelegramHTML(input)
	if strings.Contains(telegram, "before\n\n<pre") || strings.Contains(telegram, "</pre>\n\nafter") {
		t.Fatalf("Telegram code block has surrounding blank rows: %q", telegram)
	}
	whatsapp := WhatsApp(input)
	if strings.Contains(whatsapp, "before\n\n```") || strings.Contains(whatsapp, "```\n\nafter") {
		t.Fatalf("WhatsApp code block has surrounding blank rows: %q", whatsapp)
	}
	for name, output := range map[string]string{"terminal": Terminal(input, 80), "Telegram": telegram, "WhatsApp": whatsapp} {
		if strings.Contains(output, codeBlockStart) || strings.Contains(output, codeBlockEnd) {
			t.Fatalf("%s output leaked an internal code boundary: %q", name, output)
		}
	}
}

func TestCodeBlocksPreserveInternalBlankRows(t *testing.T) {
	input := "```text\nfirst\n\nthird\n```"
	terminalLines := strings.Split(ansi.Strip(Terminal(input, 80)), "\n")
	if len(terminalLines) != 3 || strings.TrimSpace(terminalLines[0]) != "first" || strings.TrimSpace(terminalLines[1]) != "" || strings.TrimSpace(terminalLines[2]) != "third" {
		t.Fatalf("terminal changed an internal blank code row: %#v", terminalLines)
	}
	if output := TelegramHTML(input); !strings.Contains(output, "first\n\nthird") {
		t.Fatalf("Telegram changed an internal blank code row: %q", output)
	}
	if output := WhatsApp(input); !strings.Contains(output, "first\n\nthird") {
		t.Fatalf("WhatsApp changed an internal blank code row: %q", output)
	}
}

func TestTerminalMakesAbsoluteFileLinksClickable(t *testing.T) {
	result := Terminal("[render.go](/workspace/spynel/internal/markdown/render.go)", 80)
	if !strings.Contains(result, "\x1b]8;;file:///workspace/spynel/internal/markdown/render.go\x1b\\") {
		t.Fatalf("terminal output lacks OSC 8 file hyperlink: %q", result)
	}
	if !strings.Contains(result, "\x1b]8;;\x1b\\") {
		t.Fatalf("terminal output lacks OSC 8 hyperlink terminator: %q", result)
	}
}

func TestTerminalFileLinkPreservesLineAndColumn(t *testing.T) {
	result := Terminal("[render.go](/workspace/spynel/internal/markdown/render.go:17:4)", 80)
	if !strings.Contains(result, "\x1b]8;;file:///workspace/spynel/internal/markdown/render.go#17:4\x1b\\") {
		t.Fatalf("terminal file hyperlink lacks source position: %q", result)
	}
}

func TestTerminalMakesWebLinksClickable(t *testing.T) {
	result := Terminal("[docs](https://example.com)", 80)
	if !strings.Contains(result, "\x1b]8;;https://example.com\x1b\\") {
		t.Fatalf("terminal output lacks OSC 8 web hyperlink: %q", result)
	}
}

func TestTerminalLeavesRelativeLinkTargetsAsPlainText(t *testing.T) {
	result := Terminal("[render.go](internal/markdown/render.go)", 80)
	if strings.Contains(result, "\x1b]8;;") {
		t.Fatalf("relative link unexpectedly received a context-free hyperlink: %q", result)
	}
}

func TestTerminalAnchorDoesNotConsumeFollowingHyperlink(t *testing.T) {
	result := Terminal("[section](#section) [docs](https://example.com)", 80)
	if strings.Count(result, "\x1b]8;;https://example.com\x1b\\") != 1 {
		t.Fatalf("link after document anchor was not hyperlinked: %q", result)
	}
}
