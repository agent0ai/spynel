package markdown

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/agent0ai/spynel/internal/theme"
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

func TestTerminalPreservesInlineCodePaddingThroughLineCleanup(t *testing.T) {
	tests := []struct {
		name  string
		input string
		codes []string
	}{
		{name: "middle", input: "Run `/log` now", codes: []string{"/log"}},
		{name: "end", input: "Run `/log`", codes: []string{"/log"}},
		{name: "punctuation", input: "Run `/log`.", codes: []string{"/log"}},
		{name: "multiple", input: "Compare `15m` with `999`", codes: []string{"15m", "999"}},
		{name: "list", input: "- Run `/log`", codes: []string{"/log"}},
		{name: "explicit newline", input: "Run `/log`\nthen wait", codes: []string{"/log"}},
		{name: "unicode adjacency", input: "界`15m`界", codes: []string{"15m"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rendered := Terminal(test.input, 80)
			if strings.Contains(rendered, inlineCodeSpace) {
				t.Fatalf("render-only non-breaking padding leaked into terminal text: %q", rendered)
			}
			for _, code := range test.codes {
				assertInlineCodePadding(t, rendered, code)
			}
		})
	}
}

func TestTerminalPreservesUserAuthoredNonbreakingSpaces(t *testing.T) {
	const input = "outside\u00a0space and `inside\u00a0code`"
	rendered := Terminal(input, 80)
	if got := strings.Count(ansi.Strip(rendered), "\u00a0"); got != 2 {
		t.Fatalf("user-authored nonbreaking spaces = %d, want 2: %q", got, rendered)
	}
	if strings.Contains(rendered, inlineCodeSpace) {
		t.Fatalf("inline-code padding marker leaked into terminal output: %q", rendered)
	}
}

func TestTerminalPreservesUserAuthoredPrivateUseRunes(t *testing.T) {
	const input = "Keep \uE000 and \U000F0000 beside editor-style and `code`."
	plain := ansi.Strip(Terminal(input, 40))
	if !strings.Contains(plain, "\uE000") || !strings.Contains(plain, "\U000F0000") {
		t.Fatalf("user-authored private-use text was treated as a render marker: %q", plain)
	}
	if !strings.Contains(plain, "editor-style") {
		t.Fatalf("compound was split while selecting collision-free markers: %q", plain)
	}
}

func TestTerminalPreservesEveryBreakMarkerCandidate(t *testing.T) {
	const spaces = "\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2008\u2009\u200A\u205F\u3000"
	input := "left" + spaces + "right and editor-style then more" + spaces + "text"
	plain := ansi.Strip(Terminal(input, 200))
	if strings.Count(plain, "\u200A") != 2 {
		t.Fatalf("authored Unicode space was treated as a render break marker: %q", plain)
	}
	if !strings.Contains(plain, "editor-style") {
		t.Fatalf("compound changed while exercising fallback marker selection: %q", plain)
	}
}

func TestTerminalInlineCodePaddingSurvivesWrappingAndNarrowWidths(t *testing.T) {
	for width := 5; width <= 10; width++ {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			// The TUI passes the physical boundary minus one because Glamour's
			// word-wrap option is an exclusive boundary (see chatMarkdownWidth).
			rendered := Terminal("a `界x`", width-1)
			for row, line := range strings.Split(rendered, "\n") {
				if got := ansi.StringWidth(line); got > width {
					t.Fatalf("row %d width = %d, exceeds %d: %q", row, got, width, rendered)
				}
			}
			assertInlineCodePadding(t, rendered, "界x")
		})
	}
}

func TestTerminalFencedCodeDoesNotGainInlinePadding(t *testing.T) {
	rendered := Terminal("```text\n/log\n```", 20)
	cells := terminalTestCells(rendered)
	for _, cell := range cells {
		if cell.text == " " && cell.background {
			t.Fatalf("fenced code gained an inline-code padding cell: %q", rendered)
		}
	}
}

type terminalTestCell struct {
	text       string
	background bool
}

func terminalTestCells(rendered string) []terminalTestCell {
	parser := ansi.NewParser()
	state := byte(ansi.NormalState)
	background := false
	var cells []terminalTestCell
	for len(rendered) > 0 {
		sequence, width, consumed, nextState := ansi.DecodeSequence(rendered, state, parser)
		if consumed <= 0 {
			break
		}
		state = nextState
		rendered = rendered[consumed:]
		if width > 0 {
			cells = append(cells, terminalTestCell{text: string(sequence), background: background})
			continue
		}
		if byte(parser.Command()) == 'm' {
			background = sgrBackground(parser, background)
		}
	}
	return cells
}

func assertInlineCodePadding(t *testing.T, rendered, code string) {
	t.Helper()
	cells := terminalTestCells(rendered)
	for start := range cells {
		if cells[start].text != " " || !cells[start].background {
			continue
		}
		var content strings.Builder
		for end := start + 1; end < len(cells); end++ {
			if cells[end].text == " " {
				if content.String() == code && cells[end].background {
					return
				}
				break
			}
			if !cells[end].background {
				break
			}
			content.WriteString(cells[end].text)
		}
	}
	t.Fatalf("inline code %q lacks symmetric background-colored padding cells: %q", code, rendered)
}

func TestTerminalKeepsExactWidthTextAndShortHeadingTogether(t *testing.T) {
	for _, test := range []struct {
		input string
		width int
		want  string
	}{
		{input: "second response", width: 15, want: "second response"},
		{input: "# About Spynel", width: 16, want: "About Spynel"},
	} {
		result := ansi.Strip(Terminal(test.input, test.width))
		if strings.Contains(result, "\n") || !strings.Contains(result, test.want) {
			t.Fatalf("exact-width Markdown wrapped unexpectedly at %d: %q", test.width, result)
		}
		if width := ansi.StringWidth(result); width > test.width {
			t.Fatalf("Markdown row width = %d, exceeds %d: %q", width, test.width, result)
		}
	}
}

func TestTerminalMovesFittingHyphenatedCompoundIntact(t *testing.T) {
	const input = "Use the familiar editor-style Up/Down controls."
	for _, width := range []int{20, 21, 22} {
		rendered := Terminal(input, width)
		plain := ansi.Strip(rendered)
		for row, line := range strings.Split(plain, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d row %d has %d cells: %q", width, row, got, plain)
			}
			if strings.HasSuffix(line, "editor-") || strings.TrimSpace(line) == "style" {
				t.Fatalf("width %d orphaned editor-style: %q", width, plain)
			}
		}
		if !strings.Contains(plain, "editor-style") {
			t.Fatalf("width %d split fitting compound: %q", width, plain)
		}
		if width == 20 && !strings.Contains(plain, "\neditor-style Up/Down\n") {
			t.Fatalf("fitting compound did not move to the better fresh row: %q", plain)
		}
	}
}

func TestTerminalCompoundAtWrapBoundary(t *testing.T) {
	const compound = "editor-style"
	for _, width := range []int{11, 12, 13} {
		plain := ansi.Strip(Terminal(compound, width))
		for row, line := range strings.Split(plain, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d row %d overflowed to %d cells: %q", width, row, got, plain)
			}
		}
		if width >= ansi.StringWidth(compound) && plain != compound {
			t.Fatalf("width %d split exact-fit compound: %q", width, plain)
		}
	}
}

func TestTerminalKeepsOrdinaryCompoundsWithoutProtectingOtherHyphens(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		width     int
		intact    []string
		unchanged string
	}{
		{name: "multiple and punctuation", input: "Try scroll-to-bottom, then Ctrl-C-safe mode.", width: 19, intact: []string{"scroll-to-bottom", "Ctrl-C-safe"}},
		{name: "Unicode words and wide cells", input: "Use café-style and 東京-style controls.", width: 15, intact: []string{"café-style", "東京-style"}},
		{name: "list indentation", input: "- Choose editor-style controls.\n- Keep Ctrl-C-safe mode.", width: 18, intact: []string{"editor-style", "Ctrl-C-safe"}},
		{name: "ordered list indentation", input: "10. Choose editor-style controls.\n11. Keep Ctrl-C-safe mode.", width: 19, intact: []string{"editor-style", "Ctrl-C-safe"}},
		{name: "inline and fenced code", input: "Use `editor-style`.\n\n```text\nscroll-to-bottom\n```", width: 10, unchanged: "editor-style"},
		{name: "URL flag and minus", input: "See https://example.test/editor-style and --scroll-to-bottom; compute 8-3.", width: 16, unchanged: "--scroll-to-bottom"},
		{name: "dash variants", input: "Keep em—dash, en–dash, and soft\u00adhyphen.", width: 14, unchanged: "soft\u00adhyphen"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rendered := Terminal(test.input, test.width)
			plain := ansi.Strip(rendered)
			if len(test.intact) > 0 {
				for row, line := range strings.Split(plain, "\n") {
					if got := ansi.StringWidth(line); got > test.width {
						t.Fatalf("row %d has %d cells, exceeds %d: %q", row, got, test.width, plain)
					}
				}
			}
			for _, compound := range test.intact {
				if !strings.Contains(plain, compound) {
					t.Fatalf("compound %q was split: %q", compound, plain)
				}
			}
			if test.unchanged != "" && !strings.Contains(strings.ReplaceAll(plain, "\n", ""), test.unchanged) {
				t.Fatalf("non-prose hyphen text changed: %q", plain)
			}
			if strings.Contains(rendered, compoundHyphen) {
				t.Fatalf("render marker leaked: %q", rendered)
			}
		})
	}
}

func TestTerminalAllowsOverwideCompoundToWrapSafely(t *testing.T) {
	const input = "extraordinarily-long-hyphenated-compound-with-several-parts"
	for width := 4; width <= 16; width++ {
		rendered := Terminal(input, width)
		if strings.Contains(rendered, compoundHyphen) {
			t.Fatalf("width %d leaked render marker: %q", width, rendered)
		}
		for row, line := range strings.Split(ansi.Strip(rendered), "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d row %d overflowed to %d cells: %q", width, row, got, ansi.Strip(rendered))
			}
		}
	}
}

func TestTerminalHardWrapsOverwideWideUnicodeWithoutOverflow(t *testing.T) {
	const input = "雪雪雪雪雪雪雪雪雪雪雪雪雪雪雪雪"
	for width := 4; width <= 12; width++ {
		plain := ansi.Strip(Terminal(input, width))
		if strings.ReplaceAll(plain, "\n", "") != input {
			t.Fatalf("width %d changed Unicode: %q", width, plain)
		}
		for _, line := range strings.Split(plain, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d overflowed to %d: %q", width, got, plain)
			}
		}
	}
}

func TestTerminalOverwideCompoundHardBreakPreservesGraphemes(t *testing.T) {
	const input = "क्त्रक्त्रक्त्रक्त्र-long"
	for width := 3; width <= 8; width++ {
		plain := ansi.Strip(Terminal(input, width))
		if flattened := strings.ReplaceAll(plain, "\n", ""); flattened != input {
			t.Fatalf("width %d changed source text: got %q, want %q", width, flattened, input)
		}
		boundaries := map[int]bool{0: true}
		graphemes := uniseg.NewGraphemes(input)
		for graphemes.Next() {
			_, to := graphemes.Positions()
			boundaries[to] = true
		}
		offset := 0
		for _, line := range strings.SplitAfter(plain, "\n") {
			content := strings.TrimSuffix(line, "\n")
			offset += len(content)
			if strings.HasSuffix(line, "\n") && !boundaries[offset] {
				t.Fatalf("width %d split a grapheme at byte %d: %q", width, offset, plain)
			}
			if got := ansi.StringWidth(content); got > width {
				t.Fatalf("width %d row has %d cells: %q", width, got, plain)
			}
		}
	}
}

func TestTerminalCompoundHintsPreserveMarkdownBlockStructure(t *testing.T) {
	heading := ansi.Strip(Terminal("# Use familiar editor-style controls", 20))
	if !strings.Contains(heading, "◆") || !strings.Contains(strings.ReplaceAll(heading, "\n", ""), "editor-style") {
		t.Fatalf("heading structure or compound changed: %q", heading)
	}
	table := ansi.Strip(Terminal("| Mode | Control |\n| --- | --- |\n| familiar | editor-style |", 24))
	if !strings.Contains(table, "Mode") || !strings.Contains(table, "Control") || !strings.Contains(strings.ReplaceAll(table, "\n", ""), "editor-style") {
		t.Fatalf("table structure or compound changed: %q", table)
	}
	for name, input := range map[string]string{
		"heading": "# extraordinarily-long-hyphenated-compound",
		"table":   "| Mode |\n| --- |\n| extraordinarily-long-hyphenated-compound |",
	} {
		rendered := ansi.Strip(Terminal(input, 12))
		if name == "heading" && !strings.Contains(rendered, "◆") {
			t.Fatalf("overwide heading lost structure: %q", rendered)
		}
		for row, line := range strings.Split(rendered, "\n") {
			if got := ansi.StringWidth(line); got > 12 {
				t.Fatalf("overwide %s row %d has %d cells: %q", name, row, got, rendered)
			}
		}
	}
}

func TestTerminalStyleUsesSemanticThemeColors(t *testing.T) {
	originalChromaText := *styles.DarkStyleConfig.CodeBlock.Chroma.Text.Color
	active := theme.Default()
	active.Colors.Text = "#010203"
	active.Colors.Primary = "#040506"
	active.Colors.Surface = "#070809"
	active.Colors.Code = "#0A0B0C"
	style := terminalStyle(active)
	if style.Document.Color == nil || *style.Document.Color != active.Colors.Text {
		t.Fatalf("document color = %#v", style.Document.Color)
	}
	if style.Heading.Color == nil || *style.Heading.Color != active.Colors.Primary {
		t.Fatalf("heading color = %#v", style.Heading.Color)
	}
	if style.Strong.Color == nil || *style.Strong.Color != active.Colors.Primary {
		t.Fatalf("strong emphasis color = %#v", style.Strong.Color)
	}
	if style.Code.Color == nil || *style.Code.Color != active.Colors.Code || style.Code.BackgroundColor == nil || *style.Code.BackgroundColor != active.Colors.SurfaceElevated {
		t.Fatalf("inline code style = %#v", style.Code)
	}
	if style.CodeBlock.Chroma != nil || style.CodeBlock.Theme != "" || style.CodeBlock.Color == nil || *style.CodeBlock.Color != active.Colors.Code {
		t.Fatalf("code block style = %#v", style.CodeBlock)
	}
	if got := *styles.DarkStyleConfig.CodeBlock.Chroma.Text.Color; got != originalChromaText {
		t.Fatalf("themed renderer mutated Glamour's global style: %q, want %q", got, originalChromaText)
	}
}

func TestRenderedFencedCodeUsesActiveTrueColorRole(t *testing.T) {
	active := theme.Default()
	active.Colors.Code = "#0A0B0C"
	result := TerminalWithTheme("```go\nstatus := runtime.Snapshot()\n```", 80, active)
	if !strings.Contains(result, "38;2;10;11;12") {
		t.Fatalf("rendered code does not use active semantic truecolor: %q", result)
	}
	if strings.Contains(result, "38;5;") {
		t.Fatalf("rendered code leaked a fixed ANSI-256 Chroma color: %q", result)
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
