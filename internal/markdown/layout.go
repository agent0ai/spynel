package markdown

import (
	"strings"
	"unicode"

	"github.com/agent0ai/spynel/internal/theme"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// Layout keeps logical text separate from terminal cells. Row ranges are rune
// offsets into Text; soft wrapping never creates a newline in that text.
type Layout struct {
	Logical, Text string
	Rows          []TextRow
}
type TextRow struct {
	ANSI       string
	Start, End int
	Cells      []TextCell
}

// A zero-length range is display-only padding, never copyable source text.
type TextCell struct{ Start, End, X, Width int }

func (l Layout) View() string {
	rows := make([]string, len(l.Rows))
	for i, row := range l.Rows {
		rows[i] = row.ANSI
	}
	return strings.Join(rows, "\n")
}

// TerminalLayout renders Markdown without reflow first, then applies one
// source-indexed wrap. Glamour's ordinary final string loses soft/hard breaks.
func TerminalLayout(input string, width int, active theme.Theme) Layout {
	return WrapLogical(TerminalLogical(input, active), width)
}

// TerminalLogical retains hard newlines without spending work on a layout the
// caller will immediately replace at its own viewport width.
func TerminalLogical(input string, active theme.Theme) string {
	return terminalWithTheme(input, 0, active)
}

func WrapLogical(logical string, width int) Layout {
	width = max(1, width)
	l := Layout{Logical: logical}
	var source strings.Builder
	marker := "\uE000"
	for strings.Contains(logical, marker) {
		marker = string([]rune(marker)[0] + 1)
	}
	offset := 0
	for _, line := range strings.Split(logical, "\n") {
		plain := ansi.Strip(strings.ReplaceAll(line, "\x00", marker))
		var plainRunes []rune
		if plain == line {
			plainRunes = []rune(plain)
		}
		type cluster struct {
			runeAt, end, cellAt, width int
			space                      bool
		}
		var chars []cluster
		runeAt, cellAt := 0, 0
		g := uniseg.NewGraphemes(plain)
		for g.Next() {
			value := g.Str()
			w := uniseg.StringWidth(strings.ReplaceAll(value, "\t", "    "))
			end := runeAt
			if value != marker {
				end += len(g.Runes())
				source.WriteString(value)
			}
			chars = append(chars, cluster{runeAt, end, cellAt, w, unicode.IsSpace(g.Runes()[0])})
			runeAt = end
			cellAt += w
		}
		chars = append(chars, cluster{runeAt: runeAt, cellAt: cellAt})
		line = strings.NewReplacer("\t", "    ", "\x00", " ").Replace(line)
		for start := 0; ; {
			end, cells, lastBreak := start, 0, start
			for end < len(chars)-1 && (cells+chars[end].width <= width || end == start) {
				cells += chars[end].width
				if chars[end].space {
					lastBreak = end + 1
				}
				end++
			}
			if end < len(chars)-1 && !chars[end].space && lastBreak > start {
				end = lastBreak
			}
			row := TextRow{Start: offset + chars[start].runeAt, End: offset + chars[end].runeAt}
			if plainRunes != nil {
				// Pending stream text has no styles. Slice its known rune range
				// directly instead of rescanning a long line for every row.
				row.ANSI = strings.ReplaceAll(string(plainRunes[chars[start].runeAt:chars[end].runeAt]), "\t", "    ")
			} else {
				// ponytail: styled row cuts still rescan one logical line; use
				// incremental style carry if long styled lines become material.
				row.ANSI = ansi.Cut(line, chars[start].cellAt, chars[end].cellAt)
			}
			for _, c := range chars[start:end] {
				row.Cells = append(row.Cells, TextCell{offset + c.runeAt, offset + c.end, c.cellAt - chars[start].cellAt, c.width})
			}
			l.Rows = append(l.Rows, row)
			if end == len(chars)-1 {
				break
			}
			// A single ordinary separator with no cell at the soft boundary
			// remains in Text. Additional authored whitespace stays visible.
			if chars[end].space && chars[end].width == 1 && chars[end].cellAt-chars[start].cellAt >= width {
				end++
			}
			start = end
		}
		offset += runeAt + 1
		source.WriteByte('\n')
	}
	l.Text = strings.TrimSuffix(source.String(), "\n")
	return l
}
