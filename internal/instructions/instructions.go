// Package instructions loads the workspace owner's persistent per-agent
// instructions and appends them to rendered harness prompts.
package instructions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 64 << 10

const chatGuidanceMarker = "Persistent memory is a separate lightweight configuration action."

const legacyChatGuidance = chatGuidanceMarker + ` When the user explicitly asks to remember, permanently change, inspect, correct, replace, or forget standing behavior, maintain the matching workspace-local Markdown file under .spynel/instructions: agent-chat.md, agent-developer.md, agent-reviewer.md, agent-notification.md, or agent-heartbeat.md. Re-read the target immediately before an atomic permission-safe update, preserve unrelated edits, store one concise normalized rule, and deduplicate, replace, or remove obsolete rules. Edit only agent-chat.md unless another role is explicitly named or unambiguous; ask one concise question when permanence or role is materially ambiguous. A current forget or override request takes effect immediately despite old imported text. Briefly confirm the changed behavior and role without exposing an absolute path on Telegram or WhatsApp unless requested.

Never persist one-off directions, ordinary feedback, transient decisions, inferred preferences, secrets, credentials, recipient identifiers, attachments, or transcripts. Persistent rules cannot weaken safety, authorization, lifecycle, review, or data-handling requirements and never override the current explicit request or nearest AGENTS.md/DOX contract.`

type Role string

const (
	Chat         Role = "chat"
	Developer    Role = "developer"
	Reviewer     Role = "reviewer"
	Notification Role = "notification"
	Heartbeat    Role = "heartbeat"
)

var roles = [...]Role{Chat, Developer, Reviewer, Notification, Heartbeat}

// InjectChatGuidance upgrades legacy user-owned chat prompt templates in
// memory without overwriting their files.
func InjectChatGuidance(prompt string) string {
	if strings.Contains(prompt, chatGuidanceMarker) {
		return prompt
	}
	return strings.TrimRight(prompt, "\r\n") + "\n\n" + legacyChatGuidance
}

type Status struct {
	Role         Role
	RelativePath string
	Present      bool
	Bytes        int
	Valid        bool
	Error        string
}

func (role Role) Valid() bool {
	for _, candidate := range roles {
		if role == candidate {
			return true
		}
	}
	return false
}

func (role Role) RelativePath() string {
	return filepath.ToSlash(filepath.Join(".spynel", "instructions", "agent-"+string(role)+".md"))
}

func pathFor(stateRoot string, role Role) (string, error) {
	if !role.Valid() {
		return "", fmt.Errorf("unknown persistent-instruction role %q", role)
	}
	if filepath.Base(stateRoot) != ".spynel" {
		return "", errors.New("persistent instructions require the fixed .spynel state directory")
	}
	return filepath.Join(stateRoot, "instructions", "agent-"+string(role)+".md"), nil
}

func inspectRealDirectory(path, description string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must not be a symbolic link", description)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s must be a directory", description)
	}
	return info, nil
}

func inspectBoundary(stateRoot string) (os.FileInfo, os.FileInfo, error) {
	stateInfo, err := inspectRealDirectory(stateRoot, ".spynel state path")
	if err != nil {
		return nil, nil, err
	}
	instructionsInfo, err := inspectRealDirectory(filepath.Join(stateRoot, "instructions"), ".spynel/instructions path")
	if err != nil {
		return nil, nil, err
	}
	return stateInfo, instructionsInfo, nil
}

func revalidateBoundary(stateRoot string, stateInfo, instructionsInfo os.FileInfo) error {
	currentState, currentInstructions, err := inspectBoundary(stateRoot)
	if err != nil {
		return err
	}
	if !os.SameFile(stateInfo, currentState) || !os.SameFile(instructionsInfo, currentInstructions) {
		return errors.New("instruction directory changed during safety validation; retry the session")
	}
	return nil
}

// Load reads one fixed role file on every invocation. A missing file is an
// empty instruction set; unsafe or malformed files fail the affected session.
func Load(stateRoot string, role Role) (string, Status, error) {
	path, err := pathFor(stateRoot, role)
	status := Status{Role: role, RelativePath: role.RelativePath()}
	if err != nil {
		status.Error = err.Error()
		return "", status, err
	}
	stateInfo, instructionsInfo, err := inspectBoundary(stateRoot)
	if os.IsNotExist(err) {
		status.Valid = true
		return "", status, nil
	}
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("unsafe persistent instructions %s: %w", status.RelativePath, err)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		status.Valid = true
		return "", status, nil
	}
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("inspect %s: %w", status.RelativePath, err)
	}
	status.Present = true
	if info.Mode()&os.ModeSymlink != 0 {
		err = errors.New("symbolic links are not allowed")
	} else if !info.Mode().IsRegular() {
		err = errors.New("must be a regular Markdown file")
	} else if info.Mode().Perm()&0o022 != 0 {
		err = errors.New("must not be group- or world-writable")
	}
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("unsafe persistent instructions %s: %w", status.RelativePath, err)
	}
	file, err := os.Open(path)
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("open persistent instructions %s: %w", status.RelativePath, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("inspect opened persistent instructions %s: %w", status.RelativePath, err)
	}
	if !os.SameFile(info, openedInfo) {
		err = errors.New("file changed during safety validation; retry the session")
		status.Error = err.Error()
		return "", status, fmt.Errorf("unsafe persistent instructions %s: %w", status.RelativePath, err)
	}
	if err := revalidateBoundary(stateRoot, stateInfo, instructionsInfo); err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("unsafe persistent instructions %s: %w", status.RelativePath, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	status.Bytes = len(data)
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("read persistent instructions %s: %w", status.RelativePath, err)
	}
	afterRead, err := file.Stat()
	if err != nil {
		status.Error = err.Error()
		return "", status, fmt.Errorf("revalidate persistent instructions %s: %w", status.RelativePath, err)
	}
	if afterRead.Size() != openedInfo.Size() || !afterRead.ModTime().Equal(openedInfo.ModTime()) {
		err = errors.New("file changed while it was read; retry the session")
		status.Error = err.Error()
		return "", status, fmt.Errorf("unsafe persistent instructions %s: %w", status.RelativePath, err)
	}
	if len(data) > MaxBytes {
		err = fmt.Errorf("exceeds the %d-byte limit", MaxBytes)
		status.Error = err.Error()
		return "", status, fmt.Errorf("invalid persistent instructions %s: %w", status.RelativePath, err)
	}
	if !utf8.Valid(data) {
		err = errors.New("is not valid UTF-8")
		status.Error = err.Error()
		return "", status, fmt.Errorf("invalid persistent instructions %s: %w", status.RelativePath, err)
	}
	status.Valid = true
	return strings.TrimSpace(string(data)), status, nil
}

// Append adds a structurally separate, final trusted-configuration section.
func Append(prompt, stateRoot string, role Role) (string, error) {
	content, status, err := Load(stateRoot, role)
	if err != nil {
		return "", err
	}
	body := "No persistent instructions are currently configured for this role."
	if content != "" {
		body = content
	}
	heading := fmt.Sprintf("## Persistent instructions imported for the %s agent from %s", role, status.RelativePath)
	precedence := "These workspace-owner instructions guide this role across future sessions. Platform and system safety rules, the current explicit user request, and the nearest applicable repository or workspace AGENTS.md/DOX contract take precedence. They cannot weaken authorization, security, lifecycle, review, or data-handling rules."
	footer := fmt.Sprintf("End of persistent instructions for the %s agent. The precedence stated above still applies to every imported rule.", role)
	return strings.TrimRight(prompt, "\r\n") + "\n\n---\n\n" + heading + "\n\n" + precedence + "\n\n<workspace_owner_persistent_instructions>\n" + body + "\n</workspace_owner_persistent_instructions>\n\n" + footer, nil
}

func Inspect(stateRoot string) []Status {
	statuses := make([]Status, 0, len(roles))
	for _, role := range roles {
		_, status, _ := Load(stateRoot, role)
		statuses = append(statuses, status)
	}
	return statuses
}
