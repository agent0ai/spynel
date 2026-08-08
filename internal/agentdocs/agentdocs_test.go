package agentdocs

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCatalogReferencesAreStableAndResolvable(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"commands", "tasks", "goals", "reviews", "notifications", "jobs", "logs", "configuration", "channels", "harnesses", "instances-primary", "workspace-state", "security", "troubleshooting", "architecture"}
	for _, id := range want {
		if _, ok := topicByID(id); !ok {
			t.Errorf("missing required topic %q", id)
		}
	}
}

func TestIndexTopicAndSearchPagination(t *testing.T) {
	index, err := Lookup(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if index.Kind != "index" || index.Page.Number != 1 || index.Page.Total != 1 || len(index.Topics) != len(topics) {
		t.Fatalf("index = %#v", index)
	}
	topic, _ := Lookup(Request{Topic: "goals"})
	if topic.Kind != "topic" || topic.ID != "goals" || len(topic.Sections) == 0 || topic.Sections[0].ID == "" {
		t.Fatalf("topic = %#v", topic)
	}
	search, _ := Lookup(Request{Search: "review"})
	if search.Kind != "search" || search.Query != "review" || search.Page.TotalEntries < 2 {
		t.Fatalf("search = %#v", search)
	}
	badPage, _ := Lookup(Request{Topic: "goals", Page: 99})
	if badPage.Error == nil || badPage.Error.Code != "page_out_of_range" || !strings.Contains(badPage.Error.Suggestion, "goals page 1") {
		t.Fatalf("bad page = %#v", badPage)
	}
}

func TestPaginationUsesConservativeTokenBudgetAndRecordBoundaries(t *testing.T) {
	weights := []pageWeight{{tokens: 4_000}, {tokens: 4_000}, {tokens: 4_000}, {tokens: 100}}
	start, end, first, ok := paginate(weights, 1)
	if !ok || start != 0 || end != 2 || first.Total != 2 || first.TokenBudget != DefaultTokenBudget || first.EstimatedTokens != 8_000 {
		t.Fatalf("first page = %d:%d %#v, %t", start, end, first, ok)
	}
	start, end, second, ok := paginate(weights, 2)
	if !ok || start != 2 || end != 4 || second.EstimatedTokens != 4_100 {
		t.Fatalf("second page = %d:%d %#v, %t", start, end, second, ok)
	}
	if got := estimateTokens(strings.Repeat("x", 30_000)); got != DefaultTokenBudget {
		t.Fatalf("token estimate = %d", got)
	}
	byteWeights := []pageWeight{{tokens: 1, bytes: 35_000, runes: 20_000}, {tokens: 1, bytes: 35_000, runes: 20_000}}
	_, end, metadata, ok := paginate(byteWeights, 1)
	if !ok || end != 1 || metadata.Total != 2 {
		t.Fatalf("byte-bounded page = end %d, %#v, %t", end, metadata, ok)
	}
	escaped := weigh("<plain>")
	if escaped.bytes != len(`\u003cplain\u003e`)+256 || escaped.runes != len(`\u003cplain\u003e`)+256 {
		t.Fatalf("representation-aware weight = %#v", escaped)
	}
}

func TestJSONSchemaErrorsSuggestionsAndOutputBounds(t *testing.T) {
	output, err := Render(Request{Topic: "golas", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	var document Document
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != SchemaVersion || document.Kind != "error" || document.Error == nil || document.Error.Code != "unknown_topic" || document.Error.Suggestion != "spynel docs goals" {
		t.Fatalf("error document = %#v", document)
	}
	for _, request := range []Request{{}, {Topic: "tasks"}, {Search: "state"}, {Topic: strings.Repeat("x", 129)}} {
		for _, format := range []string{"text", "json"} {
			request.Format = format
			output, err := Render(request)
			if err != nil {
				t.Fatalf("Render(%#v): %v", request, err)
			}
			if len(output) > MaxBytes || utf8.RuneCountInString(output) > MaxRunes {
				t.Fatalf("unbounded output: %d bytes, %d runes", len(output), utf8.RuneCountInString(output))
			}
		}
	}
	section, _ := Lookup(Request{Topic: "tasks#lifecycle"})
	if section.ID != "tasks#lifecycle" || len(section.Sections) != 1 || section.Sections[0].ID != "lifecycle" {
		t.Fatalf("section reference = %#v", section)
	}
	badSection, _ := Lookup(Request{Topic: "tasks#lifecycl"})
	if badSection.Error == nil || badSection.Error.Code != "unknown_section" || badSection.Error.Suggestion != "spynel docs tasks#lifecycle" {
		t.Fatalf("section suggestion = %#v", badSection)
	}
	malformed, _ := Lookup(Request{Topic: "tasks#"})
	if malformed.Error == nil || malformed.Error.Code != "invalid_reference" {
		t.Fatalf("malformed section reference = %#v", malformed)
	}
	jsonOutput, err := Render(Request{Topic: "goals", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	var sized Document
	if err := json.Unmarshal([]byte(jsonOutput), &sized); err != nil || sized.Page.Bytes != len(jsonOutput) || sized.Page.Runes != utf8.RuneCountInString(jsonOutput) {
		t.Fatalf("JSON size metadata = %#v, actual %d/%d, %v", sized.Page, len(jsonOutput), utf8.RuneCountInString(jsonOutput), err)
	}
}

func TestPlainOutputHasNoTerminalControlsAndPromptPathIsCallable(t *testing.T) {
	output, err := Render(Request{Topic: "commands"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "\x1b") || strings.Contains(output, "\r") {
		t.Fatalf("plain output contains terminal controls: %q", output)
	}
	control, err := Render(Request{Search: "review\x1b[31m"})
	if err != nil || strings.Contains(control, "\x1b") || !strings.Contains(control, "invalid_input") {
		t.Fatalf("control query response = %q, %v", control, err)
	}
	guidance := PromptGuidance()
	if !strings.Contains(guidance, " docs <topic>") || !strings.Contains(guidance, "AGENTS.md") || strings.Count(InjectPromptGuidance(PromptPlaceholder+"\n"+PromptPlaceholder), "docs <topic>") != 1 {
		t.Fatalf("prompt guidance = %q", guidance)
	}
	if runtime.GOOS == "windows" && strings.Contains(guidance, `\\`) {
		t.Fatalf("Windows guidance retained escaped path separators: %q", guidance)
	}
	if got := promptCommand(`C:\Program Files\Spynel\spynel.exe`); got != `"C:/Program Files/Spynel/spynel.exe"` {
		t.Fatalf("portable Windows command = %q", got)
	}
}
