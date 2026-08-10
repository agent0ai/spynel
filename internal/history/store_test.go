package history

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHistoriesAreIndependentAndBounded(t *testing.T) {
	store := New(t.TempDir())
	now := time.Now().UTC()
	_, _ = store.Append("tui", "local", Entry{At: now, Role: "user", Content: "first conversation secret"})
	_, _ = store.Append("telegram", "42", Entry{At: now, Role: "user", Content: "telegram only"})
	_, _ = store.Append("tui", "local", Entry{At: now, Role: "assistant", Content: strings.Repeat("x", 80)})
	recent, path, err := store.Recent("tui", "local", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(recent)) > 40 || strings.Contains(recent, "telegram only") {
		t.Fatalf("history was not independent and bounded: %q", recent)
	}
	if path != store.Path("tui", "local") {
		t.Fatalf("full history link mismatch: %q", path)
	}
	telegram, _, err := store.Recent("telegram", "42", 1000)
	if err != nil || !strings.Contains(telegram, "telegram only") || strings.Contains(telegram, "secret") {
		t.Fatalf("unexpected Telegram history %q (%v)", telegram, err)
	}
}

func TestRecentBoundedUsesNewestMessageAndCharacterWindow(t *testing.T) {
	store := New(t.TempDir())
	for index := 1; index <= 100; index++ {
		_, _ = store.Append("telegram", "person", Entry{Role: "user", Content: fmt.Sprintf("message-%03d", index)})
	}
	recent, _, err := store.RecentBounded("telegram", "person", 3, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recent, "message-098") || !strings.Contains(recent, "message-100") || strings.Contains(recent, "message-097") {
		t.Fatalf("unexpected bounded message window: %q", recent)
	}
	if disabled, _, err := store.RecentBounded("telegram", "person", 0, 1000); err != nil || disabled != "" {
		t.Fatalf("zero message history was not disabled: %q, %v", disabled, err)
	}
	short, _, err := store.RecentBounded("telegram", "person", 20, 35)
	if err != nil || len([]rune(short)) > 35 || !strings.Contains(short, "message-100") {
		t.Fatalf("unexpected bounded character window: %q, %v", short, err)
	}
}

func TestReplyContextSerializesAndRendersWithinBounds(t *testing.T) {
	store := New(t.TempDir())
	_, err := store.Append("telegram", "person", Entry{Role: "user", ReplyTo: "123 referenced text", Content: strings.Repeat("界", 200)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.Path("telegram", "person"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reply_to":"123 referenced text"`) {
		t.Fatalf("serialized history missing reply_to: %s", data)
	}
	recent, _, err := store.RecentBounded("telegram", "person", 20, 70)
	if err != nil || len([]rune(recent)) > 70 || !strings.Contains(recent, "[reply_to: 123 referenced text]") || !strings.Contains(recent, "…") {
		t.Fatalf("bounded reply history = %q (%v)", recent, err)
	}
	withoutReply := []byte(`{"at":"2026-08-09T00:00:00Z","role":"user","content":"ordinary"}` + "\n")
	if err := os.WriteFile(store.Path("telegram", "ordinary-entry"), withoutReply, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.Entries("telegram", "ordinary-entry")
	if err != nil || len(entries) != 1 || entries[0].ReplyTo != "" || entries[0].Content != "ordinary" {
		t.Fatalf("ordinary history = %#v, %v", entries, err)
	}
}

func TestReplyContextHandlesMaximumPreviewAndTinyLimitsSafely(t *testing.T) {
	store := New(t.TempDir())
	id := "ABCDEFGHIJKLMNOPQRST"
	reply := id + " " + strings.Repeat("🙂", 100) + "…"
	_, _ = store.Append("whatsapp", "person", Entry{Role: "user", ReplyTo: reply, Content: strings.Repeat("界", 100)})
	for _, limit := range []int{31, 32, 35} {
		recent, _, err := store.RecentBounded("whatsapp", "person", 20, limit)
		if err != nil || len([]rune(recent)) > limit || recent != "" {
			t.Fatalf("limit %d produced damaged identity %q (%v)", limit, recent, err)
		}
	}
	recent, _, err := store.RecentBounded("whatsapp", "person", 20, 64)
	if err != nil || len([]rune(recent)) > 64 || !strings.Contains(recent, "[reply_to: "+id) || !strings.Contains(recent, "…") {
		t.Fatalf("compact reply history = %q (%v)", recent, err)
	}
	_, _ = store.Append("whatsapp", "ordinary", Entry{Role: "user", Content: "ordinary"})
	data, _ := os.ReadFile(store.Path("whatsapp", "ordinary"))
	if strings.Contains(string(data), "reply_to") {
		t.Fatalf("ordinary history emitted reply_to: %s", data)
	}
}

func TestConversationIdentifiersCannotEscapeHistoryRoot(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	path := store.Path("../../etc", "../passwd")
	if !strings.HasPrefix(path, root) || strings.Contains(path, "..") {
		t.Fatalf("unsafe history path %q", path)
	}
}

func TestEntriesLoadInOrderAndClearRemovesConversation(t *testing.T) {
	store := New(t.TempDir())
	_, _ = store.Append("tui", "local", Entry{Role: "user", Content: "hello"})
	_, _ = store.Append("tui", "local", Entry{Role: "assistant", Content: "welcome back"})
	_, _ = store.Append("telegram", "42", Entry{Role: "user", Content: "keep this"})

	entries, path, err := store.Entries("tui", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Content != "hello" || entries[1].Content != "welcome back" {
		t.Fatalf("entries = %#v", entries)
	}
	if path != store.Path("tui", "local") {
		t.Fatalf("history path = %q, want %q", path, store.Path("tui", "local"))
	}

	if err := store.Clear("tui", "local"); err != nil {
		t.Fatal(err)
	}
	entries, _, err = store.Entries("tui", "local")
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries after clear = %#v, %v", entries, err)
	}
	if err := store.Clear("tui", "local"); err != nil {
		t.Fatalf("clearing missing history: %v", err)
	}
	other, _, err := store.Entries("telegram", "42")
	if err != nil || len(other) != 1 || other[0].Content != "keep this" {
		t.Fatalf("unrelated history after clear = %#v, %v", other, err)
	}
}

func TestConversationDiscoveryAndBranchingStayDiskBacked(t *testing.T) {
	store := New(t.TempDir())
	for index := 0; index < 20; index++ {
		_, _ = store.Append("telegram", "TG-alice-42", Entry{At: time.Now().Add(time.Duration(index) * time.Second), Role: "user", Content: fmt.Sprintf("message-%02d", index)})
	}
	_, _ = store.Append("whatsapp", "WA-15551234", Entry{At: time.Now().Add(-time.Hour), Role: "assistant", Content: "older conversation"})
	conversations, err := store.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 || conversations[0].Conversation != "TG-alice-42" || conversations[0].Preview != "message-19" {
		t.Fatalf("conversation list = %#v", conversations)
	}
	branch, path, err := store.Branch("telegram", "TG-alice-42")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, "resume-") || path != store.Path("tui", branch) {
		t.Fatalf("branch = %q, %q", branch, path)
	}
	cliBranch, cliPath, err := store.BranchTo("telegram", "TG-alice-42", "cli")
	if err != nil || !strings.HasPrefix(cliBranch, "resume-") || cliPath != store.Path("cli", cliBranch) {
		t.Fatalf("CLI branch = %q, %q, %v", cliBranch, cliPath, err)
	}
	cliTail, _, err := store.RecentEntries("cli", cliBranch, 1, 1000)
	if err != nil || len(cliTail) != 1 || cliTail[0].Content != "message-19" {
		t.Fatalf("CLI branch tail = %#v, %v", cliTail, err)
	}
	tail, _, err := store.RecentEntries("tui", branch, 3, 1000)
	if err != nil || len(tail) != 3 || tail[0].Content != "message-17" || tail[2].Content != "message-19" {
		t.Fatalf("branch tail = %#v, %v", tail, err)
	}
	_, _ = store.Append("tui", branch, Entry{Role: "user", Content: "branch only"})
	source, _, _ := store.RecentEntries("telegram", "TG-alice-42", 1, 1000)
	if len(source) != 1 || source[0].Content != "message-19" {
		t.Fatalf("source changed with branch: %#v", source)
	}
}

func TestConversationDiscoveryKeepsOnlyNewestBoundedMetadata(t *testing.T) {
	store := New(t.TempDir())
	base := time.Now().Add(-time.Hour)
	for index := 0; index < 75; index++ {
		_, _ = store.Append("telegram", fmt.Sprintf("TG-%03d", index), Entry{At: base.Add(time.Duration(index) * time.Second), Role: "user", Content: fmt.Sprintf("message-%03d", index)})
	}
	conversations, err := store.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 10 || conversations[0].Conversation != "TG-074" || conversations[9].Conversation != "TG-065" {
		t.Fatalf("bounded conversation metadata = %#v", conversations)
	}
}

func TestLatestReturnsMostRecentConversationForOneChannel(t *testing.T) {
	store := New(t.TempDir())
	older := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	if _, err := store.Append("tui", "local-old", Entry{At: older, Role: "user", Content: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("telegram", "TG-newest", Entry{At: newer.Add(time.Hour), Role: "user", Content: "remote"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("tui", "local-new", Entry{At: newer, Role: "assistant", Content: "new"}); err != nil {
		t.Fatal(err)
	}
	latest, found, err := store.Latest("tui")
	if err != nil || !found || latest.Channel != "tui" || latest.Conversation != "local-new" || !latest.UpdatedAt.Equal(newer) {
		t.Fatalf("latest TUI conversation = %#v, found = %t, err = %v", latest, found, err)
	}
	if _, found, err := store.Latest("whatsapp"); err != nil || found {
		t.Fatalf("missing latest conversation found = %t, err = %v", found, err)
	}
}
