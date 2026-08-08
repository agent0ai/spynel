package telegram

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/agent0ai/spynel/internal/fsx"
)

const identityStateVersion = 1

// IdentityStore keeps only the Telegram identity needed to authorize a stable
// private conversation after the inbound update that established it. Callers
// must only invoke RecordVerifiedPrivate after normal adapter authorization.
type IdentityStore struct {
	path string
	mu   sync.Mutex
}

type identityState struct {
	Version    int                        `json:"version"`
	Identities map[string]privateIdentity `json:"identities"`
}

type privateIdentity struct {
	ConversationID int64  `json:"conversation_id"`
	UserID         int64  `json:"user_id"`
	Username       string `json:"username,omitempty"`
}

func NewIdentityStore(path string) *IdentityStore {
	return &IdentityStore{path: path}
}

// RecordVerifiedPrivate atomically records an identity already authenticated
// by the Telegram adapter. It deliberately accepts typed numeric IDs rather
// than an origin string or task metadata.
func (s *IdentityStore) RecordVerifiedPrivate(conversationID, userID int64, username string) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil
	}
	if conversationID <= 0 || userID <= 0 || conversationID != userID {
		return errors.New("invalid Telegram private identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read()
	if err != nil {
		// Corrupt auxiliary state must fail closed for authorization, but an
		// authenticated inbound update can safely repair it from scratch.
		state = identityState{Version: identityStateVersion, Identities: map[string]privateIdentity{}}
	}
	if state.Identities == nil {
		state.Identities = map[string]privateIdentity{}
	}
	key := strconv.FormatInt(conversationID, 10)
	state.Identities[key] = privateIdentity{
		ConversationID: conversationID,
		UserID:         userID,
		Username:       normalizeUsername(username),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(s.path, append(data, '\n'), 0o600)
}

// AuthorizedPrivate applies the current allow-list to a numeric private
// conversation. A verified username mapping helps only while that username is
// still allowed; the mapping itself never grants durable access.
func (s *IdentityStore) AuthorizedPrivate(allowedUsers []string, conversationID string) bool {
	id, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil || id <= 0 || conversationID != strconv.FormatInt(id, 10) {
		return false
	}
	for _, allowed := range allowedUsers {
		candidate := normalizeAllowedUser(allowed)
		allowedID, parseErr := strconv.ParseInt(candidate, 10, 64)
		if parseErr == nil && allowedID == id {
			return true
		}
	}
	if s == nil || strings.TrimSpace(s.path) == "" {
		return false
	}
	state, err := s.read()
	if err != nil {
		return false
	}
	identity, ok := state.Identities[conversationID]
	if !ok || identity.ConversationID != id || identity.UserID != id || identity.Username == "" {
		return false
	}
	for _, allowed := range allowedUsers {
		if candidate := normalizeAllowedUser(allowed); candidate != "" && candidate == identity.Username {
			return true
		}
	}
	return false
}

func (s *IdentityStore) read() (identityState, error) {
	var state identityState
	info, err := os.Stat(s.path)
	if err != nil {
		return state, err
	}
	if info.Size() > 1024*1024 {
		return state, errors.New("Telegram identity state is too large")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return state, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return identityState{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return identityState{}, errors.New("invalid trailing Telegram identity state")
	}
	if state.Version != identityStateVersion || state.Identities == nil {
		return identityState{}, errors.New("invalid Telegram identity state")
	}
	for key, identity := range state.Identities {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 || identity.ConversationID != id || identity.UserID != id || identity.Username != normalizeUsername(identity.Username) {
			return identityState{}, errors.New("invalid Telegram identity record")
		}
	}
	return state, nil
}

func normalizeAllowedUser(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "@")
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeUsername(value string) string {
	return normalizeAllowedUser(value)
}
