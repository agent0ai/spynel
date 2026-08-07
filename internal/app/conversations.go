package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

const (
	resumeListLimit        = 100
	resumeDisplayMessages  = 500
	resumeDisplayCharacter = 500000
)

func (s *Service) resumeCommand(message core.Message, emit core.Emit) error {
	if message.Channel != "tui" {
		return s.localReply(message, "Conversation browsing and branching are available from the TUI only. This channel keeps using its stable conversation automatically.", emit)
	}
	screen, err := s.resumeScreen()
	if err != nil {
		return err
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventScreen, Screen: &screen, Local: true})
	}
	return nil
}

func (s *Service) resumeScreen() (core.Screen, error) {
	conversations, err := s.History.List(resumeListLimit)
	if err != nil {
		return core.Screen{}, err
	}
	screen := core.Screen{
		ID: "resume", Title: "Resume a conversation",
		Hints: []core.ScreenHint{
			{Key: "↑↓/⇥", Action: "nav"},
			{Key: "␠/↵", Action: "choose"},
			{Key: "⌦", Action: "delete"},
			{Key: "␛", Action: "exit"},
		},
	}
	for _, conversation := range conversations {
		updated := conversation.UpdatedAt.Local().Format("2006-01-02 15:04")
		label := fmt.Sprintf("%-3s  %s  %s", resumePlatform(conversation.Channel), updated, filepath.Base(conversation.Path))
		description := conversation.Preview
		if description == "" {
			description = "Empty conversation"
		} else if conversation.LastRole != "" {
			description = conversation.LastRole + ": " + description
		}
		screen.Controls = append(screen.Controls, core.ScreenControl{
			Key:  "resume:" + encodeConversation(conversation.Channel, conversation.Conversation),
			Kind: "action", Value: label, Description: description,
		})
	}
	return screen, nil
}

func resumePlatform(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "tui":
		return "TUI"
	case "cli":
		return "CLI"
	case "telegram":
		return "TG"
	case "whatsapp":
		return "WA"
	default:
		value := strings.ToUpper(strings.TrimSpace(channel))
		if runes := []rune(value); len(runes) > 3 {
			value = string(runes[:3])
		}
		return value
	}
}

func encodeConversation(channel, conversation string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(channel + "\x00" + conversation))
}

func decodeConversation(value string) (string, string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", "", err
	}
	channel, conversation, ok := strings.Cut(string(data), "\x00")
	if !ok || channel == "" || conversation == "" {
		return "", "", errors.New("invalid conversation selection")
	}
	return channel, conversation, nil
}

func (s *Service) ScreenAction(ctx context.Context, screenID, action string, values map[string]string) (*core.Screen, error) {
	if screen, handled, err := s.configurationScreenAction(ctx, screenID, action, values); handled {
		return screen, err
	}
	if screen, handled, err := s.selectionScreenAction(ctx, screenID, action); handled {
		return screen, err
	}
	if screenID == "resume" && strings.HasPrefix(action, "resume:") {
		channelName, conversation, err := decodeConversation(strings.TrimPrefix(action, "resume:"))
		if err != nil {
			return nil, err
		}
		branch, path, err := s.History.Branch(channelName, conversation)
		if err != nil {
			return nil, err
		}
		entries, _, err := s.History.RecentEntries("tui", branch, resumeDisplayMessages, resumeDisplayCharacter)
		if err != nil {
			return nil, err
		}
		transcript := make([]core.ChatEntry, 0, len(entries)+1)
		transcript = append(transcript, core.ChatEntry{Role: "assistant", Text: fmt.Sprintf("Branched from `%s/%s`. The TUI loaded the newest %d entries; the complete copied transcript remains at [%s](<%s>).", channelName, conversation, resumeDisplayMessages, path, path)})
		for _, entry := range entries {
			role := entry.Role
			if role != "user" && role != "assistant" && role != "error" {
				role = "assistant"
			}
			transcript = append(transcript, core.ChatEntry{Role: role, Text: entry.Content})
		}
		screen := core.Screen{ID: "chat", Conversation: branch, Transcript: transcript, Subtitle: fmt.Sprintf("Branched from %s/%s at %s", channelName, conversation, time.Now().Format(time.RFC3339))}
		return &screen, nil
	}
	if screenID == "resume" && strings.HasPrefix(action, "delete:") {
		channelName, conversation, err := decodeConversation(strings.TrimPrefix(action, "delete:"))
		if err != nil {
			return nil, err
		}
		if err := s.Harness.ResetSession(sessionKey(core.Message{Channel: channelName, Conversation: conversation})); err != nil {
			return nil, fmt.Errorf("cannot discard the conversation's harness thread: %w", err)
		}
		if err := s.History.Clear(channelName, conversation); err != nil {
			return nil, err
		}
		screen, err := s.resumeScreen()
		return &screen, err
	}
	if action == "chat" {
		return nil, nil
	}
	screen, err := s.Screen(action)
	if err != nil {
		return nil, err
	}
	return &screen, nil
}
