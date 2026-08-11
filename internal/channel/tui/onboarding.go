package tui

import (
	"context"
	"fmt"

	"github.com/agent0ai/spynel/internal/channel/tui/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/theme"
)

// WorkspaceChoice is the explicit result of resolving an uninitialized launch
// directory that sits below an initialized workspace.
type WorkspaceChoice string

const (
	WorkspaceChoiceExit           WorkspaceChoice = "exit"
	WorkspaceChoiceUseParent      WorkspaceChoice = "use-parent"
	WorkspaceChoiceInitializeHere WorkspaceChoice = "initialize-here"
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
			{Key: "initialize", Kind: "action", Value: "Initialize Spynel in " + root, Description: "Create the private .spynel workspace and its config.yaml"},
			{Key: "exit", Kind: "action", Value: "Exit", Description: "Leave this directory unchanged"},
		},
	}
}

// ParentWorkspaceScreen explains why bare interactive startup cannot silently
// proceed with ordinary ancestor discovery.
func ParentWorkspaceScreen(launchRoot, parentRoot string) core.Screen {
	return core.Screen{
		ID: "workspace-choice", Title: "Choose a Spynel workspace", Banner: core.SpynelASCII,
		Subtitle: fmt.Sprintf("This folder is not initialized:\n%s\n\nSpynel found an initialized parent workspace:\n%s\n\nChoose where this interactive session should run.", launchRoot, parentRoot),
		Required: true, ExitOnAction: true,
		Hints: []core.ScreenHint{
			{Key: "↑↓/⇥", Action: "nav"},
			{Key: "␠/↵", Action: "choose"},
			{Key: "␛", Action: "exit"},
		},
		Controls: []core.ScreenControl{
			{Key: string(WorkspaceChoiceUseParent), Kind: "action", Value: "Use parent workspace", Description: "Switch to " + parentRoot + " and continue there"},
			{Key: string(WorkspaceChoiceInitializeHere), Kind: "action", Value: "Initialize here", Description: "Create a new .spynel workspace in " + launchRoot},
			{Key: string(WorkspaceChoiceExit), Kind: "action", Value: "Exit", Description: "Leave both directories unchanged"},
		},
	}
}

// RunParentWorkspaceChoice presents the pre-startup workspace decision. The
// initialize callback runs only after that action is explicitly selected.
func RunParentWorkspaceChoice(ctx context.Context, launchRoot, parentRoot string, initialize func() error) (WorkspaceChoice, error) {
	return RunParentWorkspaceChoiceWithVersion(ctx, launchRoot, parentRoot, "", initialize)
}

// RunParentWorkspaceChoiceWithVersion presents the pre-startup workspace
// decision with the same embedded build identity as the main TUI.
func RunParentWorkspaceChoiceWithVersion(ctx context.Context, launchRoot, parentRoot, version string, initialize func() error) (WorkspaceChoice, error) {
	m := newRequiredActionModel(ctx, ParentWorkspaceScreen(launchRoot, parentRoot), func(screenID, action string) error {
		if screenID == "workspace-choice" && action == string(WorkspaceChoiceInitializeHere) {
			return initialize()
		}
		return nil
	})
	m.version = headerVersion(version)
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := program.Run()
	if err != nil {
		return WorkspaceChoiceExit, err
	}
	result, ok := final.(model)
	if !ok {
		return WorkspaceChoiceExit, nil
	}
	switch choice := WorkspaceChoice(result.screenResult); choice {
	case WorkspaceChoiceUseParent, WorkspaceChoiceInitializeHere:
		return choice, nil
	default:
		return WorkspaceChoiceExit, nil
	}
}

// RunInitialization presents the initialization screen and returns true only
// after the initialize action succeeds.
func RunInitialization(ctx context.Context, root string, initialize func() error) (bool, error) {
	return RunInitializationWithVersion(ctx, root, "", initialize)
}

// RunInitializationWithVersion presents setup with the same embedded build
// identity as the main TUI.
func RunInitializationWithVersion(ctx context.Context, root, version string, initialize func() error) (bool, error) {
	m := newInitializationModel(ctx, root, initialize)
	m.version = headerVersion(version)
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := program.Run()
	if err != nil {
		return false, err
	}
	result, ok := final.(model)
	return ok && result.screenResult == "initialize", nil
}

func newInitializationModel(ctx context.Context, root string, initialize func() error) model {
	return newRequiredActionModel(ctx, InitializationScreen(root), func(screenID, action string) error {
		if screenID != "initialize" || action != "initialize" {
			return nil
		}
		return initialize()
	})
}

func newRequiredActionModel(ctx context.Context, screen core.Screen, callback func(screenID, action string) error) model {
	// Match the normal TUI entry point. Capability probing is unreliable across
	// docker exec and nested PTYs, and an ASCII/ANSI fallback drops or quantizes
	// the canonical semantic palette before the workspace exists.
	lipgloss.SetColorProfile(termenv.TrueColor)
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
		return nil, callback(screenID, action)
	}
	m.openScreen(screen)
	return m
}
