package harness

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/agent0ai/spynel/internal/core"
)

var workerConversationIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var workerConversationName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,118}[A-Za-z0-9-])?$`)

// ConversationRefused identifies a journaled source refusal, never a provider result.
func (e WorkerExecution) ConversationRefused() bool {
	return e.Conversation.Validate() == nil && e.Key == "chat:"+e.Conversation.Channel+":"+e.Conversation.Conversation &&
		e.Sequence > 0 && e.Workflow == nil && e.Agent == nil && e.Reservation == nil && len(e.Followups) == 0 &&
		!e.SourcesReserved && e.Done && e.Error != "" && e.ThreadID == "" && !e.Steered && !e.Cancelled &&
		e.LastCursor == 1 && e.CommittedCursor <= e.LastCursor
}

// Validate owns the private descriptor schema shared by admission and replay.
func (c *WorkerConversation) Validate() error {
	if c == nil || !slices.Contains([]string{"cli", "tui", "telegram", "whatsapp"}, c.Channel) ||
		!workerConversationName.MatchString(c.Conversation) ||
		!workerConversationIdentity.MatchString(c.ExecutionID) || !workerConversationIdentity.MatchString(c.SourceMessageID) ||
		len(c.Sources) == 0 || len(c.Sources) > 1024 || !slices.IsSorted(c.Sources) || !slices.Contains(c.Sources, c.SourceMessageID) {
		return errors.New("invalid worker conversation descriptor")
	}
	for i, source := range c.Sources {
		if !workerConversationIdentity.MatchString(source) || i > 0 && source == c.Sources[i-1] {
			return errors.New("invalid worker conversation sources")
		}
	}
	return nil
}

func (h *workerHost) validateConversationLocked(request WorkerRequest) error {
	conversation := request.Conversation
	if strings.HasPrefix(request.Key, "chat:") && conversation == nil {
		return errors.New("worker conversation descriptor is required")
	}
	if conversation != nil {
		if err := conversation.Validate(); err != nil {
			return err
		}
		if request.Workflow != nil || request.Agent != nil || request.Key != "chat:"+conversation.Channel+":"+conversation.Conversation {
			return errors.New("invalid worker conversation admission")
		}
	}
	// New sequences cannot bypass the owning same-execution follow-up operation.
	for _, retained := range h.executions {
		if retained.Key == request.Key && (retained.Conversation != nil || conversation != nil) {
			return errors.New("conversation admission already retained; consume and retire before another dispatch")
		}
	}
	return nil
}

// Published event descriptors are immutable snapshots. The retained execution
// accumulates admitted batches, independently of its attached controller.
func (h *workerHost) captureConversationLocked(execution *workerExecution, event core.Event, requestOnly bool) (*WorkerConversation, string) {
	if execution.Conversation == nil {
		return nil, ""
	}

	snapshot := *execution.Conversation
	streamed := ""
	if event.Kind == core.EventDelta {
		execution.streamed += event.Text
	}
	if event.Done && !requestOnly && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
		// Application hooks or outbound validation can turn a final into an error.
		streamed = execution.streamed
		execution.streamed = ""
	}
	return &snapshot, streamed
}
