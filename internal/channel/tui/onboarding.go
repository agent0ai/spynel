package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/theme"
)

// InitializationScreen uses the same form/action contract as runtime config
// screens, but is required because chat cannot start without a workspace.
func InitializationScreen(root string) core.Screen {
	return core.Screen{
		ID: "initialize", Title: "Welcome to Spynel", Banner: core.SpynelASCII,
		Subtitle: fmt.Sprintf("Spynel is not configured in this directory:\n%s\n\nWould you like to initialize it here?", root),
		Required: true, ExitOnAction: true,
		Hints: []core.ScreenHint{
			{Key: "↑↓/⇥", Action: "nav"},
			{Key: "␠/↵", Action: "choose"},
			{Key: "␛", Action: "exit"},
		},
		Controls: []core.ScreenControl{
			{Key: "initialize", Kind: "action", Value: "Initialize Spynel in " + root, Description: "Create spynel.yaml and the private .spynel workspace"},
			{Key: "exit", Kind: "action", Value: "Exit", Description: "Leave this directory unchanged"},
		},
	}
}

// RunInitialization presents the initialization screen and returns true only
// after the initialize action succeeds.
func RunInitialization(ctx context.Context, root string, initialize func() error) (bool, error) {
	input := textarea.New()
	activeTheme := theme.Default()
	styles := stylesFor(activeTheme)
	input.Placeholder = "Select an action"
	styleComposer(&input, styles)
	input.SetHeight(maxComposerHeight)
	input.SetWidth(80)
	m := model{
		ctx: ctx, title: "Spynel", input: input, inputWidth: 80,
		viewport: viewport.New(80, 20), events: make(chan core.Event, 1), composerRows: minComposerHeight,
		logoSpinner: newLogoSpinner(), workingSpinner: newWorkingSpinner(), connection: map[string]channel.ConnectionStatus{},
		themes: []theme.Theme{activeTheme}, activeTheme: activeTheme, styles: styles,
		status: "Setup", width: 80, height: 24, conversation: "local",
	}
	m.screenAction = func(_ context.Context, screenID, action string, _ map[string]string) (*core.Screen, error) {
		if screenID != "initialize" || action != "initialize" {
			return nil, nil
		}
		return nil, initialize()
	}
	screen := InitializationScreen(root)
	m.openScreen(screen)
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := program.Run()
	if err != nil {
		return false, err
	}
	result, ok := final.(model)
	return ok && result.screenResult == "initialize", nil
}
