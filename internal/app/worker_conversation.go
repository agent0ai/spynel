package app

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/channel"
	"github.com/agent0ai/spynel/internal/channel/telegram"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/shortid"
)

func (s *Service) dispatchWorkerConversation(ctx context.Context, message core.Message, prompt string, emit core.Emit, additionalSources []string) error {
	executionID, err := shortid.New()
	if err != nil {
		return err
	}
	sources := append([]string{message.SourceMessageID}, additionalSources...)
	sort.Strings(sources)
	c := harness.WorkerConversation{Channel: message.Channel, Conversation: message.Conversation, SourceMessageID: message.SourceMessageID, ExecutionID: executionID, Sources: sources}
	if message.Channel == "telegram" || message.Channel == "whatsapp" {
		// The inbound emitter cannot acknowledge delivery. The ordinary scan
		// consumes through the fallible router after this intake returns.
		_, err := s.Orchestrator.AdmitConversation(ctx, c, prompt, message.Text)
		return err
	}
	activity := newChatActivityEmitter(func(event core.Event) {
		event.RequestID = message.SourceMessageID
		if emit != nil {
			emit(event)
		}
	})
	defer activity.stop()
	terminal := false
	err = s.Orchestrator.Converse(ctx, c, prompt, message.Text, func(_ context.Context, jobID int, delivery harness.WorkerDelivery) error {
		activity.start()
		return s.ConsumeConversationDelivery(jobID, delivery, func(transformed harness.WorkerDelivery) error {
			activity.emit(transformed.Event)
			terminal = transformed.Event.Done && !transformed.Event.Continues &&
				(transformed.Event.Kind == core.EventFinal || transformed.Event.Kind == core.EventError)
			return nil
		})
	})
	// A lost commit/retirement reply cannot replace an already delivered result.
	if terminal {
		return nil
	}
	return err
}

// Worker source reservation and Send share its exact-term fence. Preparing the
// prompt here must never reserve recovery or enclose a worker RPC in that fence.
func (s *Service) dispatchWorkerRecovery(ctx context.Context, origin orchestrator.Origin, candidates []recoveryCandidate) bool {
	entries := make([]history.Entry, 0, len(candidates))
	sources := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		entry := candidate.entry
		if candidate.admitted {
			entry.Admission = "admitted"
		}
		entries = append(entries, entry)
		sources = append(sources, entry.SourceMessageID)
	}
	message, prompt, err := s.recoveryPrompt(origin, entries)
	if err != nil {
		s.recoveryFailure(origin, err)
		return false
	}
	prompt, handled, err := s.prepareConversationPrompt(ctx, message, prompt, s.recoveryEmitter(origin))
	if err != nil {
		s.recoveryFailure(origin, err)
		return false
	}
	if handled {
		return false
	}
	executionID, err := shortid.New()
	if err != nil {
		s.recoveryFailure(origin, err)
		return false
	}
	sort.Strings(sources)
	c := harness.WorkerConversation{Channel: origin.Channel, Conversation: origin.Conversation, SourceMessageID: message.SourceMessageID, ExecutionID: executionID, Sources: sources, Recovery: true}
	execution, err := s.Orchestrator.AdmitConversation(ctx, c, prompt, message.Text)
	if err != nil {
		// Unknown Send must reconcile through the ordinary scan, never through
		// a fabricated terminal failure or a repeated recovery dispatch.
		s.Runtime.LogEvent("info", "recovery", "admission_unconfirmed", "Worker recovery admission was not confirmed")
		return false
	}
	return execution.SourcesReserved
}

// Called before worker source reservation, under the exact owner fence.
func (s *Service) startWorkerConversation(c harness.WorkerConversation) (int, error) {
	if c.Validate() != nil {
		return 0, errors.New("invalid conversation admission")
	}
	if _, err := s.workerConversationRouter(c); err != nil {
		return 0, err
	}
	return s.beginWorkerConversation(c)
}

func (s *Service) beginWorkerConversation(c harness.WorkerConversation) (int, error) {
	key := "chat:" + c.Channel + ":" + c.Conversation
	if job, ok := s.Runtime.JobForSession(key); ok && (job.Kind != "conversation" || job.WorkID != c.ExecutionID) {
		return 0, errors.New("retained conversation conflicts with its live job")
	}
	return s.Runtime.TryBeginJobWithDetails(key, c.Channel, c.Conversation, "Retained conversation", JobDetails{
		Kind: "conversation", WorkID: c.ExecutionID, Provider: s.Settings.Snapshot().Harness.Name,
	})
}

// Called under the Manager's native/exact-owner fence; it must not use RPC.
func (s *Service) attachWorkerConversation(execution harness.WorkerExecution) (int, error) {
	c := execution.Conversation
	if c.Validate() != nil || execution.Key != "chat:"+c.Channel+":"+c.Conversation || !execution.SourcesReserved && !execution.ConversationRefused() || execution.Workflow != nil || execution.Agent != nil {
		return 0, errors.New("invalid retained conversation admission")
	}
	id, err := s.beginWorkerConversation(*c)
	if err == nil && (c.Channel == "tui" || c.Channel == "telegram" || c.Channel == "whatsapp") && !execution.Done && !execution.Cancelled {
		if _, routeErr := s.workerConversationRouter(*c); routeErr != nil {
			return id, nil // Restore the job, but never start unauthorized activity.
		}
		s.chatActivityMu.Lock()
		if len(s.chatActivity[id]) == 0 {
			activity := newChatActivityEmitter(s.recoveryEmitter(orchestrator.Origin{Channel: c.Channel, Conversation: c.Conversation}))
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
	router, err := s.workerConversationRouter(*c)
	if err != nil {
		return err
	}
	if router == nil {
		return s.ConsumeConversationDelivery(jobID, delivery, nil)
	}
	return s.ConsumeConversationDelivery(jobID, delivery, func(transformed harness.WorkerDelivery) error {
		return router.DeliverEvent(ctx, c.Channel, c.Conversation, transformed.ReceiptID(), transformed.Event)
	})
}

func (s *Service) workerConversationRouter(c harness.WorkerConversation) (channel.ConversationEventRouter, error) {
	if c.Channel != "telegram" && c.Channel != "whatsapp" {
		return nil, nil
	}
	cfg := s.Settings.Snapshot()
	if c.Channel == "telegram" && !cfg.Channels.Telegram.Enabled || c.Channel == "whatsapp" && !cfg.Channels.WhatsApp.Enabled {
		return nil, errors.New("retained conversation transport is disabled")
	}
	if err := s.validateOrigin(orchestrator.Origin{Channel: c.Channel, Conversation: c.Conversation}); err != nil {
		return nil, err
	}
	router, ok := s.conversationDelivery().(channel.ConversationEventRouter)
	if !ok {
		return nil, errors.New("retained conversation response router unavailable")
	}
	return router, nil
}

// AdmitWorkerConversation runs inside the worker's existing owner fence.
// Recheck canonical remote authorization before reserving any source.
func AdmitWorkerConversation(root, key string, conversation harness.WorkerConversation, receipt string) error {
	if conversation.Validate() != nil || key != "chat:"+conversation.Channel+":"+conversation.Conversation {
		return errors.New("invalid worker conversation admission")
	}
	store := history.New(filepath.Join(root, ".spynel", "history"))
	var cfg config.Config
	if conversation.Recovery || conversation.Channel == "telegram" || conversation.Channel == "whatsapp" {
		var err error
		cfg, err = config.Load(config.PathForRoot(root))
		if err != nil {
			return err
		}
		if conversation.Channel == "telegram" && !cfg.Channels.Telegram.Enabled || conversation.Channel == "whatsapp" && !cfg.Channels.WhatsApp.Enabled {
			return errors.New("worker conversation transport is disabled")
		}
		if err := validateOrigin(cfg, store, telegram.NewIdentityStore(cfg.StatePath("runtime", "telegram-identities.json")), orchestrator.Origin{Channel: conversation.Channel, Conversation: conversation.Conversation}); err != nil {
			return err
		}
	}
	if conversation.Recovery {
		if !cfg.Orchestrator.RetriggerUnrespondedMessages || strings.Contains(conversation.Conversation, "-group-") {
			return errors.New("worker conversation recovery is disabled or ineligible")
		}
		activation, err := store.ActivateRecovery()
		if err != nil {
			return err
		}
		return store.AdmitRecoveryConversation(conversation.Channel, conversation.Conversation, conversation.ExecutionID, receipt, conversation.Sources, activation, time.Now().UTC().Add(-recoveryWindow), func(entries []history.Entry) error {
			for _, entry := range entries {
				if !eligibleRecoveryCommand(entry.Content) {
					return errors.New("worker recovery source command is ineligible")
				}
			}
			linked, err := durableRecoverySources(orchestrator.WorkflowDirectories(root), conversation.Sources)
			if err != nil || len(linked) != 0 {
				return errors.New("worker recovery source links are unavailable or already own the request")
			}
			return nil
		})
	}
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
