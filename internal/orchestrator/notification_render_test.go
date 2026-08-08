package orchestrator_test

import (
	"strings"
	"testing"

	markdownfmt "github.com/agent0ai/spynel/internal/markdown"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

func TestTaskNotificationRendersAcrossChannelsWithoutControls(t *testing.T) {
	document := orchestrator.Document{FrontMatter: map[string]any{
		"title": "Unicode ✓ and *Markdown*", "attempt": 1, "review_attempt": 1,
		"notification_summary": map[string]any{
			"verdict": "accepted", "outcome": "All checks passed for `inline code`.", "reviewed_at": "2026-08-08T00:00:00Z",
		},
	}}
	message := orchestrator.FormatTaskNotification(document, "done", "render-id")
	outputs := map[string]string{
		"TUI":      markdownfmt.Terminal(message, 80),
		"Telegram": markdownfmt.TelegramHTML(message),
		"WhatsApp": markdownfmt.WhatsApp(message),
		"CLI":      message,
	}
	for channel, output := range outputs {
		if strings.TrimSpace(output) == "" || strings.Contains(output, "Status: spynel status") || strings.Contains(output, "/workspace/") || strings.Contains(output, "render-id") || strings.Contains(output, "implementation attempt") || strings.ContainsAny(output, "\r\x00") {
			t.Fatalf("%s rendered unsafe notification %q", channel, output)
		}
	}
}
