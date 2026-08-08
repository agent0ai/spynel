package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/theme"
)

// TestVisualCapture writes deterministic full-frame ANSI renders when
// SPYNEL_CAPTURE_DIR is set. scripts/capture-tui.sh converts them into PNGs so
// visual changes can be reviewed as images instead of inferred from test text.
func TestVisualCapture(t *testing.T) {
	directory := os.Getenv("SPYNEL_CAPTURE_DIR")
	if directory == "" {
		t.Skip("set SPYNEL_CAPTURE_DIR to write visual fixtures")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lipgloss.SetColorProfile(termenv.TrueColor)

	captures := map[string]model{
		"chat":                  visualChatModel(),
		"history-boundary":      visualHistoryBoundaryModel(),
		"fresh":                 visualFreshModel(),
		"chat-scrolled":         visualScrolledChatModel(),
		"composer-expanded":     visualExpandedComposerModel(),
		"help":                  visualHelpModel(),
		"help-narrow":           visualNarrowHelpModel(),
		"theme-applied":         visualAppliedThemeModel(),
		"theme-dark":            visualThemedChatModel("nord"),
		"theme-cb-dark":         visualThemedChatModel("github-colorblind-dark"),
		"theme-cb-light":        visualThemedChatModel("okabe-ito-light"),
		"inline-code-dark":      visualInlineCodeModel("spynel"),
		"inline-code-light":     visualInlineCodeModel("catppuccin-latte"),
		"hyphen-wrap-dark":      visualHyphenWrapModel("spynel"),
		"hyphen-wrap-light":     visualHyphenWrapModel("catppuccin-latte"),
		"geometry-wrap-dark":    visualGeometryWrapModel("spynel"),
		"geometry-wrap-light":   visualGeometryWrapModel("catppuccin-latte"),
		"geometry-wrap-tiny":    visualTinyGeometryModel(),
		"geometry-wrap-narrow":  visualNarrowGeometryModel(),
		"geometry-wrap-emoji":   visualExtendedGraphemeGeometryModel(),
		"geometry-wrap-space":   visualExactFitSpaceGeometryModel(),
		"geometry-wrap-padding": visualPaddingYieldGeometryModel(),
		"theme-picker":          visualThemeModel(),
		"welcome":               visualWelcomeModel(),
		"welcome-manual":        visualManualWelcomeModel(),
		"config":                visualConfigModel(),
		"config-advanced":       visualAdvancedConfigModel(),
		"telegram-config":       visualTelegramConfigModel(),
		"whatsapp-config":       visualWhatsAppConfigModel(),
		"wizard":                visualWizardModel(),
		"whatsapp-wizard":       visualWhatsAppWizardModel(),
		"whatsapp-qr":           visualWhatsAppQRModel(),
		"resume":                visualResumeModel(),
		"jobs":                  visualJobsModel(),
	}
	for _, palette := range theme.Builtins() {
		captures["theme-"+palette.Name] = visualThemedChatModel(palette.Name)
	}
	for name, value := range captures {
		if err := os.WriteFile(filepath.Join(directory, name+".ansi"), []byte(value.View()), 0o600); err != nil {
			t.Fatalf("write %s capture: %v", name, err)
		}
	}
}

func visualJobsModel() model {
	value := visualBaseModel()
	value.transcript = []transcriptEntry{
		{role: "user", text: "/jobs"},
		{role: "assistant", text: "# Jobs\n\n- **Job 1** 20260808-release-check.md  \n  1m24s 13▶ 4↻ · running · orchestrator/markdown\n- **Job 2** Check the Unicode 雪 output  \n  18s 1▶ · reconnecting 2/5 · telegram/42\n\nUse `/job info <number>` to inspect a job.\nUse `/job kill <number>` to stop a job."},
	}
	value.renderHistory()
	value.viewport.GotoBottom()
	return value
}

func visualFreshModel() model {
	value := visualBaseModel()
	value.runtimeStatus = core.RuntimeStatus{}
	value.renderHistory()
	return value
}

func visualHelpModel() model {
	value := visualBaseModel()
	value.transcript = []transcriptEntry{
		{role: "user", text: "/help"},
		{role: "assistant", text: "# Spynel Help\n\nSpynel connects coding agents to local and remote chat channels.\n\n- `/help about` — What Spynel does\n- `/help commands` — Slash-command reference"},
		{role: "user", text: "/help about"},
		{role: "assistant", text: "# About Spynel\n\nSpynel connects local coding agents to a terminal UI, Telegram, and WhatsApp."},
	}
	value.renderHistory()
	return value
}

func visualNarrowHelpModel() model {
	value := visualBaseModel()
	value.width = 24
	value.height = 20
	value.viewport.Width = 21
	value.viewport.Height = 13
	value.inputWidth = 20
	value.input.SetWidth(value.inputWidth)
	value.transcript = []transcriptEntry{
		{role: "user", text: "/help about"},
		{role: "assistant", text: "# About Spynel\n\nSpynel connects local coding agents to chat."},
	}
	value.renderHistory()
	return value
}

func visualWelcomeModel() model {
	value := visualBaseModel()
	value.welcome = &core.Screen{
		ID: "welcome", Banner: core.SpynelASCII,
		Subtitle: "👋 Hey, I'm **Spynel** — you can call me **Spy**.\n\nI handle tasks and orchestrate agents. Just tell me your objectives and leave the rest to me.\nFeel free to ask me for updates anytime or have me get things done. 👍\n\n- type `/help` if you ever feel lost\n- type `/whatsapp` to connect WhatsApp",
		Markdown: true,
	}
	value.welcomeFocus = true
	value.renderHistory()
	value.viewport.GotoTop()
	return value
}

func visualManualWelcomeModel() model {
	value := visualBaseModel()
	value.transcript = []transcriptEntry{{
		role: "assistant",
		text: core.SpynelLogoMarkdown + "\n\n👋 Hey, I'm **Spynel** — you can call me **Spy**.\n\nI handle tasks and orchestrate agents. Just tell me your objectives and leave the rest to me.\nFeel free to ask me for updates anytime or have me get things done. 👍\n\n- type `/help` if you ever feel lost\n- type `/whatsapp` to connect WhatsApp",
	}}
	value.renderHistory()
	value.viewport.GotoBottom()
	return value
}

func visualExpandedComposerModel() model {
	value := visualScrolledChatModel()
	value.viewport.GotoBottom()
	value.input.SetValue("First line of a longer message.\nSecond line remains visible.\nThird line expands the composer.\nFourth line keeps history anchored.")
	value.resizeComposer()
	return value
}

func visualScrolledChatModel() model {
	value := visualBaseModel()
	for index := 1; index <= 18; index++ {
		value.transcript = append(value.transcript,
			transcriptEntry{role: "user", text: fmt.Sprintf("Review checkpoint %02d and keep the important context visible.", index)},
			transcriptEntry{role: "assistant", text: fmt.Sprintf("Checkpoint **%02d** is recorded with its result and follow-up.", index)},
		)
	}
	value.renderHistory()
	value.viewport.SetYOffset(max(1, value.viewport.TotalLineCount()/2))
	return value
}

func visualAppliedThemeModel() model {
	return visualThemedChatModel("catppuccin-latte")
}

func visualThemedChatModel(name string) model {
	value := visualChatModel()
	selected, _ := theme.Find(theme.Builtins(), name)
	value.applyTheme(selected)
	value.status = "Theme changed to " + name
	return value
}

func visualInlineCodeModel(name string) model {
	value := visualBaseModel()
	selected, _ := theme.Find(theme.Builtins(), name)
	value.applyTheme(selected)
	value.transcript = []transcriptEntry{{
		role: "assistant",
		text: "Inline code uses identical padding in the middle: `/log` followed by text.\nThe same command ends this line: `/log`\nMultiple values stay distinct: `15m`, `999`, and Unicode 界`15m`界.",
	}}
	value.invalidateHistoryRender()
	value.renderHistory()
	return value
}

func visualHyphenWrapModel(name string) model {
	value := visualBaseModel()
	selected, _ := theme.Find(theme.Builtins(), name)
	value.applyTheme(selected)
	value.width = 28
	value.height = 16
	value.viewport.Width = 25
	value.viewport.Height = 9
	value.inputWidth = 24
	value.input.SetWidth(value.inputWidth)
	value.transcript = []transcriptEntry{{
		role: "assistant",
		text: "Use the familiar editor-style Up/Down controls.\nThen choose scroll-to-bottom or Ctrl-C-safe mode.",
	}}
	value.invalidateHistoryRender()
	value.renderHistory()
	return value
}

func visualGeometryWrapModel(name string) model {
	value := visualBaseModel()
	selected, _ := theme.Find(theme.Builtins(), name)
	value.applyTheme(selected)
	value.width = 42
	value.height = 22
	value.viewport.Width = 39
	value.viewport.Height = 10
	value.inputWidth = 38
	value.input.SetWidth(value.inputWidth)
	value.transcript = []transcriptEntry{
		{role: "user", text: "Run every 15 minutes by default, configurable from the config screen. Ordinary fitting words should move intact."},
		{role: "assistant", text: "The semantic workflow heartbeat checks tasks and jobs. It should preserve `config` and avoid anomalously short rows."},
	}
	value.input.SetValue("A wrapped composer keeps this final cursor visible while history remains anchored.")
	value.resizeComposer()
	value.invalidateHistoryRender()
	value.renderHistory()
	value.viewport.SetYOffset(max(0, value.viewport.TotalLineCount()-value.viewport.Height-2))
	return value
}

func visualTinyGeometryModel() model {
	value := visualBaseModel()
	value.input.SetValue(strings.Repeat("hidden line\n", 10) + "Final cursor row")
	next, _ := value.Update(tea.WindowSizeMsg{Width: 24, Height: 5})
	return next.(model)
}

func visualNarrowGeometryModel() model {
	value := visualBaseModel()
	value.input.SetValue(strings.Repeat("hidden\n", 10) + "Z")
	next, _ := value.Update(tea.WindowSizeMsg{Width: 12, Height: 5})
	return next.(model)
}

func visualExtendedGraphemeGeometryModel() model {
	value := visualBaseModel()
	// Six terminal cells leave the exact two-cell composer content width that
	// previously split this fitting family cluster into rune fragments.
	next, _ := value.Update(tea.WindowSizeMsg{Width: 6, Height: 14})
	value = next.(model)
	for _, r := range []rune("👨‍👩‍👧‍👦") {
		next, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		value = next.(model)
	}
	return value
}

func visualExactFitSpaceGeometryModel() model {
	value := visualBaseModel()
	// Eight terminal cells leave a four-cell composer content width. The token
	// fills it exactly; its following space and cursor belong on the next row.
	next, _ := value.Update(tea.WindowSizeMsg{Width: 8, Height: 14})
	value = next.(model)
	next, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abcd")})
	value = next.(model)
	next, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	return next.(model)
}

func visualPaddingYieldGeometryModel() model {
	value := visualBaseModel()
	// Four terminal cells leave only the two border cells and two content
	// cells. Cosmetic padding yields so this fitting wide grapheme remains
	// intact while its exact-fit cursor occupies the following visual row.
	next, _ := value.Update(tea.WindowSizeMsg{Width: 4, Height: 14})
	value = next.(model)
	next, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("界")})
	return next.(model)
}

func visualBaseModel() model {
	value := testModel()
	value.title = "API workspace"
	value.connection = connectionMap([]channel.ConnectionStatus{
		{Name: "telegram", State: channel.ConnectionConnected},
		{Name: "whatsapp", State: channel.ConnectionConnecting},
	})
	value.runtimeStatus = core.RuntimeStatus{Jobs: 2, Logs: 18}
	value.width = 120
	value.height = 34
	value.viewport.Width = 117
	value.viewport.Height = 27
	value.inputWidth = 116
	value.input.SetWidth(value.inputWidth)
	value.input.SetHeight(maxComposerHeight)
	value.composerRows = 1
	return value
}

func visualChatModel() model {
	value := visualBaseModel()
	value.transcript = []transcriptEntry{
		{role: "user", text: "Summarize the release and call out anything that still needs attention."},
		{role: "assistant", text: "## Release overview\n\nThe channel supervisor is healthy and the new onboarding flow is ready for review.\n\n- Telegram is connected\n- WhatsApp is pairing\n- Two background jobs are active\n\n```go\nstatus := runtime.Snapshot()\n```\n\nSee [configuration](/workspace/spynel/docs/configuration.md:56) for the remaining rollout notes."},
		{role: "user", text: "Great—keep the terminal readable while the response streams."},
	}
	value.streaming = "I’ll keep the hierarchy compact and preserve the current scroll position."
	value.working = true
	value.status = "Harness working"
	value.renderHistory()
	return value
}

func visualHistoryBoundaryModel() model {
	value := visualBaseModel()
	value.width = 109
	value.height = 14
	value.viewport.Width = 106
	value.viewport.Height = 7
	value.inputWidth = 105
	value.input.SetWidth(value.inputWidth)
	value.transcript = []transcriptEntry{
		{role: "user", text: "/jobs"},
		{role: "assistant", text: "One background job is active."},
		{role: "user", text: "Keep the existing themes unchanged."},
		{role: "assistant", text: "Got it — I’ll update the active theme-rebalance job so the existing `spynel` and `hack-the-box` themes are explicitly preserved.\nUpdated the active job: the existing `Spynel` and `Hack The Box` themes and their palettes must remain unchanged."},
	}
	value.renderHistory()
	value.viewport.GotoBottom()
	return value
}

func visualThemeModel() model {
	value := visualBaseModel()
	value.transcript = []transcriptEntry{
		{role: "user", text: "/theme"},
		{role: "assistant", text: "Choose a palette to preview."},
	}
	value.themes = theme.Builtins()
	value.renderHistory()
	value.openThemeMenu()
	_, _ = value.handleThemeMenuKey(tea.KeyMsg{Type: tea.KeyDown})
	return value
}

func visualConfigModel() model {
	value := visualBaseModel()
	value.status = "Editing configuration"
	value.openScreen(core.Screen{
		ID: "config",
		Controls: []core.ScreenControl{
			{Key: "harness", Section: "Core settings", Kind: "action", Value: "Coding harness · Codex", Description: "Select Codex or Claude Code"},
			{Key: "model", Kind: "action", Value: "Model · GPT-5", Description: "Choose from the harness model catalog"},
			{Key: "harness.sandbox", Label: "agent access", Kind: "select", Value: "danger-full-access", Options: []string{"danger-full-access", "workspace-write", "read-only"}, Description: "Filesystem access granted to the coding agent"},
			{Key: "workspace.history_max_messages", Label: "context messages", Kind: "text", Value: "50", Description: "Maximum recent messages passed to the harness"},
			{Key: "workspace.history_char_limit", Label: "context characters", Kind: "text", Value: "12000", Description: "Maximum total history characters passed to the harness"},
			{Key: "startup.enabled", Label: "run at startup", Kind: "toggle", Value: "off", Options: []string{"on", "off"}, Description: "Run Spynel automatically for this workspace"},
			{Key: "advanced", Section: "Advanced settings", Kind: "disclosure", Value: "Advanced settings", Description: "Show task, speech, channel, extension, and storage controls"},
			{Key: "tasks.enabled", Label: "task loop", Kind: "toggle", Value: "on", Options: []string{"on", "off"}, Description: "Process durable task documents", Advanced: true},
			{Key: "speech.enabled", Label: "speech transcription", Kind: "toggle", Value: "on", Options: []string{"on", "off"}, Description: "Transcribe supported voice attachments", Advanced: true},
		},
	})
	return value
}

func visualAdvancedConfigModel() model {
	value := visualConfigModel()
	value.screenAdvanced = true
	value.screenIndex = 6
	return value
}

func visualTelegramConfigModel() model {
	value := visualBaseModel()
	value.status = "Editing Telegram"
	value.connection = connectionMap([]channel.ConnectionStatus{
		{Name: "telegram", State: channel.ConnectionConnected, Identity: "@spynel_bot", Link: "https://t.me/spynel_bot"},
		{Name: "whatsapp", State: channel.ConnectionConnecting},
	})
	value.openScreen(core.Screen{
		ID: "telegram",
		Controls: []core.ScreenControl{
			{Key: "channels.telegram.enabled", Label: "enabled", Kind: "toggle", Value: "on", Options: []string{"on", "off"}, Description: "Start the Telegram connection"},
			{Key: "wizard", Section: "Setup", Kind: "action", Value: "Setup wizard", Description: "Configure the essential connection step by step"},
			{Key: "channels.telegram.token", Section: "Basic settings", Label: "bot token", Kind: "password", Value: "", Secret: true, Configured: true, Description: "Telegram bot token generated by BotFather"},
			{Key: "channels.telegram.allowed_users", Label: "allowed users", Kind: "text", Value: "123456789", Description: "Telegram user IDs or usernames allowed to message the bot"},
			{Key: "advanced", Section: "Advanced settings", Kind: "disclosure", Value: "Advanced settings", Description: "Show optional connection, group, notification, and storage controls"},
		},
	})
	return value
}

func visualWhatsAppConfigModel() model {
	value := visualBaseModel()
	value.status = "Editing WhatsApp"
	value.connection = connectionMap([]channel.ConnectionStatus{
		{Name: "telegram", State: channel.ConnectionConnected},
		{Name: "whatsapp", State: channel.ConnectionConnected, Detail: "Paired device online", Identity: "+15551234567", Link: "https://wa.me/15551234567"},
	})
	value.openScreen(core.Screen{
		ID: "whatsapp",
		Controls: []core.ScreenControl{
			{Key: "channels.whatsapp.enabled", Label: "enabled", Kind: "toggle", Value: "on", Options: []string{"on", "off"}, Description: "Start the WhatsApp connection"},
			{Key: "wizard", Section: "Setup", Kind: "action", Value: "Setup wizard", Description: "Configure the essential connection step by step"},
			{Key: "channels.whatsapp.mode", Section: "Basic settings", Label: "mode", Kind: "select", Value: "self-chat", Options: []string{"self-chat", "dedicated"}, Description: "Choose the account behavior"},
			{Key: "channels.whatsapp.allowed_numbers", Label: "allowed numbers", Kind: "text", Value: "15551234567", Description: "Required phone numbers allowed to message Spynel"},
			{Key: "advanced", Section: "Advanced settings", Kind: "disclosure", Value: "Advanced settings", Description: "Show optional connection, group, notification, and storage controls"},
		},
	})
	return value
}

func visualResumeModel() model {
	value := visualBaseModel()
	value.status = "Resume a conversation"
	value.openScreen(core.Screen{
		ID: "resume", Title: "Resume a conversation", SaveDisabled: true,
		Hints: []core.ScreenHint{
			{Key: "↑↓/⇥", Action: "nav"},
			{Key: "␠/↵", Action: "choose"},
			{Key: "⌦", Action: "delete"},
			{Key: "␛", Action: "exit"},
		},
		Controls: []core.ScreenControl{
			{Key: "resume:telegram", Kind: "action", Value: "TG   2026-08-07 15:21  TG-1029384756.jsonl", Description: "assistant: The deployment finished and all health checks passed. This deliberately long continuation must be trimmed rather than wrapped onto another row."},
			{Key: "resume:whatsapp", Kind: "action", Value: "WA   2026-08-07 14:03  WA-15551234567.jsonl", Description: "user: Check the background jobs and send me a concise update."},
			{Key: "resume:tui", Kind: "action", Value: "TUI  2026-08-06 19:44  local-a1b2c3d4.jsonl", Description: "assistant: Configuration was saved successfully."},
			{Key: "resume:cli", Kind: "action", Value: "CLI  2026-08-06 18:31  release-check.jsonl", Description: "user: Run the release checks."},
		},
	})
	return value
}

func visualWizardModel() model {
	value := visualBaseModel()
	value.status = "Telegram setup"
	value.openScreen(core.Screen{
		ID: "wizard:telegram:token", Title: "Telegram setup", Markdown: true, SaveDisabled: true,
		Tabs: []string{"Start", "Create bot", "Token", "Access", "Enable"}, ActiveTab: 2,
		Subtitle: "Paste the complete token from [@BotFather](https://t.me/BotFather). It is stored privately and never shown in history.",
		Controls: []core.ScreenControl{
			{Key: "channels.telegram.token", Label: "bot token", Kind: "password", Value: "123456:secret", Secret: true, Description: "Paste the token exactly as BotFather supplied it"},
			{Key: "next", Kind: "action", Value: "Continue", Description: "Keep this token in memory and configure access"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to bot creation"},
			{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return without saving"},
		},
	})
	return value
}

func visualWhatsAppWizardModel() model {
	value := visualBaseModel()
	value.status = "WhatsApp setup"
	value.openScreen(core.Screen{
		ID: "wizard:whatsapp:pair", Title: "WhatsApp setup", Markdown: true, SaveDisabled: true, Status: "WhatsApp is ready to pair",
		Tabs: []string{"Mode", "Access", "Pair"}, ActiveTab: 2,
		Subtitle: "On your primary phone, open **Linked devices → Link a device** (under ⋮ on Android or Settings on iPhone). Open the QR by itself so the terminal can use every available row, or link with a phone-number code instead. Pairing continues in the background when you leave this screen.",
		Controls: []core.ScreenControl{
			{Key: "show_qr", Kind: "action", Value: "Show QR", Description: "Use the full terminal for the QR; press any key to return"},
			{Key: "phone", Kind: "action", Value: "Use pairing code", Description: "Link with the account phone number when QR scanning is unavailable"},
			{Key: "retry", Kind: "action", Value: "Retry pairing", Description: "Refresh immediately instead of waiting for the automatic retry"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to allowed numbers"},
			{Key: "done", Kind: "action", Value: "Done", Description: "Return to WhatsApp settings; pairing continues in the background"},
		},
	})
	return value
}

func visualWhatsAppQRModel() model {
	value := visualBaseModel()
	value.openScreen(core.Screen{
		ID: core.ScreenWhatsAppQR,
		Banner: `██████████████  ██  ████  ██████████████
██          ██    ████    ██          ██
██  ██████  ██  ██    ██  ██  ██████  ██
██  ██████  ██    ██  ██  ██  ██████  ██
██  ██████  ██  ████████  ██  ██████  ██
██          ██  ██  ██    ██          ██
██████████████  ██  ██  ██  ████████████
` + "                  ██████                  " + `
████  ██████████    ██    ████    ██  ██
██  ██  ██      ██      ██    ██████████
  ████  ████████  ████  ██████    ██  ██
██  ██████  ██  ████  ████  ██████████
  ██    ██████████  ████      ████  ████
                ██    ██████  ██      ██
██████████████  ████  ██  ██  ██  ██  ██
██          ██    ██████      ██      ██
██  ██████  ██  ██  ████████  ██████████
` + "██  ██████  ██    ██  ██    ████  ████  " + `
██  ██████  ██  ██████    ██████████  ██
██          ██    ██    ██  ████  ██████
██████████████  ████  ████  ██  ██    ██`,
		SaveDisabled: true,
	})
	return value
}
