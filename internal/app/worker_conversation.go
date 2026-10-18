package app

import (
	"errors"
	"path/filepath"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

// AdmitWorkerConversation runs inside the worker's existing owner fence.
// Transport authorization and job admission precede this source reservation.
func AdmitWorkerConversation(root, key string, conversation harness.WorkerConversation, receipt string) error {
	if conversation.Validate() != nil || key != "chat:"+conversation.Channel+":"+conversation.Conversation {
		return errors.New("invalid worker conversation admission")
	}
	store := history.New(filepath.Join(root, ".spynel", "history"))
	return store.AdmitConversation(conversation.Channel, conversation.Conversation, conversation.ExecutionID, receipt, conversation.Sources, conversation.Recovery)
}

// ConsumeConversationDelivery runs under WorkerClient.Consume's exact owner
// fence. Its downstream must persist/deduplicate ReceiptID and must not use RPC.
func (s *Service) ConsumeConversationDelivery(jobID int, delivery harness.WorkerDelivery, downstream func(harness.WorkerDelivery) error) (resultErr error) {
	conversation := delivery.Conversation
	if conversation.Validate() != nil ||
		!(jobWorkerReceipt{Worker: delivery.WorkerID, Sequence: delivery.Sequence, Cursor: delivery.Cursor}).valid() {
		return errors.New("invalid retained conversation delivery")
	}
	job, exists := s.Runtime.Job(jobID)
	if !exists || job.SessionKey != "chat:"+conversation.Channel+":"+conversation.Conversation ||
		job.Kind != "conversation" || job.WorkID != conversation.ExecutionID {
		return errors.New("retained conversation job does not match its execution")
	}
	defer func() {
		if resultErr != nil {
			s.Runtime.UpdateJob(jobID, core.ExecutionStatus{State: string(JobDegraded), Detail: "Conversation delivery remains pending"})
		}
	}()
	known, err := s.History.HasUserSourceIDs(conversation.Channel, conversation.Conversation, conversation.Sources)
	if err != nil {
		return err
	}
	if !known {
		return errors.New("retained conversation source is absent from history")
	}
	message := core.Message{Channel: conversation.Channel, Conversation: conversation.Conversation, SourceMessageID: conversation.SourceMessageID}
	if conversation.Recovery {
		message.Sender = "recovery"
	}
	consume := s.conversationEventConsumer(message, jobID, func(event core.Event) error {
		if downstream == nil {
			return nil
		}
		delivery.Event = event
		return downstream(delivery)
	}, &delivery)
	return consume(delivery.Event)
}
