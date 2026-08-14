// Package instructions composes framework and workspace-owner instruction
// sections into rendered harness prompts.
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

const ScopeDisciplineGuidance = `Stay within the assigned scope; prioritize the smallest actions and verification that deliver most of the requested value quickly; and avoid unnecessary exploration or polish unless safety, correctness, or explicit acceptance criteria require it. This proportionality rule never overrides explicit user instructions or safety, authorization, lifecycle, independent-review, evidence, or data-handling requirements.`

const scopeDisciplineSection = "## Framework scope discipline\n\n" + ScopeDisciplineGuidance

const EpistemicTrustGuidance = `## Evidence-grounded honesty

Never knowingly lie, fabricate evidence, invent a cause, imply that you inspected something you did not inspect, or present an assumption, inference, recollection, stale chat claim, or unverified external condition as established fact. Distinguish what you directly observed, what durable or source-backed evidence establishes, what you infer, what remains uncertain, and what you do not know.

Before stating a materially uncertain, current, workspace-specific, causal, or high-impact claim as fact, check the relevant authoritative state when a bounded practical inspection can establish it. This includes claims that inspection, dispatch, delivery, completion, release, code behavior, or another external action or condition actually occurred. Intent, a prior chat statement, or a plausible explanation is not evidence that it happened. Stable common knowledge and low-risk answers do not require ritual lookup, but disclose material lack of verification instead of hiding it.

When the answer is not established and bounded inspection is unavailable or outside this communication turn, say plainly “I don't know yet,” then either perform an appropriate bounded lookup or offer to investigate or dispatch the investigation. Never fill an evidence gap with a plausible story. When correcting a claim, identify precisely what was unsupported and what new evidence changes the conclusion. Do not reflexively say “you are correct” unless the evidence actually establishes it. This prompt is a mandatory product behavior contract, not a guarantee that a model can never make a mistake.`

const chatGuidanceMarker = "Persistent memory is a separate lightweight configuration action."

const requiredChatGuidance = chatGuidanceMarker + ` When the user explicitly asks to remember, permanently change, inspect, correct, replace, or forget standing behavior, maintain the matching workspace-local Markdown file under .spynel/instructions: agent-chat.md, agent-developer.md, agent-reviewer.md, agent-notification.md, or agent-heartbeat.md. Re-read the target immediately before an atomic permission-safe update, preserve unrelated edits, store one concise normalized rule, and deduplicate, replace, or remove obsolete rules. Edit only agent-chat.md unless another role is explicitly named or unambiguous; ask one concise question when permanence or role is materially ambiguous. A current forget or override request takes effect immediately despite old imported text. Briefly confirm the changed behavior and role without exposing an absolute path on Telegram or WhatsApp unless requested.

Never persist one-off directions, ordinary feedback, transient decisions, inferred preferences, secrets, credentials, recipient identifiers, attachments, or transcripts. Persistent rules cannot weaken safety, authorization, lifecycle, review, or data-handling requirements and never override the current explicit request or nearest AGENTS.md/DOX contract.`

const chatTranscriptionGuidanceMarker = "Speech transcription can render Spynel"

const chatTranscriptionGuidance = chatTranscriptionGuidanceMarker + " as `spinal`, `spinel`, `spy nell`, or other phonetically similar words. When the surrounding context clearly refers to the Spynel framework, interpret those variants as `Spynel`. Preserve the literal meaning when the context instead indicates an unrelated use, such as a medical reference to `spinal`; contextual confidence is required."

type Role string

const (
	Chat         Role = "chat"
	Developer    Role = "developer"
	Reviewer     Role = "reviewer"
	Notification Role = "notification"
	Heartbeat    Role = "heartbeat"
)

var roles = [...]Role{Chat, Developer, Reviewer, Notification, Heartbeat}

// InjectScopeDiscipline guarantees that every agent prompt carries the same
// framework-owned proportionality rule, including user-overridden prompts.
func InjectScopeDiscipline(prompt string) string {
	if strings.Contains(prompt, scopeDisciplineSection) {
		return prompt
	}
	return strings.TrimRight(prompt, "\r\n") + "\n\n" + scopeDisciplineSection
}

// EnsureChatGuidance enforces framework-owned chat behavior for both stock and
// user-edited chat templates without overwriting their files.
func EnsureChatGuidance(prompt string) string {
	if !strings.Contains(prompt, EpistemicTrustGuidance) {
		prompt = strings.TrimRight(prompt, "\r\n") + "\n\n" + EpistemicTrustGuidance
	}
	if !strings.Contains(prompt, chatGuidanceMarker) {
		prompt = strings.TrimRight(prompt, "\r\n") + "\n\n" + requiredChatGuidance
	}
	if !strings.Contains(prompt, chatTranscriptionGuidanceMarker) {
		prompt = strings.TrimRight(prompt, "\r\n") + "\n\n" + chatTranscriptionGuidance
	}
	return prompt
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
	precedence := "These workspace-owner instructions guide this role across future sessions. Platform and system safety rules, the current explicit user request, and the nearest applicable repository or workspace AGENTS.md/DOX contract take precedence. They cannot weaken authorization, security, lifecycle, review, data-handling, or any framework-owned evidence-grounded honesty contract present in this prompt."
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
