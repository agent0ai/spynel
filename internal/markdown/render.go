package markdown

import (
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const (
	terminalLinkStart = "\x02"
	terminalLinkEnd   = "\x03"
	codeBlockStart    = "\x04\x04\x04"
	codeBlockEnd      = "\x05\x05\x05"
	osc8Close         = "\x1b]8;;\x1b\\"
)

// Terminal renders GitHub-flavored Markdown for an ANSI terminal.
func Terminal(input string, width int) string {
	width = max(20, width)
	style := styles.DarkStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.BlockPrefix = codeBlockStart
	style.CodeBlock.BlockSuffix = codeBlockEnd
	// Mark the rendered href boundaries before Glamour wraps the document.
	// They are replaced with destination-specific OSC 8 sequences afterward,
	// so the control sequence itself cannot be split across display rows.
	style.Link.BlockPrefix = terminalLinkStart
	style.Link.BlockSuffix = terminalLinkEnd
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return input
	}
	result, err := renderer.Render(input)
	if err != nil {
		return input
	}
	result = applyTerminalLinks(result, terminalLinkTargets(input))
	return strings.TrimSpace(compactCodeBlockSpacing(result))
}

// compactCodeBlockSpacing removes only blank rendered rows immediately next
// to code blocks. Boundary markers let this remain independent of ANSI styles
// and preserve ordinary paragraph gaps and intentional blank code lines.
func compactCodeBlockSpacing(value string) string {
	lines := strings.Split(value, "\n")
	result := make([]string, 0, len(lines))
	skipBlank := false
	for _, line := range lines {
		hasStart := strings.Contains(line, codeBlockStart)
		hasEnd := strings.Contains(line, codeBlockEnd)
		line = strings.ReplaceAll(strings.ReplaceAll(line, codeBlockStart, ""), codeBlockEnd, "")
		if hasStart {
			for len(result) > 0 && visuallyBlank(result[len(result)-1]) {
				result = result[:len(result)-1]
			}
		}
		if skipBlank && visuallyBlank(line) {
			continue
		}
		skipBlank = false
		if !hasEnd || !visuallyBlank(line) {
			result = append(result, line)
		}
		if hasEnd {
			skipBlank = true
		}
	}
	return strings.Join(result, "\n")
}

func visuallyBlank(value string) bool {
	return strings.TrimSpace(ansi.Strip(value)) == ""
}

func terminalLinkTargets(input string) []string {
	source := []byte(input)
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()
	document := parser.Parse(text.NewReader(source))
	var targets []string
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch value := node.(type) {
		case *ast.Link:
			destination := string(value.Destination)
			if !strings.HasPrefix(destination, "#") {
				targets = append(targets, terminalLinkTarget(destination))
			}
		case *ast.AutoLink:
			targets = append(targets, terminalLinkTarget(string(value.URL(source))))
		}
		return ast.WalkContinue, nil
	})
	return targets
}

func terminalLinkTarget(destination string) string {
	if destination == "" || strings.ContainsAny(destination, "\x00\x1b\a") {
		return ""
	}
	if strings.HasPrefix(destination, "/") {
		path, fragment := filePosition(destination)
		if decoded, err := url.PathUnescape(path); err == nil {
			path = decoded
		}
		return (&url.URL{Scheme: "file", Path: path, Fragment: fragment}).String()
	}
	parsed, err := url.Parse(destination)
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "file", "ftp", "http", "https", "mailto":
		return parsed.String()
	default:
		// This renderer has no reliable base directory for relative paths.
		return ""
	}
}

func filePosition(destination string) (string, string) {
	lastColon := strings.LastIndexByte(destination, ':')
	if lastColon < 1 {
		return destination, ""
	}
	last, err := strconv.Atoi(destination[lastColon+1:])
	if err != nil || last < 1 {
		return destination, ""
	}
	previousColon := strings.LastIndexByte(destination[:lastColon], ':')
	if previousColon > 0 {
		line, lineErr := strconv.Atoi(destination[previousColon+1 : lastColon])
		if lineErr == nil && line > 0 {
			return destination[:previousColon], strconv.Itoa(line) + ":" + strconv.Itoa(last)
		}
	}
	return destination[:lastColon], strconv.Itoa(last)
}

func applyTerminalLinks(rendered string, targets []string) string {
	var result strings.Builder
	rest := rendered
	for _, target := range targets {
		start := strings.Index(rest, terminalLinkStart)
		if start < 0 {
			break
		}
		contentStart := start + len(terminalLinkStart)
		endOffset := strings.Index(rest[contentStart:], terminalLinkEnd)
		if endOffset < 0 {
			break
		}
		end := contentStart + endOffset
		result.WriteString(rest[:start])
		if target != "" {
			result.WriteString("\x1b]8;;")
			result.WriteString(target)
			result.WriteString("\x1b\\")
		}
		result.WriteString(rest[contentStart:end])
		if target != "" {
			result.WriteString(osc8Close)
		}
		rest = rest[end+len(terminalLinkEnd):]
	}
	result.WriteString(strings.ReplaceAll(strings.ReplaceAll(rest, terminalLinkStart, ""), terminalLinkEnd, ""))
	return result.String()
}

// TelegramHTML converts GitHub-flavored Markdown to Telegram's supported HTML
// subset. Unsupported block structure is flattened into readable text.
func TelegramHTML(input string) string {
	return render(input, telegram)
}

// WhatsApp converts GitHub-flavored Markdown to WhatsApp's native lightweight
// formatting syntax.
func WhatsApp(input string) string {
	return render(input, whatsapp)
}

type platform int

const (
	telegram platform = iota
	whatsapp
)

type nativeRenderer struct {
	source []byte
	mode   platform
}

func render(input string, mode platform) string {
	source := []byte(input)
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()
	document := parser.Parse(text.NewReader(source))
	renderer := nativeRenderer{source: source, mode: mode}
	return strings.TrimSpace(compactCodeBlockSpacing(renderer.node(document)))
}

func (r nativeRenderer) node(node ast.Node) string { //nolint:gocyclo
	switch value := node.(type) {
	case *ast.Document:
		return r.children(node)
	case *ast.Text:
		result := r.escape(string(value.Segment.Value(r.source)))
		if value.SoftLineBreak() || value.HardLineBreak() {
			result += "\n"
		}
		return result
	case *ast.String:
		return r.escape(string(value.Value))
	case *ast.Paragraph, *ast.TextBlock:
		return strings.TrimSpace(r.children(node)) + "\n\n"
	case *ast.Heading:
		content := strings.TrimSpace(r.children(node))
		if r.mode == telegram {
			return "<b>" + content + "</b>\n\n"
		}
		return "*" + content + "*\n\n"
	case *ast.Emphasis:
		content := r.children(node)
		if r.mode == telegram {
			if value.Level == 2 {
				return "<b>" + content + "</b>"
			}
			return "<i>" + content + "</i>"
		}
		if value.Level == 2 {
			return "*" + content + "*"
		}
		return "_" + content + "_"
	case *extast.Strikethrough:
		content := r.children(node)
		if r.mode == telegram {
			return "<s>" + content + "</s>"
		}
		return "~" + content + "~"
	case *ast.CodeSpan:
		content := strings.TrimSpace(r.children(node))
		if r.mode == telegram {
			return "<code>" + content + "</code>"
		}
		return "`" + content + "`"
	case *ast.FencedCodeBlock:
		return r.codeBlock(value, string(value.Language(r.source)))
	case *ast.CodeBlock:
		return r.codeBlock(value, "")
	case *ast.Link:
		return r.link(r.children(node), string(value.Destination))
	case *ast.Image:
		return r.link("Image: "+r.children(node), string(value.Destination))
	case *ast.AutoLink:
		url := string(value.URL(r.source))
		return r.link(r.escape(url), url)
	case *ast.Blockquote:
		content := strings.TrimSpace(r.children(node))
		if r.mode == telegram {
			return "<blockquote>" + content + "</blockquote>\n\n"
		}
		return prefixLines(content, "> ") + "\n\n"
	case *ast.List:
		return r.list(value)
	case *ast.ListItem:
		return strings.TrimSpace(r.children(node))
	case *ast.ThematicBreak:
		return "────────\n\n"
	case *extast.Table:
		return strings.TrimSpace(r.children(node)) + "\n\n"
	case *extast.TableHeader, *extast.TableRow:
		var cells []string
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			cells = append(cells, strings.TrimSpace(r.node(child)))
		}
		return strings.Join(cells, " | ") + "\n"
	case *extast.TableCell:
		return r.children(node)
	case *ast.RawHTML:
		return ""
	default:
		return r.children(node)
	}
}

func (r nativeRenderer) children(node ast.Node) string {
	var result strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		result.WriteString(r.node(child))
	}
	return result.String()
}

func (r nativeRenderer) codeBlock(node ast.Node, language string) string {
	var code strings.Builder
	for index := 0; index < node.Lines().Len(); index++ {
		segment := node.Lines().At(index)
		code.Write(segment.Value(r.source))
	}
	content := strings.TrimSuffix(code.String(), "\n")
	if r.mode == telegram {
		class := ""
		if language != "" {
			class = ` class="language-` + html.EscapeString(language) + `"`
		}
		return codeBlockStart + "<pre><code" + class + ">" + html.EscapeString(content) + "</code></pre>" + codeBlockEnd + "\n\n"
	}
	return codeBlockStart + "```\n" + content + "\n```" + codeBlockEnd + "\n\n"
}

func (r nativeRenderer) list(list *ast.List) string {
	var result strings.Builder
	index := list.Start
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		content := strings.TrimSpace(r.node(child))
		prefix := "• "
		if r.mode == whatsapp {
			prefix = "- "
		}
		if list.IsOrdered() {
			prefix = formatNumber(index) + ". "
			index++
		}
		result.WriteString(prefix)
		result.WriteString(strings.ReplaceAll(content, "\n", "\n  "))
		result.WriteByte('\n')
	}
	result.WriteByte('\n')
	return result.String()
}

func (r nativeRenderer) link(label, destination string) string {
	if r.mode == telegram {
		return `<a href="` + html.EscapeString(destination) + `">` + label + `</a>`
	}
	if label == destination {
		return destination
	}
	return label + " (" + destination + ")"
}

func (r nativeRenderer) escape(value string) string {
	if r.mode == telegram {
		return html.EscapeString(value)
	}
	return value
}

func prefixLines(value, prefix string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = prefix + lines[index]
	}
	return strings.Join(lines, "\n")
}

func formatNumber(value int) string {
	if value == 0 {
		value = 1
	}
	const digits = "0123456789"
	if value < 10 {
		return string(digits[value])
	}
	var reversed []byte
	for value > 0 {
		reversed = append(reversed, digits[value%10])
		value /= 10
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return string(reversed)
}
