// Package agentdocs owns Spynel's curated, offline documentation catalog.
// It deliberately contains no workspace or runtime readers: live state belongs to
// status, jobs, and logs rather than static documentation.
package agentdocs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaVersion      = "spynel.docs/v1"
	DefaultTokenBudget = 10_000
	MaxEntries         = 128
	MaxBytes           = 64 * 1024
	MaxRunes           = 48 * 1024
)

type Section struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type Topic struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Kind        string    `json:"kind"`
	Sections    []Section `json:"sections,omitempty"`
	Related     []string  `json:"related,omitempty"`
	HelpID      string    `json:"-"`
	HelpSummary string    `json:"-"`
}

type Page struct {
	Number          int `json:"number"`
	Total           int `json:"total"`
	EntryStart      int `json:"entry_start"`
	EntryEnd        int `json:"entry_end"`
	TotalEntries    int `json:"total_entries"`
	MaxEntries      int `json:"max_entries"`
	TokenBudget     int `json:"token_budget,omitempty"`
	EstimatedTokens int `json:"estimated_tokens,omitempty"`
	Bytes           int `json:"bytes"`
	Runes           int `json:"runes"`
}

type Error struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Suggestion string   `json:"suggestion,omitempty"`
	Valid      []string `json:"valid,omitempty"`
}

type Document struct {
	SchemaVersion string    `json:"schema_version"`
	Kind          string    `json:"kind"`
	ID            string    `json:"id,omitempty"`
	Title         string    `json:"title,omitempty"`
	Summary       string    `json:"summary,omitempty"`
	Topics        []Topic   `json:"topics,omitempty"`
	Sections      []Section `json:"sections,omitempty"`
	Related       []string  `json:"related,omitempty"`
	Query         string    `json:"query,omitempty"`
	Page          Page      `json:"page"`
	Error         *Error    `json:"error,omitempty"`
}

type Request struct {
	Topic  string
	Search string
	Page   int
	Format string
}

type pageWeight struct {
	tokens int
	bytes  int
	runes  int
}

func HelpTopics() []Topic {
	var out []Topic
	for _, topic := range topics {
		if topic.HelpID != "" {
			copy := topic
			copy.ID, copy.Summary = topic.HelpID, topic.HelpSummary
			out = append(out, copy)
		}
	}
	return out
}

// DocumentedSlashCommands returns the base slash commands named by the static
// command topic. The app package verifies these against its canonical catalog.
func DocumentedSlashCommands() []string {
	return []string{"/help", "/status", "/config", "/harness", "/model", "/telegram", "/whatsapp", "/stop", "/restart", "/update", "/history", "/log", "/jobs", "/job", "/tasks", "/goals", "/clear", "/task", "/goal", "/trigger", "/cleanup", "/extension"}
}

func Render(request Request) (string, error) {
	document, err := Lookup(request)
	if err != nil {
		return "", err
	}
	format := strings.ToLower(strings.TrimSpace(request.Format))
	if format == "" || format == "text" || format == "markdown" || format == "md" {
		text := renderText(document)
		if len(text) > MaxBytes || utf8.RuneCountInString(text) > MaxRunes {
			return "", fmt.Errorf("documentation page exceeds the %d-byte/%d-rune safety limit", MaxBytes, MaxRunes)
		}
		return text, nil
	}
	if format != "json" {
		return "", fmt.Errorf("unsupported docs format %q; use text or json", request.Format)
	}
	var data []byte
	for range 8 {
		var err error
		data, err = json.MarshalIndent(document, "", "  ")
		if err != nil {
			return "", err
		}
		bytes, runes := len(data)+1, utf8.RuneCount(data)+1
		if document.Page.Bytes == bytes && document.Page.Runes == runes {
			break
		}
		document.Page.Bytes, document.Page.Runes = bytes, runes
	}
	data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", err
	}
	if len(data)+1 > MaxBytes || utf8.RuneCount(data)+1 > MaxRunes {
		return "", fmt.Errorf("documentation page exceeds the %d-byte/%d-rune safety limit", MaxBytes, MaxRunes)
	}
	return string(data) + "\n", nil
}

func Lookup(request Request) (Document, error) {
	page := request.Page
	if page == 0 {
		page = 1
	}
	if page < 1 {
		return errorDocument("invalid_page", "page must be a positive integer", "spynel docs page 1", nil), nil
	}
	if containsControl(request.Search) || containsControl(request.Topic) {
		return errorDocument("invalid_input", "topics and search queries may not contain control characters", "spynel docs", nil), nil
	}
	if strings.Count(request.Topic, "#") > 1 || strings.HasPrefix(request.Topic, "#") || strings.HasSuffix(request.Topic, "#") {
		return errorDocument("invalid_reference", "section references must use topic#section", "spynel docs", nil), nil
	}
	if utf8.RuneCountInString(request.Search) > 256 || utf8.RuneCountInString(request.Topic) > 128 {
		return errorDocument("input_too_large", "topic names are limited to 128 runes and search queries to 256 runes", "spynel docs", nil), nil
	}
	if strings.TrimSpace(request.Search) != "" {
		return searchDocument(strings.TrimSpace(request.Search), page), nil
	}
	name, sectionID := splitReference(request.Topic)
	name = canonicalTopic(name)
	if name == "" {
		entries := make([]Topic, len(topics))
		for i, topic := range topics {
			entries[i] = Topic{ID: topic.ID, Title: topic.Title, Summary: topic.Summary, Kind: topic.Kind, Related: topic.Related}
		}
		weights := make([]pageWeight, len(entries))
		for index, entry := range entries {
			weights[index] = weigh(entry.ID + " " + entry.Title + " " + entry.Summary)
		}
		start, end, metadata, ok := paginate(weights, page)
		if !ok {
			return errorDocument("page_out_of_range", fmt.Sprintf("index page %d does not exist", page), "spynel docs page "+strconv.Itoa(metadata.Total), nil), nil
		}
		doc := Document{SchemaVersion: SchemaVersion, Kind: "index", ID: "index", Title: "Spynel documentation", Summary: "Curated static behavior; use status, jobs, and logs for current runtime state.", Topics: entries[start:end], Page: metadata}
		return bounded(doc)
	}
	topic, ok := topicByID(name)
	if !ok {
		valid := topicIDs()
		suggestion := closest(name, valid)
		hint := "spynel docs"
		if suggestion != "" {
			hint = "spynel docs " + suggestion
		}
		return errorDocument("unknown_topic", fmt.Sprintf("unknown documentation topic %q", request.Topic), hint, valid), nil
	}
	sections := topic.Sections
	if sectionID != "" {
		sections = nil
		for _, section := range topic.Sections {
			if section.ID == sectionID {
				sections = []Section{section}
				break
			}
		}
		if len(sections) == 0 {
			valid := make([]string, 0, len(topic.Sections))
			for _, section := range topic.Sections {
				valid = append(valid, topic.ID+"#"+section.ID)
			}
			suggestion := closest(topic.ID+"#"+sectionID, valid)
			hint := "spynel docs " + topic.ID
			if suggestion != "" {
				hint = "spynel docs " + suggestion
			}
			return errorDocument("unknown_section", fmt.Sprintf("unknown documentation section %q", request.Topic), hint, valid), nil
		}
	}
	weights := make([]pageWeight, len(sections))
	for index, section := range sections {
		weights[index] = weigh(section.ID + " " + section.Title + " " + section.Content)
	}
	start, end, metadata, ok := paginate(weights, page)
	if !ok {
		return errorDocument("page_out_of_range", fmt.Sprintf("topic %q has no page %d", topic.ID, page), fmt.Sprintf("spynel docs %s page %d", topic.ID, metadata.Total), nil), nil
	}
	id := topic.ID
	if sectionID != "" {
		id += "#" + sectionID
	}
	doc := Document{SchemaVersion: SchemaVersion, Kind: "topic", ID: id, Title: topic.Title, Summary: topic.Summary, Sections: sections[start:end], Related: topic.Related, Page: metadata}
	return bounded(doc)
}

func Validate() error {
	seenTopics := map[string]bool{}
	seenRefs := map[string]bool{}
	seenHelp := map[string]bool{}
	for _, topic := range topics {
		if !validID(topic.ID) || seenTopics[topic.ID] {
			return fmt.Errorf("invalid or duplicate topic ID %q", topic.ID)
		}
		seenTopics[topic.ID] = true
		if topic.HelpID != "" {
			if !validID(topic.HelpID) || seenHelp[topic.HelpID] {
				return fmt.Errorf("invalid or duplicate help ID %q", topic.HelpID)
			}
			seenHelp[topic.HelpID] = true
		}
		for _, section := range topic.Sections {
			ref := topic.ID + "#" + section.ID
			if !validID(section.ID) || seenRefs[ref] {
				return fmt.Errorf("invalid or duplicate section reference %q", ref)
			}
			seenRefs[ref] = true
		}
	}
	for _, topic := range topics {
		for _, ref := range topic.Related {
			parts := strings.Split(ref, "#")
			if len(parts) > 2 || !seenTopics[parts[0]] || (len(parts) == 2 && !seenRefs[ref]) {
				return fmt.Errorf("unresolved reference %q from %q", ref, topic.ID)
			}
		}
	}
	return nil
}

func searchDocument(query string, page int) Document {
	words := strings.Fields(strings.ToLower(query))
	var results []Topic
	for _, topic := range topics {
		for _, section := range topic.Sections {
			haystack := strings.ToLower(topic.ID + " " + topic.Title + " " + topic.Summary + " " + section.ID + " " + section.Title + " " + section.Content)
			matched := true
			for _, word := range words {
				if !strings.Contains(haystack, word) {
					matched = false
					break
				}
			}
			if matched {
				results = append(results, Topic{ID: topic.ID + "#" + section.ID, Title: section.Title, Summary: snippet(section.Content, query), Kind: topic.Kind, Related: []string{topic.ID}})
			}
		}
	}
	weights := make([]pageWeight, len(results))
	for index, result := range results {
		weights[index] = weigh(result.ID + " " + result.Title + " " + result.Summary)
	}
	start, end, metadata, ok := paginate(weights, page)
	if !ok {
		return errorDocument("page_out_of_range", fmt.Sprintf("search page %d does not exist", page), "spynel docs search "+query+" page "+strconv.Itoa(metadata.Total), nil)
	}
	doc := Document{SchemaVersion: SchemaVersion, Kind: "search", ID: "search", Title: "Documentation search", Query: query, Topics: results[start:end], Page: metadata}
	boundedDoc, _ := bounded(doc)
	return boundedDoc
}

func bounded(doc Document) (Document, error) {
	data, _ := json.Marshal(doc)
	if len(data) > MaxBytes || utf8.RuneCount(data) > MaxRunes {
		return Document{}, fmt.Errorf("documentation page exceeds the %d-byte/%d-rune safety limit", MaxBytes, MaxRunes)
	}
	return doc, nil
}

func paginate(weights []pageWeight, page int) (int, int, Page, bool) {
	const framingReserve = 4 * 1024
	ranges := [][2]int{}
	for start := 0; start < len(weights); {
		end, tokens, bytes, runes := start, 0, 0, 0
		for end < len(weights) && end-start < MaxEntries {
			weight := weights[end]
			if end > start && (tokens+weight.tokens > DefaultTokenBudget || bytes+weight.bytes > MaxBytes-framingReserve || runes+weight.runes > MaxRunes-framingReserve) {
				break
			}
			tokens += weight.tokens
			bytes += weight.bytes
			runes += weight.runes
			end++
		}
		if end == start {
			end++
		}
		ranges = append(ranges, [2]int{start, end})
		start = end
	}
	if len(ranges) == 0 {
		ranges = append(ranges, [2]int{0, 0})
	}
	meta := Page{Number: page, Total: len(ranges), TotalEntries: len(weights), MaxEntries: MaxEntries}
	if page < 1 || page > len(ranges) {
		return 0, 0, meta, false
	}
	start, end := ranges[page-1][0], ranges[page-1][1]
	meta.EntryStart, meta.EntryEnd = start+1, end
	if len(weights) == 0 {
		meta.EntryStart = 0
	}
	if len(ranges) > 1 {
		meta.TokenBudget = DefaultTokenBudget
		for _, weight := range weights[start:end] {
			meta.EstimatedTokens += weight.tokens
		}
	}
	return start, end, meta, true
}

// estimateTokens deliberately overestimates ordinary English/code text at one
// token per three Unicode runes. Pagination only occurs between complete
// records, so paragraphs, code blocks, tables, and JSON items are never split.
func estimateTokens(text string) int {
	runes := utf8.RuneCountInString(text)
	if runes == 0 {
		return 0
	}
	return (runes + 2) / 3
}

func weigh(text string) pageWeight {
	// Measure the actual raw and JSON string representations instead of
	// charging every character for JSON's longest possible escape. The larger
	// representation plus a per-record allowance covers keys, indentation,
	// punctuation, and Markdown list/heading framing.
	jsonText, _ := json.Marshal(text)
	rawBytes, rawRunes := len(text), utf8.RuneCountInString(text)
	jsonBytes, jsonRunes := len(jsonText)-2, utf8.RuneCount(jsonText)-2
	return pageWeight{
		tokens: estimateTokens(text),
		bytes:  max(rawBytes, jsonBytes) + 256,
		runes:  max(rawRunes, jsonRunes) + 256,
	}
}

func splitReference(reference string) (string, string) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(reference)), "#", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func renderText(doc Document) string {
	if doc.Error != nil {
		text := "Error [" + doc.Error.Code + "]: " + doc.Error.Message
		if doc.Error.Suggestion != "" {
			text += "\nTry: " + doc.Error.Suggestion
		}
		return text + "\n"
	}
	var lines []string
	lines = append(lines, "# "+doc.Title, "")
	if doc.Summary != "" {
		lines = append(lines, doc.Summary, "")
	}
	if doc.Query != "" {
		lines = append(lines, "Query: `"+doc.Query+"`", "")
	}
	for _, topic := range doc.Topics {
		lines = append(lines, "- `"+topic.ID+"` — "+topic.Title+": "+topic.Summary)
	}
	for _, section := range doc.Sections {
		lines = append(lines, "## "+section.Title+" {#"+section.ID+"}", "", section.Content, "")
	}
	if len(doc.Related) > 0 {
		lines = append(lines, "Related: `"+strings.Join(doc.Related, "`, `")+"`", "")
	}
	lines = append(lines, fmt.Sprintf("Page %d/%d; entries %d-%d of %d.", doc.Page.Number, doc.Page.Total, doc.Page.EntryStart, doc.Page.EntryEnd, doc.Page.TotalEntries))
	if doc.Page.Total > 1 {
		lines = append(lines, fmt.Sprintf("Applied budget: %d estimated tokens; this page: %d (one token per three runes).", doc.Page.TokenBudget, doc.Page.EstimatedTokens))
	}
	if doc.Page.Number < doc.Page.Total {
		if doc.Kind == "topic" {
			lines = append(lines, fmt.Sprintf("Next: `spynel docs %s page %d`", doc.ID, doc.Page.Number+1))
		} else if doc.Kind == "search" {
			lines = append(lines, fmt.Sprintf("Next: `spynel docs search %s page %d`", doc.Query, doc.Page.Number+1))
		} else {
			lines = append(lines, fmt.Sprintf("Next: `spynel docs page %d`", doc.Page.Number+1))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func topicByID(id string) (Topic, bool) {
	for _, topic := range topics {
		if topic.ID == id {
			return topic, true
		}
	}
	return Topic{}, false
}

func canonicalTopic(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	aliases := map[string]string{"config": "configuration", "workflow": "tasks", "instances": "instances-primary", "primary": "instances-primary", "state": "workspace-state"}
	if replacement := aliases[id]; replacement != "" {
		return replacement
	}
	return id
}

func topicIDs() []string {
	ids := make([]string, 0, len(topics))
	for _, topic := range topics {
		ids = append(ids, topic.ID)
	}
	sort.Strings(ids)
	return ids
}

func errorDocument(code, message, suggestion string, valid []string) Document {
	return Document{SchemaVersion: SchemaVersion, Kind: "error", Page: Page{Number: 1, Total: 1}, Error: &Error{Code: code, Message: message, Suggestion: suggestion, Valid: valid}}
}

func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func closest(input string, candidates []string) string {
	best, distance := "", 1<<30
	for _, candidate := range candidates {
		d := editDistance(input, candidate)
		if d < distance {
			best, distance = candidate, d
		}
	}
	if distance > 4 && !strings.HasPrefix(best, input) {
		return ""
	}
	return best
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	row := make([]int, len(br)+1)
	for j := range row {
		row[j] = j
	}
	for i, ca := range ar {
		prev := row[0]
		row[0] = i + 1
		for j, cb := range br {
			old := row[j+1]
			cost := 0
			if ca != cb {
				cost = 1
			}
			row[j+1] = min(row[j+1]+1, row[j]+1, prev+cost)
			prev = old
		}
	}
	return row[len(br)]
}

func snippet(content, query string) string {
	content = strings.Join(strings.Fields(content), " ")
	if len([]rune(content)) <= 180 {
		return content
	}
	return string([]rune(content)[:177]) + "..."
}
