package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/theme"
)

func (s *Service) Screen(id string) (core.Screen, error) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "config":
		return settingsScreen(s.Settings.Snapshot(), "config"), nil
	case "telegram", "whatsapp":
		section := strings.ToLower(strings.TrimSpace(id))
		if s.channelNeedsSetup(section) {
			return s.initialChannelWizard(section), nil
		}
		if section == "whatsapp" {
			if pairing, ok := s.Pairing(section); ok && pairing.State != "" && pairing.State != "connected" {
				return s.whatsappWizardScreen("pair", nil), nil
			}
		}
		return s.channelSettingsScreen(section), nil
	case "welcome":
		return s.WelcomeScreen(), nil
	default:
		return core.Screen{}, fmt.Errorf("unknown screen %q", id)
	}
}

func (s *Service) initialChannelWizard(section string) core.Screen {
	if section == "telegram" {
		return s.telegramWizardScreen("intro", nil)
	}
	return s.whatsappWizardScreen("mode", nil)
}

func (s *Service) channelSettingsScreen(section string) core.Screen {
	screen := settingsScreen(s.Settings.Snapshot(), section)
	if section == "whatsapp" {
		// Pairing QR data belongs only to the dedicated pairing flow, never the
		// general channel configuration surface. The shared connection section
		// already reports health, so pairing success text would be redundant.
		screen.Banner = ""
		screen.Status = ""
	}
	return screen
}

func (s *Service) channelNeedsSetup(section string) bool {
	cfg := s.Settings.Snapshot()
	switch section {
	case "telegram":
		return strings.TrimSpace(cfg.TelegramToken()) == "" || !hasCommaSeparatedValue(strings.Join(cfg.Channels.Telegram.AllowedUsers, ","))
	case "whatsapp":
		return !config.HasAllowedWhatsAppNumber(cfg.Channels.WhatsApp.AllowedNumbers)
	default:
		return false
	}
}

func (s *Service) WelcomeScreen() core.Screen {
	return core.Screen{
		ID: "welcome", Title: "Welcome to Spynel", Banner: core.SpynelASCII,
		Subtitle: s.welcomeText("tui"), Markdown: true,
	}
}

func (s *Service) welcomeText(channelName string) string {
	lines := []string{
		"👋 Hey, I'm **Spynel** — you can call me **Spy**.", "",
		"I handle tasks and orchestrate agents. Just tell me your objectives and leave the rest to me.",
		"Feel free to ask me for updates anytime or have me get things done. 👍",
		"", "- type `/help` if you ever feel lost",
	}
	if channelName == "tui" {
		if s.connectionStatus("telegram").State != channel.ConnectionConnected {
			lines = append(lines, "- type `/telegram` to connect Telegram")
		}
		if s.connectionStatus("whatsapp").State != channel.ConnectionConnected {
			lines = append(lines, "- type `/whatsapp` to connect WhatsApp")
		}
		if availability, ok := s.Harness.(harness.Availability); ok {
			if ready, detail := availability.Available(); !ready {
				lines = append(lines, "", "**Heads up:** The configured coding harness is unavailable: "+detail, "Use `/harness` to select or repair it before sending an objective.")
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (s *Service) welcomeMessage(channelName string) string {
	text := s.welcomeText(channelName)
	if channelName != "tui" {
		return text
	}
	return core.SpynelLogoMarkdown + "\n\n" + text
}

// InitialWelcome returns the welcome screen once per workspace. The marker
// ensures /clear leaves a genuinely empty chat instead of re-onboarding later.
func (s *Service) InitialWelcome() (*core.Screen, error) {
	path := s.Settings.Snapshot().StatePath("welcome-seen")
	if _, err := os.Stat(path); err == nil {
		return nil, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := fsx.AtomicWriteFile(path, []byte("seen\n"), 0o600); err != nil {
		return nil, err
	}
	screen := s.WelcomeScreen()
	return &screen, nil
}

func (s *Service) configurationCommand(message core.Message, section, remainder string, emit core.Emit) error {
	parts := strings.Fields(remainder)
	if len(parts) == 0 {
		if message.Channel == "tui" && emit != nil {
			screen, err := s.Screen(section)
			if err != nil {
				return err
			}
			emit(core.Event{Kind: core.EventScreen, Screen: &screen, Local: true})
			return nil
		}
		return s.localReply(message, formatSettings(s.Settings.Snapshot(), section), emit)
	}
	if (section == "telegram" || section == "whatsapp") && len(parts) == 1 && (strings.EqualFold(parts[0], "on") || strings.EqualFold(parts[0], "off")) {
		key := "channels." + section + ".enabled"
		return s.setSetting(message, key, strings.ToLower(parts[0]), emit)
	}
	action := strings.ToLower(parts[0])
	switch action {
	case "get":
		if len(parts) != 2 {
			return s.localReply(message, configurationUsage(section), emit)
		}
		key := scopedSettingKey(section, parts[1])
		setting, ok := config.SettingByKey(s.Settings.Snapshot(), key)
		if !ok || (section != "config" && setting.Section != section) {
			return s.localReply(message, fmt.Sprintf("Unknown %s setting %q.", section, parts[1]), emit)
		}
		return s.localReply(message, fmt.Sprintf("`%s` = `%s`\n\n%s", setting.Key, setting.Value, setting.Description), emit)
	case "set":
		if len(parts) < 3 {
			return s.localReply(message, configurationUsage(section), emit)
		}
		key := scopedSettingKey(section, parts[1])
		setting, ok := config.SettingByKey(s.Settings.Snapshot(), key)
		if !ok || (section != "config" && setting.Section != section) {
			return s.localReply(message, fmt.Sprintf("Unknown %s setting %q.", section, parts[1]), emit)
		}
		value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(remainder, parts[0])), parts[1]))
		return s.setSetting(message, key, value, emit)
	default:
		return s.localReply(message, configurationUsage(section), emit)
	}
}

func (s *Service) SetPairing(event channel.PairingEvent) {
	if event.Name == "" {
		return
	}
	s.pairingMu.Lock()
	s.pairing[event.Name] = event
	s.pairingMu.Unlock()
	select {
	case <-s.pairingEvents:
	default:
	}
	s.pairingEvents <- event
}

func (s *Service) Pairing(name string) (channel.PairingEvent, bool) {
	s.pairingMu.RLock()
	defer s.pairingMu.RUnlock()
	event, ok := s.pairing[name]
	return event, ok
}

func (s *Service) PairingEvents() <-chan channel.PairingEvent { return s.pairingEvents }

const (
	telegramTokenKey   = "channels.telegram.token"
	telegramAllowedKey = "channels.telegram.allowed_users"
	telegramEnabledKey = "channels.telegram.enabled"
	whatsappModeKey    = "channels.whatsapp.mode"
	whatsappAllowedKey = "channels.whatsapp.allowed_numbers"
	whatsappEnabledKey = "channels.whatsapp.enabled"
	whatsappPhoneKey   = "whatsapp_pairing_phone"
)

func (s *Service) configurationScreenAction(ctx context.Context, screenID, action string, values map[string]string) (*core.Screen, bool, error) { //nolint:gocyclo
	if screenID == "config" && action == "harness" {
		screen := s.HarnessScreen(false)
		screen.ParentID = "config"
		return &screen, true, nil
	}
	if screenID == "config" && action == "model" {
		screen, err := s.modelScreen(ctx)
		if screen != nil {
			screen.ParentID = "config"
		}
		return screen, true, err
	}
	if (screenID == "telegram" || screenID == "whatsapp") && action == "wizard" {
		screen := s.initialChannelWizard(screenID)
		screen.ParentID = screenID
		return &screen, true, nil
	}
	if !strings.HasPrefix(screenID, "wizard:") {
		return nil, false, nil
	}
	parts := strings.Split(screenID, ":")
	if len(parts) != 3 {
		return nil, true, fmt.Errorf("invalid setup wizard screen %q", screenID)
	}
	channelName, step := parts[1], parts[2]
	if action == "cancel" {
		return nil, true, nil
	}
	if action == "done" {
		screen := s.channelSettingsScreen(channelName)
		return &screen, true, nil
	}
	switch channelName {
	case "telegram":
		switch step + ":" + action {
		case "intro:next":
			screen := s.telegramWizardScreen("create", values)
			return &screen, true, nil
		case "create:back":
			screen := s.telegramWizardScreen("intro", values)
			return &screen, true, nil
		case "create:next":
			screen := s.telegramWizardScreen("token", values)
			return &screen, true, nil
		case "token:back":
			screen := s.telegramWizardScreen("create", values)
			return &screen, true, nil
		case "token:next":
			if strings.TrimSpace(values[telegramTokenKey]) == "" && s.Settings.Snapshot().TelegramToken() == "" {
				return nil, true, errors.New("paste the bot token from BotFather before continuing")
			}
			screen := s.telegramWizardScreen("access", values)
			return &screen, true, nil
		case "access:back":
			screen := s.telegramWizardScreen("token", values)
			return &screen, true, nil
		case "access:next":
			if !hasCommaSeparatedValue(values[telegramAllowedKey]) {
				return nil, true, errors.New("add at least one allowed Telegram user before continuing")
			}
			screen := s.telegramWizardScreen("enable", values)
			return &screen, true, nil
		case "enable:back":
			screen := s.telegramWizardScreen("access", values)
			return &screen, true, nil
		case "enable:finish":
			changes := map[string]string{
				telegramAllowedKey: values[telegramAllowedKey],
				telegramEnabledKey: wizardValue(values, telegramEnabledKey, "on"),
			}
			if token := strings.TrimSpace(values[telegramTokenKey]); token != "" {
				changes[telegramTokenKey] = token
			}
			if _, err := s.ApplySettings(changes); err != nil {
				return nil, true, err
			}
			screen := s.channelSettingsScreen("telegram")
			screen.Status = "Telegram setup saved. Connection status will update in the header."
			return &screen, true, nil
		}
	case "whatsapp":
		switch step + ":" + action {
		case "mode:next":
			screen := s.whatsappWizardScreen("access", values)
			return &screen, true, nil
		case "access:back":
			screen := s.whatsappWizardScreen("mode", values)
			return &screen, true, nil
		case "access:next":
			if !hasPhoneNumberValue(values[whatsappAllowedKey]) {
				return nil, true, errors.New("add at least one allowed WhatsApp number before continuing")
			}
			if _, err := s.ApplySettings(map[string]string{
				whatsappModeKey:    wizardValue(values, whatsappModeKey, "self-chat"),
				whatsappAllowedKey: values[whatsappAllowedKey],
				whatsappEnabledKey: "on",
			}); err != nil {
				return nil, true, err
			}
			if pairing, ok := s.Pairing("whatsapp"); ok && pairingNeedsRetry(pairing.State) && s.PairingControl != nil {
				if err := s.PairingControl.RetryPairing("whatsapp"); err != nil {
					return nil, true, err
				}
				s.SetPairing(channel.PairingEvent{Name: "whatsapp", State: "starting", Detail: "Starting a fresh WhatsApp pairing session…"})
			}
			screen := s.whatsappWizardScreen("pair", values)
			return &screen, true, nil
		case "pair:back":
			screen := s.whatsappWizardScreen("access", values)
			return &screen, true, nil
		case "pair:show_qr":
			pairing, ok := s.Pairing("whatsapp")
			if !ok || (pairing.State != "code" && pairing.State != "phone-code") || strings.TrimSpace(pairing.Rendered) == "" {
				return nil, true, errors.New("the WhatsApp QR is not ready; wait a moment or select Retry pairing")
			}
			screen := core.Screen{
				ID: core.ScreenWhatsAppQR, ParentID: "wizard:whatsapp:pair", Banner: pairing.Rendered,
				SaveDisabled: true, StartAtTop: true,
			}
			return &screen, true, nil
		case "pair:phone":
			screen := s.whatsappPhonePairingScreen(values, "")
			return &screen, true, nil
		case "pair:retry":
			if s.PairingControl == nil {
				return nil, true, errors.New("WhatsApp pairing is not running yet")
			}
			if err := s.PairingControl.RetryPairing("whatsapp"); err != nil {
				return nil, true, err
			}
			s.SetPairing(channel.PairingEvent{Name: "whatsapp", State: "starting", Detail: "Starting a fresh WhatsApp pairing session…"})
			screen := s.whatsappWizardScreen("pair", values)
			return &screen, true, nil
		case "phone:generate":
			phone := strings.TrimSpace(values[whatsappPhoneKey])
			if !validWhatsAppPairingPhone(phone) {
				return nil, true, errors.New("enter the WhatsApp account's phone number in international format")
			}
			if s.PairingControl == nil {
				return nil, true, errors.New("WhatsApp pairing is not running yet")
			}
			code, err := s.PairingControl.PairPhone(ctx, "whatsapp", phone)
			if err != nil {
				return nil, true, err
			}
			screen := s.whatsappPhonePairingScreen(values, code)
			return &screen, true, nil
		}
	}
	return nil, true, fmt.Errorf("invalid %s setup wizard action %q on step %q", channelName, action, step)
}

func (s *Service) telegramWizardScreen(step string, values map[string]string) core.Screen {
	cfg := s.Settings.Snapshot()
	token := wizardValue(values, telegramTokenKey, "")
	allowed := wizardValue(values, telegramAllowedKey, strings.Join(cfg.Channels.Telegram.AllowedUsers, ","))
	enabled := wizardValue(values, telegramEnabledKey, "on")
	var state []core.ScreenControl
	screen := core.Screen{
		ID: "wizard:telegram:" + step, Title: "Telegram setup", Markdown: true, SaveDisabled: true, StartAtTop: true,
		Hints: wizardScreenHints(),
		Tabs:  []string{"Start", "Create bot", "Token", "Access", "Enable"}, ActiveTab: wizardStepIndex(step, []string{"intro", "create", "token", "access", "enable"}),
	}
	switch step {
	case "intro":
		screen.Subtitle = "Open Telegram on your phone, tablet, or desktop.\n\nUse Telegram's verified bot-management account: [@BotFather](https://t.me/BotFather).\n\nTreat the token it gives you like a password. [Read Telegram's official bot tutorial](https://core.telegram.org/bots/tutorial#obtain-your-bot-token)."
		screen.Controls = wizardActions("Open BotFather, then continue", false)
		state = telegramWizardState(token, allowed, enabled)
	case "create":
		screen.Subtitle = "In the BotFather chat:\n\n1. Send `/newbot`.\n2. Choose the display name people will see.\n3. Choose a unique username ending in `bot`, such as `my_spynel_bot`.\n\nBotFather will reply with an HTTP API token. Copy that complete token."
		screen.Controls = wizardActions("I have the token", true)
		state = telegramWizardState(token, allowed, enabled)
	case "token":
		screen.Subtitle = "Paste the complete token from BotFather below. It is stored in the private `spynel.yaml` file and never shown in configuration replies or history. Leave it blank only if a token is already configured through the file or environment."
		screen.Controls = []core.ScreenControl{
			{Key: telegramTokenKey, Label: "bot token", Kind: "password", Value: token, Secret: true, Configured: cfg.TelegramToken() != "", Description: "Paste the token exactly as BotFather supplied it"},
			{Key: "next", Kind: "action", Value: "Continue", Description: "Keep this token in memory and choose who may use the bot"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to the BotFather instructions"},
			{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return to Telegram settings without saving"},
		}
		state = []core.ScreenControl{
			{Key: telegramAllowedKey, Kind: "hidden", Value: allowed},
			{Key: telegramEnabledKey, Kind: "hidden", Value: enabled},
		}
	case "access":
		screen.Subtitle = "Enter at least one Telegram numeric user ID or username, separated by commas. The whitelist is required before Telegram can be enabled.\n\nIf you do not know your numeric ID, message [@userinfobot](https://t.me/userinfobot). It is a third-party bot and will see the message you send it."
		screen.Controls = []core.ScreenControl{
			{Key: telegramAllowedKey, Label: "allowed users", Kind: "text", Value: allowed, Description: "Required; find your ID with [@userinfobot](https://t.me/userinfobot) (third-party)", DescriptionMarkdown: true},
			{Key: "next", Kind: "action", Value: "Continue", Description: "Review whether Telegram should be enabled"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to the token step"},
			{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return to Telegram settings without saving"},
		}
		state = []core.ScreenControl{
			{Key: telegramTokenKey, Kind: "hidden", Value: token, Secret: true, Configured: cfg.TelegramToken() != ""},
			{Key: telegramEnabledKey, Kind: "hidden", Value: enabled},
		}
	case "enable":
		screen.Subtitle = "Turn Telegram on to start the bot now. Finishing validates and saves the token, access list, and enabled state together. You can change optional polling, webhook, and group behavior later under Advanced settings."
		screen.Controls = []core.ScreenControl{
			{Key: telegramEnabledKey, Label: "enabled", Kind: "toggle", Value: enabled, Options: []string{"on", "off"}, Description: "Start Telegram after saving"},
			{Key: "finish", Kind: "action", Value: "Save and finish", Description: "Apply all essential Telegram settings atomically"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to the access-list step"},
			{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return to Telegram settings without saving"},
		}
		state = []core.ScreenControl{
			{Key: telegramTokenKey, Kind: "hidden", Value: token, Secret: true, Configured: cfg.TelegramToken() != ""},
			{Key: telegramAllowedKey, Kind: "hidden", Value: allowed},
		}
	}
	screen.Controls = append(screen.Controls, state...)
	return screen
}

func (s *Service) whatsappWizardScreen(step string, values map[string]string) core.Screen {
	cfg := s.Settings.Snapshot()
	mode := wizardValue(values, whatsappModeKey, cfg.Channels.WhatsApp.Mode)
	allowed := wizardValue(values, whatsappAllowedKey, strings.Join(cfg.Channels.WhatsApp.AllowedNumbers, ","))
	var state []core.ScreenControl
	screen := core.Screen{
		ID: "wizard:whatsapp:" + step, Title: "WhatsApp setup", Markdown: true, SaveDisabled: true, StartAtTop: true,
		Hints: wizardScreenHints(),
		Tabs:  []string{"Mode", "Access", "Pair"}, ActiveTab: wizardStepIndex(step, []string{"mode", "access", "pair"}),
	}
	switch step {
	case "mode":
		screen.Subtitle = "Choose how the linked WhatsApp account will be used.\n\n- **Self-chat:** send requests from the account to its own chat; Spynel suppresses reply loops.\n- **Dedicated:** use a separate account as the bot number; messages sent by that linked account are ignored."
		screen.Controls = []core.ScreenControl{
			{Key: whatsappModeKey, Label: "mode", Kind: "select", Value: mode, Options: []string{"self-chat", "dedicated"}, Description: "Choose the account behavior"},
			{Key: "next", Kind: "action", Value: "Continue", Description: "Choose who may message Spynel"},
			{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return to WhatsApp settings without saving"},
		}
		state = []core.ScreenControl{
			{Key: whatsappAllowedKey, Kind: "hidden", Value: allowed},
		}
	case "access":
		screen.Subtitle = "Enter at least one allowed phone number in international format, separated by commas. A leading + or 00, spaces, and punctuation are normalized. Continuing saves these settings and opens secure device pairing."
		screen.Controls = []core.ScreenControl{
			{Key: whatsappAllowedKey, Label: "allowed numbers", Kind: "text", Value: allowed, Description: "Required; for example: 15551234567,442071234567"},
			{Key: "next", Kind: "action", Value: "Continue", Description: "Save settings and open pairing"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to account mode"},
			{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return without saving"},
		}
		state = []core.ScreenControl{
			{Key: whatsappModeKey, Kind: "hidden", Value: mode},
		}
	case "pair":
		screen.StartAtTop = true
		screen.Status = "Starting WhatsApp and waiting for its pairing QR code…"
		screen.Subtitle = "On your primary phone, open **Linked devices → Link a device** (under ⋮ on Android or Settings on iPhone). Open the QR by itself so the terminal can use every available row, or link with a phone-number code instead. Pairing continues in the background when you leave this screen.\n\n[Open WhatsApp's official linking help](https://faq.whatsapp.com/1317564962315842)."
		screen.Controls = []core.ScreenControl{
			{Key: "show_qr", Kind: "action", Value: "Show QR", Description: "Use the full terminal for the QR; press any key to return"},
			{Key: "phone", Kind: "action", Value: "Use pairing code", Description: "Link with the account phone number when QR scanning is unavailable"},
			{Key: "retry", Kind: "action", Value: "Retry pairing", Description: "Refresh immediately instead of waiting for the automatic retry"},
			{Key: "back", Kind: "action", Value: "Back", Description: "Return to allowed numbers"},
			{Key: "done", Kind: "action", Value: "Done", Description: "Return to WhatsApp settings; pairing continues in the background"},
		}
		state = nil
		if pairing, ok := s.Pairing("whatsapp"); ok {
			screen.Status = pairing.Detail
		}
	}
	screen.Controls = append(screen.Controls, state...)
	return screen
}

func (s *Service) whatsappPhonePairingScreen(values map[string]string, code string) core.Screen {
	screen := core.Screen{
		ID: "wizard:whatsapp:phone", ParentID: "wizard:whatsapp:pair", Title: "WhatsApp setup",
		Markdown: true, SaveDisabled: true, StartAtTop: true, Tabs: []string{"Mode", "Access", "Pair"}, ActiveTab: 2,
		Hints:    wizardScreenHints(),
		Subtitle: "Enter the international phone number of the WhatsApp account being linked. Then, on that phone, open **Linked devices → Link a device → Link with phone number instead** and enter the generated code.\n\n[Open WhatsApp's official phone-linking help](https://faq.whatsapp.com/1324084875126592).",
		Controls: []core.ScreenControl{
			{Key: whatsappPhoneKey, Label: "account phone number", Kind: "text", Value: wizardValue(values, whatsappPhoneKey, ""), Description: "Required; international format, for example 15551234567"},
			{Key: "generate", Kind: "action", Value: "Generate pairing code", Description: "Request a short-lived code from WhatsApp"},
			{Key: "cancel", Kind: "action", Value: "Back to pairing", Description: "Return to QR and retry options"},
		},
	}
	if code != "" {
		screen.Banner = "Pairing code: " + code
		screen.Status = "Enter this code on your phone before the pairing session expires."
	}
	return screen
}

func pairingNeedsRetry(state string) bool {
	return state == "timeout" || state == "error" || strings.HasPrefix(state, "err-")
}

func wizardStepIndex(step string, steps []string) int {
	for index, candidate := range steps {
		if candidate == step {
			return index
		}
	}
	return 0
}

func wizardActions(nextLabel string, back bool) []core.ScreenControl {
	controls := []core.ScreenControl{{Key: "next", Kind: "action", Value: nextLabel, Description: "Continue to the next setup step"}}
	if back {
		controls = append(controls, core.ScreenControl{Key: "back", Kind: "action", Value: "Back", Description: "Return to the previous step"})
	}
	return append(controls, core.ScreenControl{Key: "cancel", Kind: "action", Value: "Cancel setup", Description: "Return without saving"})
}

func formScreenHints() []core.ScreenHint {
	return []core.ScreenHint{
		{Key: "↑↓/⇥", Action: "nav"},
		{Key: "␠/↵", Action: "choose"},
		{Key: "⌃S", Action: "save"},
		{Key: "␛", Action: "exit"},
	}
}

func wizardScreenHints() []core.ScreenHint {
	return []core.ScreenHint{
		{Key: "↑↓/⇥", Action: "nav"},
		{Key: "␠/↵", Action: "choose"},
		{Key: "␛", Action: "cancel"},
	}
}

func selectionScreenHints() []core.ScreenHint {
	return []core.ScreenHint{
		{Key: "↑↓/⇥", Action: "nav"},
		{Key: "␠/↵", Action: "select"},
		{Key: "␛", Action: "cancel"},
	}
}

func telegramWizardState(token, allowed, enabled string) []core.ScreenControl {
	return []core.ScreenControl{
		{Key: telegramTokenKey, Kind: "hidden", Value: token, Secret: true},
		{Key: telegramAllowedKey, Kind: "hidden", Value: allowed},
		{Key: telegramEnabledKey, Kind: "hidden", Value: enabled},
	}
}

func wizardValue(values map[string]string, key, fallback string) string {
	if value, ok := values[key]; ok {
		return value
	}
	return fallback
}

func hasCommaSeparatedValue(value string) bool {
	for _, item := range strings.Split(value, ",") {
		if strings.TrimSpace(item) != "" {
			return true
		}
	}
	return false
}

func hasPhoneNumberValue(value string) bool {
	return config.HasAllowedWhatsAppNumber(strings.Split(value, ","))
}

func validWhatsAppPairingPhone(value string) bool {
	var digits strings.Builder
	for _, character := range value {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
		}
	}
	normalized := digits.String()
	return len(normalized) > 6 && normalized[0] != '0'
}

func (s *Service) SetNotice(notice channel.Notice) {
	s.Runtime.Log(notice.Channel + " message from " + notice.Sender + ": " + notice.Text)
	s.noticeMu.Lock()
	s.noticeSequence++
	s.lastNotice = notice
	s.noticeMu.Unlock()
	select {
	case s.noticeEvents <- notice:
	default:
	}
}

func (s *Service) NoticeEvents() <-chan channel.Notice { return s.noticeEvents }

// SharedState is the bounded process state needed by every attached TUI. It
// intentionally excludes configuration secrets, log bodies, and conversation
// contents.
type SharedState struct {
	Title          string                     `json:"title"`
	Theme          string                     `json:"theme"`
	Connections    []channel.ConnectionStatus `json:"connections"`
	Pairings       []channel.PairingEvent     `json:"pairings"`
	Runtime        core.RuntimeStatus         `json:"runtime"`
	NoticeSequence uint64                     `json:"notice_sequence"`
	Notice         channel.Notice             `json:"notice"`
}

func (s *Service) SharedState() SharedState {
	state := SharedState{
		Title: s.currentTitle(), Theme: s.Settings.Snapshot().Channels.TUI.Theme,
		Runtime: s.Runtime.Status(),
	}
	s.connectionMu.RLock()
	for _, name := range []string{"telegram", "whatsapp"} {
		if status, ok := s.connections[name]; ok {
			state.Connections = append(state.Connections, status)
		}
	}
	s.connectionMu.RUnlock()
	s.pairingMu.RLock()
	for _, name := range []string{"telegram", "whatsapp"} {
		if pairing, ok := s.pairing[name]; ok {
			state.Pairings = append(state.Pairings, pairing)
		}
	}
	s.pairingMu.RUnlock()
	s.noticeMu.RLock()
	state.NoticeSequence = s.noticeSequence
	state.Notice = s.lastNotice
	s.noticeMu.RUnlock()
	return state
}

func (s *Service) harnessCommand(message core.Message, remainder string, emit core.Emit) error {
	if strings.TrimSpace(remainder) != "" {
		name := harness.NormalizeName(remainder)
		if _, ok := harness.Lookup(name); !ok {
			return s.localReply(message, "Unknown coding harness `"+name+"`. Choose `codex` or `claude-code`.", emit)
		}
		if _, err := s.ApplySettings(map[string]string{"harness.name": name}); err != nil {
			return s.localReply(message, "Cannot select coding harness: "+err.Error(), emit)
		}
		if _, err := harness.ResolveCommand(name, nil); err != nil {
			definition, _ := harness.Lookup(name)
			return s.localReply(message, fmt.Sprintf("Saved `harness.name` = `%s`. **%s** is not installed or not on PATH. Install it from %s, then run `/harness %s` again to connect.", name, definition.DisplayName, definition.InstallURL, name), emit)
		}
		return s.localReply(message, "Saved `harness.name` = `"+name+"` and connected the coding harness.", emit)
	}
	if message.Channel == "tui" && emit != nil {
		screen := s.HarnessScreen(false)
		emit(core.Event{Kind: core.EventScreen, Screen: &screen, Local: true})
		return nil
	}
	current := emptyAs(s.Settings.Snapshot().Harness.Name, "not selected")
	lines := []string{"# Coding harnesses", "", "Active: `" + current + "`", ""}
	for _, definition := range harness.Catalog() {
		status := "not installed"
		if _, err := harness.ResolveCommand(definition.Name, nil); err == nil {
			status = "detected"
		}
		lines = append(lines, fmt.Sprintf("- `%s` — %s (%s)", definition.Name, definition.DisplayName, status))
	}
	lines = append(lines, "", "Select one with `/harness <name>`.")
	return s.localReply(message, strings.Join(lines, "\n"), emit)
}

// HarnessScreen is shared by onboarding, /config, and /harness so detection
// state and setup guidance never drift between entry points.
func (s *Service) HarnessScreen(required bool) core.Screen {
	current := s.Settings.Snapshot().Harness.Name
	detectedAny := false
	screen := core.Screen{
		ID: "harness", Title: "Coding harness", SaveDisabled: true, Required: required,
		Hints:    selectionScreenHints(),
		Subtitle: "Choose the coding CLI Spynel should use. Executables and working directories are detected automatically.",
	}
	if required {
		screen.Hints[len(screen.Hints)-1].Action = "exit"
	}
	for _, definition := range harness.Catalog() {
		description := definition.Description
		if _, err := harness.ResolveCommand(definition.Name, nil); err == nil {
			description += " · detected"
			detectedAny = true
			if screen.InitialControl == "" {
				screen.InitialControl = "select:" + definition.Name
			}
		} else {
			description += " · not detected · install: " + definition.InstallURL
		}
		if definition.Name == current {
			description += " · selected"
			screen.InitialControl = "select:" + definition.Name
		}
		screen.Controls = append(screen.Controls, core.ScreenControl{
			Key: "select:" + definition.Name, Kind: "action", Value: definition.DisplayName, Description: description,
		})
	}
	if current == "" {
		if detectedAny {
			screen.Status = "A coding harness is available. Select it to continue."
		} else {
			screen.Status = "No coding harness was detected. Choose one to see setup guidance."
		}
	} else if availability, ok := s.Harness.(harness.Availability); ok {
		if ready, detail := availability.Available(); !ready {
			screen.Status = "Selected but unavailable"
			if strings.TrimSpace(detail) != "" {
				screen.Status += ": " + detail
			}
		}
	}
	return screen
}

func (s *Service) modelCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	if strings.TrimSpace(remainder) != "" {
		return s.setSetting(message, "harness.model", remainder, emit)
	}
	provider, ok := s.Harness.(harness.ModelProvider)
	if !ok {
		return s.localReply(message, "The active harness does not provide a model catalog. Set an exact model with `/model <name>`.", emit)
	}
	models, err := provider.Models(ctx)
	if err != nil {
		return s.localReply(message, "Cannot load models: "+err.Error(), emit)
	}
	if len(models) == 0 {
		return s.localReply(message, "The active harness returned no available models. Set an exact model with `/model <name>`.", emit)
	}
	if message.Channel == "tui" && emit != nil {
		screen, err := s.modelScreen(ctx)
		if err != nil {
			return s.localReply(message, "Cannot load models: "+err.Error(), emit)
		}
		emit(core.Event{Kind: core.EventScreen, Screen: screen, Local: true})
		return nil
	}
	lines := []string{"# Available models", ""}
	for _, model := range models {
		marker := ""
		if model.Default {
			marker = " (default)"
		}
		lines = append(lines, fmt.Sprintf("- `%s` — %s%s", model.ID, model.DisplayName, marker))
	}
	lines = append(lines, "", "Select one with `/model <name>`.")
	return s.localReply(message, strings.Join(lines, "\n"), emit)
}

func (s *Service) modelScreen(ctx context.Context) (*core.Screen, error) {
	provider, ok := s.Harness.(harness.ModelProvider)
	if !ok {
		return nil, errors.New("the active harness does not provide a model catalog")
	}
	models, err := provider.Models(ctx)
	if err != nil {
		return nil, err
	}
	current := s.Settings.Snapshot().Harness.Model
	screen := &core.Screen{
		ID: "model", Title: "Harness model", SaveDisabled: true,
		Hints:          selectionScreenHints(),
		Subtitle:       "Choose a model supplied by " + s.Settings.Snapshot().Harness.Name + ". Enter applies the highlighted model immediately.",
		InitialControl: "select:" + current,
		Controls: []core.ScreenControl{{
			Key: "select:", Kind: "action", Value: "Harness default",
			Description: modelChoiceDescription("Use the harness's default model", current == "", false),
		}},
	}
	currentFound := current == ""
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		label := model.ID
		description := model.DisplayName
		if description == "" {
			description = model.ID
		}
		if model.Description != "" {
			description += " — " + model.Description
		}
		isCurrent := model.ID == current
		currentFound = currentFound || isCurrent
		screen.Controls = append(screen.Controls, core.ScreenControl{
			Key: "select:" + model.ID, Kind: "action", Value: label,
			Description: modelChoiceDescription(description, isCurrent, model.Default),
		})
	}
	if !currentFound {
		screen.Controls = append([]core.ScreenControl{{
			Key: "select:" + current, Kind: "action", Value: current,
			Description: "Current custom model (not in the harness catalog)",
		}}, screen.Controls...)
	}
	return screen, nil
}

func modelChoiceDescription(description string, current, defaultModel bool) string {
	labels := make([]string, 0, 2)
	if current {
		labels = append(labels, "current")
	}
	if defaultModel {
		labels = append(labels, "harness default")
	}
	if len(labels) == 0 {
		return description
	}
	return description + " (" + strings.Join(labels, ", ") + ")"
}

func (s *Service) selectionScreenAction(ctx context.Context, screenID, action string) (*core.Screen, bool, error) {
	if screenID != "harness" && screenID != "model" {
		return nil, false, nil
	}
	if !strings.HasPrefix(action, "select:") {
		return nil, true, fmt.Errorf("invalid %s selection action %q", screenID, action)
	}
	selected := strings.TrimPrefix(action, "select:")
	key := "harness.model"
	if screenID == "harness" {
		key = "harness.name"
		setting, _ := config.SettingByKey(s.Settings.Snapshot(), key)
		valid := false
		for _, choice := range setting.Choices {
			valid = valid || choice == selected
		}
		if !valid {
			return nil, true, fmt.Errorf("unknown harness %q", selected)
		}
	} else if selected != "" && selected != s.Settings.Snapshot().Harness.Model {
		provider, ok := s.Harness.(harness.ModelProvider)
		if !ok {
			return nil, true, errors.New("the active harness does not provide a model catalog")
		}
		models, err := provider.Models(ctx)
		if err != nil {
			return nil, true, fmt.Errorf("load models: %w", err)
		}
		valid := false
		for _, model := range models {
			valid = valid || model.ID == selected
		}
		if !valid {
			return nil, true, fmt.Errorf("model %q is no longer in the harness catalog", selected)
		}
	}
	_, err := s.ApplySettings(map[string]string{key: selected})
	if err != nil {
		return nil, true, err
	}
	if screenID == "harness" {
		if _, resolveErr := harness.ResolveCommand(selected, nil); resolveErr != nil {
			screen := s.HarnessScreen(false)
			screen.Status = resolveErr.Error() + ". Install the CLI, then select it again to retry."
			return &screen, true, nil
		}
		welcome, welcomeErr := s.InitialWelcome()
		if welcomeErr != nil {
			return nil, true, welcomeErr
		}
		if welcome != nil {
			return welcome, true, nil
		}
	}
	return nil, true, nil
}

func (s *Service) setSetting(message core.Message, key, value string, emit core.Emit) error {
	if (message.Channel == "telegram" || message.Channel == "whatsapp") && strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "channels."+message.Channel+".") {
		name := "Telegram"
		if message.Channel == "whatsapp" {
			name = "WhatsApp"
		}
		return s.localReply(message, name+" cannot be configured from "+name+" itself. Use the TUI or another channel so a bad setting cannot lock you out.", emit)
	}
	changed, err := s.ApplySettings(map[string]string{key: value})
	if err != nil {
		return s.localReply(message, "Cannot save configuration: "+err.Error(), emit)
	}
	setting := changed[0]
	response := fmt.Sprintf("Saved `%s` = `%s`.", setting.Key, setting.Value)
	if setting.Restart {
		response += " Restart Spynel to apply this change to the running harness or service."
	}
	return s.localReply(message, response, emit)
}

// ApplySettings is the single validated persistence path shared by commands
// and TUI forms.
func (s *Service) ApplySettings(values map[string]string) ([]config.Setting, error) {
	s.configurationMu.Lock()
	defer s.configurationMu.Unlock()
	previous := s.Settings.Snapshot()
	next := previous
	changed, err := config.SetSettings(&next, values)
	if err != nil {
		return nil, err
	}
	var selectedTheme theme.Theme
	themeChanged := previous.Channels.TUI.Theme != next.Channels.TUI.Theme
	if themeChanged {
		themes, loadErr := theme.LoadDir(next.StatePath("themes"))
		if loadErr != nil {
			return nil, loadErr
		}
		var found bool
		selectedTheme, found = theme.Find(themes, next.Channels.TUI.Theme)
		if !found {
			return nil, fmt.Errorf("unknown theme %q; use /theme to list available themes", next.Channels.TUI.Theme)
		}
		next.Channels.TUI.Theme = selectedTheme.Name
		for index := range changed {
			if changed[index].Key == "channels.tui.theme" {
				changed[index], _ = config.SettingByKey(next, changed[index].Key)
			}
		}
	}
	unchanged := reflect.DeepEqual(previous, next)
	harnessChanged := previous.Harness != next.Harness
	startupChanged := previous.Startup.Enabled != next.Startup.Enabled
	if harnessChanged {
		if err := s.reconfigureHarness(next); err != nil {
			return nil, err
		}
	}
	if unchanged {
		if themeChanged {
			s.publishTheme(selectedTheme)
		}
		return changed, nil
	}
	if _, err := s.Settings.Update(func(current *config.Config) error {
		*current = next
		return nil
	}); err != nil {
		var rollback error
		if harnessChanged {
			rollback = s.reconfigureHarness(previous)
		}
		return nil, errors.Join(err, wrapRollback("harness", rollback))
	}
	if startupChanged && s.Startup != nil {
		if err := s.Startup.Sync(next, next.Startup.Enabled); err != nil {
			var rollbacks []error
			_, rollbackConfig := s.Settings.Update(func(current *config.Config) error {
				*current = previous
				return nil
			})
			rollbacks = append(rollbacks, wrapRollback("configuration", rollbackConfig))
			if harnessChanged {
				rollbacks = append(rollbacks, wrapRollback("harness", s.reconfigureHarness(previous)))
			}
			rollbacks = append(rollbacks, wrapRollback("startup registration", s.Startup.Sync(previous, previous.Startup.Enabled)))
			return nil, errors.Join(fmt.Errorf("configure run at startup: %w", err), errors.Join(rollbacks...))
		}
	}
	for _, setting := range changed {
		if setting.Key == "channels.tui.title" {
			s.publishTitle(values[setting.Key])
		}
	}
	if themeChanged {
		s.publishTheme(selectedTheme)
	}
	return changed, nil
}

func (s *Service) themeCommand(message core.Message, name string, emit core.Emit) error {
	values, err := theme.LoadDir(s.Settings.Snapshot().StatePath("themes"))
	if err != nil {
		return s.localReply(message, "Cannot load themes: "+err.Error(), emit)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		if message.Channel == "tui" && emit != nil {
			emit(core.Event{Kind: core.EventThemePicker, Local: true})
			return nil
		}
		current := s.Settings.Snapshot().Channels.TUI.Theme
		lines := []string{"# Themes", "", "Active: `" + current + "`", ""}
		for _, value := range values {
			lines = append(lines, "- `"+value.Name+"` — "+value.Description)
		}
		lines = append(lines, "", "Use `/theme <name>` to apply one to the TUI.")
		return s.localReply(message, strings.Join(lines, "\n"), emit)
	}
	selected, ok := theme.Find(values, name)
	if !ok {
		return s.localReply(message, "Unknown theme `"+name+"`. Use `/theme` to list available themes.", emit)
	}
	unchanged := strings.EqualFold(s.Settings.Snapshot().Channels.TUI.Theme, selected.Name)
	if _, err := s.ApplySettings(map[string]string{"channels.tui.theme": selected.Name}); err != nil {
		return s.localReply(message, "Cannot apply theme: "+err.Error(), emit)
	}
	// Re-publish even when the configured name is unchanged so editing the
	// corresponding YAML file can be applied without restarting Spynel.
	if unchanged {
		s.publishTheme(selected)
	}
	return s.localReply(message, "Theme changed to `"+selected.Name+"` — "+selected.Description, emit)
}

// ThemeChanges publishes persisted theme selections from every channel.
func (s *Service) ThemeChanges() <-chan theme.Theme { return s.themeChanges }

func (s *Service) publishTheme(value theme.Theme) {
	select {
	case <-s.themeChanges:
	default:
	}
	s.themeChanges <- value
}

func wrapRollback(component string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("roll back %s: %w", component, err)
}

func (s *Service) reconfigureHarness(cfg config.Config) error {
	runtimeHarness, ok := s.Harness.(interface {
		HarnessConfig() harness.HarnessConfig
		Reconfigure(harness.HarnessConfig) error
	})
	if !ok {
		return nil
	}
	runtimeConfig := runtimeHarness.HarnessConfig()
	runtimeConfig.Name = cfg.Harness.Name
	runtimeConfig.Cwd = cfg.Root
	runtimeConfig.Model = cfg.Harness.Model
	runtimeConfig.Effort = "medium"
	runtimeConfig.ApprovalPolicy = "never"
	runtimeConfig.Sandbox = cfg.Harness.Sandbox
	runtimeConfig.Network = false
	runtimeConfig.SessionsFile = cfg.HarnessSessionsPath(cfg.Harness.Name)
	command, err := harness.ResolveCommand(cfg.Harness.Name, nil)
	if err != nil {
		if definition, ok := harness.Lookup(cfg.Harness.Name); ok {
			runtimeConfig.Command = definition.Command
		}
		if unavailable, ok := s.Harness.(interface {
			ConfigureUnavailable(harness.HarnessConfig, error) error
		}); ok {
			return unavailable.ConfigureUnavailable(runtimeConfig, err)
		}
		return err
	}
	runtimeConfig.Command = command
	return runtimeHarness.Reconfigure(runtimeConfig)
}

func formatSettings(cfg config.Config, section string) string {
	title := "Spynel configuration"
	if section == "telegram" {
		title = "Telegram configuration"
	} else if section == "whatsapp" {
		title = "WhatsApp configuration"
	}
	lines := []string{"# " + title, ""}
	grouped := section == "config" || section == "telegram" || section == "whatsapp"
	if grouped {
		lines = append(lines, "## Essential", "")
	}
	if section == "config" {
		lines = append(lines,
			"- `harness.name` = `"+emptyAs(cfg.Harness.Name, "not selected")+"` — Active coding harness; use `/harness [name]`",
			"- `harness.model` = `"+emptyAs(cfg.Harness.Model, "harness default")+"` — Active model; use `/model [name]`",
		)
	}
	advanced := false
	for _, setting := range config.Settings(cfg) {
		if setting.Section != section {
			continue
		}
		if grouped && setting.Advanced && !advanced {
			lines = append(lines, "", "## Advanced", "")
			advanced = true
		}
		lines = append(lines, fmt.Sprintf("- `%s` = `%s` — %s", setting.Key, setting.Value, setting.Description))
	}
	lines = append(lines, "", configurationUsage(section))
	return strings.Join(lines, "\n")
}

func configurationUsage(section string) string {
	if section == "telegram" || section == "whatsapp" {
		return fmt.Sprintf("Use `/%s on|off`, `/%s get <key>`, or `/%s set <key> <value>`.", section, section, section)
	}
	return "Use `/config get <key>` or `/config set <key> <value>`."
}

func scopedSettingKey(section, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if section == "config" || strings.Contains(key, ".") {
		return key
	}
	return "channels." + section + "." + key
}

func settingsScreen(cfg config.Config, section string) core.Screen {
	screen := core.Screen{ID: section, Hints: formScreenHints()}
	if section == "config" {
		harnessName := emptyAs(cfg.Harness.Name, "Choose a harness")
		modelName := emptyAs(cfg.Harness.Model, "Harness default")
		screen.StartAtTop = true
		screen.Controls = append(screen.Controls,
			core.ScreenControl{Key: "harness", Section: "Core settings", Kind: "action", Value: "Coding harness · " + harnessName, Description: "Select Codex or Claude Code; Spynel finds the executable automatically"},
			core.ScreenControl{Key: "model", Kind: "action", Value: "Model · " + modelName, Description: "Choose from the active harness model catalog"},
		)
	} else if section == "telegram" || section == "whatsapp" {
		screen.StartAtTop = true
	}
	advanced := false
	basic := section == "telegram" || section == "whatsapp"
	settings := config.Settings(cfg)
	order := map[string]int{}
	enabledKey := ""
	switch section {
	case "telegram":
		enabledKey = telegramEnabledKey
		order = map[string]int{
			telegramEnabledKey: 0,
			telegramTokenKey:   1,
			telegramAllowedKey: 2,
		}
	case "whatsapp":
		enabledKey = whatsappEnabledKey
		order = map[string]int{
			whatsappEnabledKey: 0,
			whatsappModeKey:    1,
			whatsappAllowedKey: 2,
		}
	}
	if len(order) > 0 {
		sort.SliceStable(settings, func(left, right int) bool {
			leftOrder, leftBasic := order[settings[left].Key]
			rightOrder, rightBasic := order[settings[right].Key]
			if !leftBasic {
				leftOrder = len(order)
			}
			if !rightBasic {
				rightOrder = len(order)
			}
			return leftOrder < rightOrder
		})
	}
	for _, setting := range settings {
		if setting.Section != section {
			continue
		}
		if setting.Advanced && !advanced {
			description := "Show optional connection, group, notification, and storage controls"
			if section == "config" {
				description = "Show task-management, channel, speech, extension, and storage controls"
			}
			screen.Controls = append(screen.Controls, core.ScreenControl{
				Key: "advanced", Section: "Advanced settings", Kind: "disclosure", Value: "Advanced settings",
				Description: description,
			})
			advanced = true
		}
		kind := "text"
		if setting.Secret {
			kind = "password"
		} else if len(setting.Choices) == 2 && setting.Choices[0] == "on" && setting.Choices[1] == "off" {
			kind = "toggle"
		} else if len(setting.Choices) > 0 {
			kind = "select"
		}
		label := strings.ReplaceAll(strings.TrimPrefix(setting.Key, "channels."+section+"."), "_", " ")
		switch setting.Key {
		case telegramTokenKey:
			label = "bot token"
		case "workspace.history_max_messages":
			label = "context messages"
		case "harness.sandbox":
			label = "agent filesystem access"
		case "workspace.history_char_limit":
			label = "context character limit"
		case "workspace.attachment_max_mb":
			label = "attachment size limit (MB)"
		case "startup.enabled":
			label = "run at startup"
		}
		value := setting.Value
		configured := false
		if setting.Secret {
			configured = setting.Value == "set"
			value = ""
		}
		control := core.ScreenControl{
			Key: setting.Key, Label: label, Description: setting.Description, Kind: kind,
			Value: value, Options: append([]string(nil), setting.Choices...), Secret: setting.Secret, Configured: configured,
			DescriptionMarkdown: setting.DescriptionMarkdown, Advanced: setting.Advanced,
		}
		if setting.Key == enabledKey {
			screen.Controls = append(screen.Controls, control, core.ScreenControl{
				Key: "wizard", Section: "Setup", Kind: "action", Value: "Setup wizard",
				Description: "Configure the essential connection step by step",
			})
			continue
		}
		if basic && !setting.Advanced {
			control.Section = "Basic settings"
			basic = false
		}
		screen.Controls = append(screen.Controls, control)
	}
	return screen
}

func redactSensitiveCommand(text string) string {
	trimmed := strings.TrimSpace(text)
	parts := strings.Fields(trimmed)
	if len(parts) < 4 || !strings.EqualFold(parts[1], "set") {
		return text
	}
	command := strings.ToLower(strings.TrimPrefix(strings.SplitN(parts[0], "@", 2)[0], "/"))
	key := scopedSettingKey(command, parts[2])
	if (command == "config" || command == "telegram" || command == "whatsapp") && config.IsSecretSetting(key) {
		return strings.Join(parts[:3], " ") + " <redacted>"
	}
	return text
}
