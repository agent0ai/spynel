package app

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
)

var ErrDuplicateMessage = errors.New("duplicate message identity; inspect events or history for its outcome")
var ErrMessageAdmissionBusy = errors.New("message admission busy; retry the same identity later")
var conversationName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,119}$`)
var requestIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// ValidateLocalMessage owns the untrusted local operator boundary. Remote
// channel intake has its own transport authorization and identity rules.
func ValidateLocalMessage(message core.Message) error {
	if message.Channel != "cli" && message.Channel != "tui" {
		return errors.New("local messages require channel cli or tui")
	}
	if err := ValidateConversationName(message.Conversation); err != nil {
		return err
	}
	if !utf8.ValidString(message.Text) || len(message.Text) > 512*1024 || strings.TrimSpace(message.Text) == "" {
		return errors.New("message requires nonempty UTF-8 text up to 524288 bytes")
	}
	if message.SourceMessageID != "" && !requestIdentity.MatchString(message.SourceMessageID) {
		return errors.New("invalid source_message_id")
	}
	if len(message.Sender) > 128 || len(message.InstanceID) > 128 || len(message.ReplyTo) > 1024 {
		return errors.New("message metadata exceeds bounds")
	}
	return nil
}

func ValidateConversationName(name string) error {
	if !conversationName.MatchString(name) || strings.Trim(name, "._") != name {
		return errors.New("conversation must be 1-120 ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit and not ending in a dot or underscore")
	}
	return nil
}

// Fence the check-through-dispatch interval for each source identity. Durable
// history detects later/restarted retries; this bounded map closes concurrent
// retries before any extension or provider side effects in the owning service.
func (s *Service) admitMessage(message core.Message) (func(), error) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	key := sessionKey(message) + "\x00" + message.SourceMessageID
	if s.admissions[key] {
		return nil, ErrDuplicateMessage
	}
	if len(s.admissions) >= 64 {
		return nil, ErrMessageAdmissionBusy
	}
	if s.admissions == nil {
		s.admissions = make(map[string]bool)
	}
	s.admissions[key] = true
	return func() { s.admissionMu.Lock(); delete(s.admissions, key); s.admissionMu.Unlock() }, nil
}
