package core

import "strings"

// ParseCommand shares the application grammar with remote intake classification.
func ParseCommand(channel, text string) (string, string) {
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/"))
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return "", ""
	}
	command := strings.ToLower(parts[0])
	if channel == "telegram" {
		command = strings.SplitN(command, "@", 2)[0]
	}
	return command, strings.TrimSpace(strings.TrimPrefix(line, parts[0]))
}

// IsCommand recognizes authored commands, never generated speech or quoted text.
// Authorization and provider addressing still belong to the adapter.
func IsCommand(channel, text string) bool {
	if !strings.HasPrefix(strings.TrimSpace(text), "/") {
		return false
	}
	name, _ := ParseCommand(channel, text)
	switch name {
	case "":
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/")) == ""
	case "commands", "logs", "todo", "extensions", "exit":
		return true
	case "start":
		return channel == "telegram"
	}
	for _, command := range slashCommands {
		candidate, _ := ParseCommand(channel, command.Value)
		if name == candidate {
			return true
		}
	}
	return false
}

// SlashCommands returns a copy of the shared application command catalog.
func SlashCommands() []SlashCommand { return append([]SlashCommand(nil), slashCommands...) }

var slashCommands = []SlashCommand{
	{Value: "/help", Usage: "/help", Description: "Show the help topic index"},
	{Value: "/help about", Usage: "/help about", Description: "Learn what Spynel does and where it stores state"},
	{Value: "/help commands", Usage: "/help commands", Description: "Show the complete slash-command reference"},
	{Value: "/help extensions", Usage: "/help extensions", Description: "Learn how trusted project extensions work"},
	{Value: "/help config", Usage: "/help config", Description: "Understand .spynel/config.yaml and path resolution"},
	{Value: "/help channels", Usage: "/help channels", Description: "Learn about the TUI, Telegram, and WhatsApp"},
	{Value: "/help workflows", Usage: "/help workflows", Description: "Learn how tasks, goals, and scans work"},
	{Value: "/status", Usage: "/status", Description: "Show work, runtime, channel, and orchestrator state"},
	{Value: "/primary", Usage: "/primary", Description: "Safely make this TUI instance the workspace primary"},
	{Value: "/welcome", Usage: "/welcome", Description: "Print the Spynel welcome guide in this conversation"},
	{Value: "/config", Usage: "/config", Description: "Open or show Spynel configuration"},
	{Value: "/config get ", Usage: "/config get <key>", Description: "Read one configuration value"},
	{Value: "/config set ", Usage: "/config set <key> <value>", Description: "Persist one configuration value"},
	{Value: "/harness ", Usage: "/harness [name]", Description: "Show or select the coding harness"},
	{Value: "/model ", Usage: "/model [name]", Description: "Show or select the harness model"},
	{Value: "/effort ", Usage: "/effort [level|inherit]", Description: "Show, select, or reset reasoning effort"},
	{Value: "/speed ", Usage: "/speed [mode|inherit]", Description: "Show, select, or reset a model service mode"},
	{Value: "/theme", Usage: "/theme [name]", Description: "Preview or select a color theme"},
	{Value: "/telegram", Usage: "/telegram", Description: "Open or show Telegram configuration"},
	{Value: "/telegram on", Usage: "/telegram on", Description: "Enable Telegram"},
	{Value: "/telegram off", Usage: "/telegram off", Description: "Disable Telegram"},
	{Value: "/telegram set ", Usage: "/telegram set <key> <value>", Description: "Persist a Telegram setting"},
	{Value: "/whatsapp", Usage: "/whatsapp", Description: "Open or show WhatsApp configuration"},
	{Value: "/whatsapp on", Usage: "/whatsapp on", Description: "Enable WhatsApp"},
	{Value: "/whatsapp off", Usage: "/whatsapp off", Description: "Disable WhatsApp"},
	{Value: "/whatsapp set ", Usage: "/whatsapp set <key> <value>", Description: "Persist a WhatsApp setting"},
	{Value: "/title ", Usage: "/title <name>", Description: "Rename and persist this TUI window"},
	{Value: "/new", Usage: "/new", Description: "Start a distinct TUI conversation and preserve this one"},
	{Value: "/stop", Usage: "/stop", Description: "Stop the active execution for this conversation"},
	{Value: "/restart", Usage: "/restart", Description: "Restart Spynel and restore saved state"},
	{Value: "/update", Usage: "/update", Description: "Update and restart all instances of this installation"},
	{Value: "/update check", Usage: "/update check", Description: "Check versions without updating or restarting"},
	{Value: "/history", Usage: "/history", Description: "Show the complete history file"},
	{Value: "/resume", Usage: "/resume", Description: "Browse saved conversations and branch one into the TUI"},
	{Value: "/log", Usage: "/log", Description: "Show the newest page of captured runtime logs"},
	{Value: "/log page ", Usage: "/log page <number|start-end>", Description: "Show one or a range of available runtime log pages"},
	{Value: "/log search ", Usage: "/log search <text>", Description: "Search captured runtime logs"},
	{Value: "/log clear", Usage: "/log clear", Description: "Clear captured runtime logs"},
	{Value: "/jobs", Usage: "/jobs", Description: "List running agent jobs"},
	{Value: "/jobs recent", Usage: "/jobs recent", Description: "List recent job archives by number"},
	{Value: "/job info ", Usage: "/job info <number>", Description: "Show bounded live or archived job metadata"},
	{Value: "/job output ", Usage: "/job output <number> [tail <bytes>]", Description: "Show bounded captured job output"},
	{Value: "/job message ", Usage: "/job message <number> <text>", Description: "Guide a running orchestrator job without replacing it"},
	{Value: "/job ping ", Usage: "/job ping <number>", Description: "Request a durable progress update from a running job"},
	{Value: "/job kill ", Usage: "/job kill <number>", Description: "Stop a running agent job by number"},
	{Value: "/tasks", Usage: "/tasks [view] [options]", Description: "List open tasks or select a semantic view"},
	{Value: "/goals", Usage: "/goals [view] [options]", Description: "List open goals or select a semantic view"},
	{Value: "/clear", Usage: "/clear", Description: "Clear this conversation's history and harness thread"},
	{Value: "/task ", Usage: "/task <request>", Description: "Ask the communication agent to create or refine a finite task"},
	{Value: "/goal ", Usage: "/goal <objective>", Description: "Ask the communication agent to create or refine a measurable goal"},
	{Value: "/trigger", Usage: "/trigger [process]", Description: "List or start a triggerable background process"},
	{Value: "/trigger orchestrator", Usage: "/trigger orchestrator", Description: "Request an immediate safe orchestrator pass"},
	{Value: "/trigger heartbeat", Usage: "/trigger heartbeat", Description: "Start the semantic heartbeat when idle"},
	{Value: "/cleanup ", Usage: "/cleanup [days]", Description: "Remove old conversations and job archives; archive old terminal tasks"},
	{Value: "/extension list", Usage: "/extension list", Description: "List installed project extensions"},
	{Value: "/extension install ", Usage: "/extension install URL", Description: "Install a trusted Git extension"},
	{Value: "/extension remove ", Usage: "/extension remove NAME", Description: "Remove an installed extension"},
	{Value: "/quit", Usage: "/quit", Description: "Exit the TUI only (Telegram and WhatsApp use server lifecycle)"},
}
