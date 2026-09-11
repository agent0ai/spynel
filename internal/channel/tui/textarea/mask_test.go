package textarea

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestMaskedFieldPreservesSourceAndGraphemeHits(t *testing.T) {
	m := New()
	m.Prompt = ""
	m.ShowLineNumbers = false
	m.Mask = true
	m.SetWidth(30)
	m.SetHeight(1)
	m.SetValue("a👩🏽‍💻é secret")
	m.Focus()
	m.SelectAll()
	plain := ansi.Strip(m.View())
	if strings.Contains(plain, "secret") || strings.ContainsAny(plain, "ae👩") || !strings.Contains(plain, "***") {
		t.Fatalf("masked rendering leaked text: %q", plain)
	}
	if m.SelectedText() != "a👩🏽‍💻é secret" || m.Hit(1, 0) != 1 {
		t.Fatal("mask changed source editing or emoji hit")
	}
	m.InsertString("replacement")
	if m.Value() != "replacement" {
		t.Fatal("masked selection replacement failed")
	}
	m.Undo()
	if m.Value() != "a👩🏽‍💻é secret" {
		t.Fatal("masked undo failed")
	}
}
