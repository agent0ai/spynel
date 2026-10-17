package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRecoveryProjectsLargeBodiesAndValidatesOmittedSuffix(t *testing.T) {
	body := strings.Repeat(`escaped \"\u4e16界 `, 400000)
	raw, _ := json.Marshal(Entry{Role: "assistant", Content: body, Covers: []string{"pending"}, Outcome: "terminal_assistant"})
	entry, size, err := readCorrelationEntry(bufio.NewReader(strings.NewReader(string(raw) + "\n")))
	if err != nil || size != len(raw)+1 || !entry.IsContentExcerpt() || len(entry.Content) > 32772 || len(entry.Covers) != 1 || entry.Covers[0] != "pending" {
		t.Fatalf("body projection lost bounds or coverage: size=%d content=%d covers=%v err=%v", size, len(entry.Content), entry.Covers, err)
	}
	for _, suffix := range []string{`\q`, `\u00zz`, "\t", "\n", `\`} {
		input := `{"content":"` + strings.Repeat("x", 40000) + suffix + `","covers":["pending"]}`
		if _, _, err := readCorrelationEntry(bufio.NewReader(strings.NewReader(input))); !errors.Is(err, ErrRecoveryCorrupt) {
			t.Fatalf("accepted corruption in omitted body suffix %q: %v", suffix, err)
		}
	}
	for _, raw := range []string{`{"content":"ok","covers":["source"],"role":"user"}`, `{"content":null,"final_text":"\u4e16\u754c"}`, `{"nested":{"content":"nested"},"content":"ok"}`} {
		var expected Entry
		if err := json.Unmarshal([]byte(raw), &expected); err != nil {
			t.Fatal(err)
		}
		actual, _, err := readCorrelationEntry(bufio.NewReader(strings.NewReader(" \n" + raw)))
		if err != nil || actual.Content != expected.Content || actual.Role != expected.Role {
			t.Fatalf("projection changed ordinary record: %#v %v", actual, err)
		}
	}
	if _, _, err := readCorrelationEntry(bufio.NewReader(strings.NewReader(" \n\t"))); err != io.EOF {
		t.Fatalf("blank tail: %v", err)
	}
}

func TestRecoveryPagesAndTargetedReservationPreserveAllWork(t *testing.T) {
	store := New(t.TempDir())
	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		if _, err := store.Append("cli", "pages", Entry{At: now, AcceptedAt: now, Role: "user", SourceMessageID: fmt.Sprint(i), Content: "request"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Append("cli", "pages", Entry{At: now, Role: "assistant", Content: strings.Repeat("x", 5<<20)}); err != nil {
			t.Fatal(err)
		}
	}
	// Coverage after the first page must still suppress its source.
	store.Append("cli", "pages", Entry{At: now.Add(-48 * time.Hour), Covers: []string{"0"}, Outcome: "intentional_cancellation"})
	var ids []string
	pages := 0
	for after := 0; ; {
		page, err := store.RecoveryPage("cli", "pages", now.Add(-time.Hour), after, 3)
		if err != nil || page.Records > 3 || page.More && page.Next <= after {
			t.Fatalf("page did not progress: %#v %v", page, err)
		}
		for _, entry := range page.Entries {
			if entry.SourceMessageID == "0" && page.Covered["0"] != "intentional_cancellation" {
				t.Fatal("lost late old-timestamp cancellation")
			}
			ids = append(ids, entry.SourceMessageID)
		}
		pages++
		if !page.More {
			break
		}
		after = page.Next
	}
	if len(ids) != 7 || pages < 4 {
		t.Fatalf("lost pending work: ids=%v pages=%d", ids, pages)
	}
	reserved, err := store.ReserveRetrigger("cli", "pages", ids, now.Add(-time.Minute), now.Add(-time.Hour), 3)
	if err != nil || len(reserved) != 6 {
		t.Fatalf("reservation depended on selection page: count=%d %v", len(reserved), err)
	}
	reserved, err = store.ReserveRetrigger("cli", "pages", ids, now.Add(-time.Minute), now.Add(-time.Hour), 3)
	if err != nil || len(reserved) != 0 {
		t.Fatalf("repeated reservation: count=%d %v", len(reserved), err)
	}
}

func TestLongValidBodyDoesNotBlockAdmissionOrNotificationLedger(t *testing.T) {
	store := New(t.TempDir())
	for _, entry := range []Entry{
		{Role: "assistant", Content: strings.Repeat("x", 5<<20)},
		{Role: "user", SourceMessageID: "accepted", Content: "new request"},
		{Role: "assistant", Sender: "Spy", EventID: "receipt", Content: "delivered"},
	} {
		if _, err := store.Append("cli", "large", entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"accepted", "new"} {
		found, err := store.HasUserSourceID("cli", "large", id)
		if err != nil || found != (id == "accepted") {
			t.Fatalf("ordinary admission blocked by old body: %t %v", found, err)
		}
	}
	if state, err := store.DeliveryState("cli", "large", "receipt"); err != nil || state != "sent" {
		t.Fatalf("notification receipt hidden by old body: %q %v", state, err)
	}
}

func TestRecentContextProjectsOversizedBodyAndKeepsNewerMessages(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprint(newer), func(t *testing.T) {
			store := New(t.TempDir())
			for _, entry := range []Entry{{Role: "user", Content: "older context"}, {Role: "assistant", Content: strings.Repeat("large response ", 400000)}} {
				if _, err := store.Append("cli", "context", entry); err != nil {
					t.Fatal(err)
				}
			}
			if newer {
				store.Append("cli", "context", Entry{Role: "user", Content: "newest question"})
			}
			context, _, err := store.RecentBounded("cli", "context", 3, 12000)
			if err != nil || len([]rune(context)) > 12000 || !newer && (!strings.Contains(context, "large response") || !strings.Contains(context, "Body excerpt")) || newer && !strings.Contains(context, "newest question") {
				t.Fatalf("large body discarded recent context: characters=%d newer=%t err=%v", len([]rune(context)), newer, err)
			}
		})
	}
}
