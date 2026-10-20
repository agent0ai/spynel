package history

import (
	"os"
	"testing"
)

func TestHasUserSourceIDsValidatesCompleteHistory(t *testing.T) {
	store := New(t.TempDir())
	for _, source := range []string{"first", "second"} {
		if _, err := store.Append("cli", "synthetic", Entry{Role: "user", SourceMessageID: source}); err != nil {
			t.Fatal(err)
		}
	}
	if found, err := store.HasUserSourceIDs("cli", "synthetic", []string{"first", "second", "first"}); err != nil || !found {
		t.Fatal("complete source set was not found", err)
	}
	if found, err := store.HasUserSourceIDs("cli", "synthetic", []string{"first", "missing"}); err != nil || found {
		t.Fatal("missing source was accepted", err)
	}
	file, err := os.OpenFile(store.Path("cli", "synthetic"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("corrupt tail\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if found, err := store.HasUserSourceIDs("cli", "synthetic", []string{"first", "second"}); err == nil || found {
		t.Fatal("corruption after found sources was ignored")
	}
}
