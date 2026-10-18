package app

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
)

// Called under the Manager's native/exact-owner fence; it must not use RPC.
func (s *Service) attachWorkerConversation(execution harness.WorkerExecution) (int, error) {
	c := execution.Conversation
	if c.Validate() != nil || execution.Key != "chat:"+c.Channel+":"+c.Conversation || !execution.SourcesReserved || execution.Workflow != nil || execution.Agent != nil {
		return 0, errors.New("invalid retained conversation admission")
	}
	if job, ok := s.Runtime.JobForSession(execution.Key); ok && (job.Kind != "conversation" || job.WorkID != c.ExecutionID) {
		return 0, errors.New("retained conversation conflicts with its live job")
	}
	id, err := s.Runtime.TryBeginJobWithDetails(execution.Key, c.Channel, c.Conversation, "Retained conversation", JobDetails{
		Kind: "conversation", WorkID: c.ExecutionID, Provider: s.Settings.Snapshot().Harness.Name,
	})
	if err == nil && c.Channel == "tui" && !execution.Done && !execution.Cancelled {
		s.chatActivityMu.Lock()
		if len(s.chatActivity[id]) == 0 {
			activity := newChatActivityEmitter(func(event core.Event) {
				s.setConversationActivity(c.Channel, c.Conversation, event.Active)
			})
			s.trackChatActivityLocked(id, activity)
			activity.start()
		}
		s.chatActivityMu.Unlock()
	}
	return id, err
}

func (s *Service) deliverWorkerConversation(ctx context.Context, jobID int, delivery harness.WorkerDelivery) error {
	c := delivery.Conversation
	if c.Validate() != nil {
		return errors.New("invalid retained conversation delivery")
	}
	if c.Channel != "telegram" && c.Channel != "whatsapp" {
		return s.ConsumeConversationDelivery(jobID, delivery, nil)
	}
	cfg := s.Settings.Snapshot()
	if c.Channel == "telegram" && !cfg.Channels.Telegram.Enabled || c.Channel == "whatsapp" && !cfg.Channels.WhatsApp.Enabled {
		return errors.New("retained conversation transport is disabled")
	}
	if err := s.validateOrigin(orchestrator.Origin{Channel: c.Channel, Conversation: c.Conversation}); err != nil {
		return err
	}
	router, ok := s.conversationDelivery().(channel.ConversationEventRouter)
	if !ok {
		return errors.New("retained conversation response router unavailable")
	}
	return s.ConsumeConversationDelivery(jobID, delivery, func(transformed harness.WorkerDelivery) error {
		return router.DeliverEvent(ctx, c.Channel, c.Conversation, transformed.ReceiptID(), transformed.Event)
	})
}

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
	if delivery.Event.Done && !delivery.Event.Continues && !delivery.RequestOnly &&
		(delivery.Event.Kind == core.EventFinal || delivery.Event.Kind == core.EventError) {
		s.stopJobChatActivity(jobID)
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
