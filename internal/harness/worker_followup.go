package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

// followup runs under the same operation/election fence as initial admission.
// One execution retains every batch and its ordered journal until settlement.
func (h *workerHost) followup(request WorkerRequest) WorkerReply {
	c := request.Conversation
	if c.Validate() != nil || c.Recovery || request.Prompt == "" || request.Workflow != nil || request.Agent != nil || request.Control != nil {
		return WorkerReply{Error: "invalid worker conversation follow-up"}
	}
	data, _ := json.Marshal(struct {
		Conversation    WorkerConversation
		Prompt, Message string
	}{*c, request.Prompt, request.Message})
	digest := sha256.Sum256(data)
	h.mu.Lock()
	e := h.executions[request.Sequence]
	if e == nil || e.Conversation == nil || e.Key != request.Key ||
		c.Channel != e.Conversation.Channel || c.Conversation != e.Conversation.Conversation || c.ExecutionID != e.Conversation.ExecutionID {
		h.mu.Unlock()
		return WorkerReply{Error: "follow-up requires the exact retained conversation"}
	}
	for _, prior := range e.Followups {
		if prior.Conversation.SourceMessageID == c.SourceMessageID {
			if prior.Digest != digest {
				h.mu.Unlock()
				return WorkerReply{Error: "worker follow-up payload changed"}
			}
			reply := workerFollowupReply(e.WorkerExecution, prior)
			h.mu.Unlock()
			return reply
		}
	}
	if e.Done || e.Cancelled || !e.SourcesReserved || e.Error != "" || len(e.Followups) >= 1024 {
		h.mu.Unlock()
		return WorkerReply{Error: "conversation cannot admit a follow-up"}
	}
	previous := *e.Conversation
	sources := slices.Clone(previous.Sources)
	for _, source := range c.Sources {
		if slices.Contains(sources, source) {
			h.mu.Unlock()
			return WorkerReply{Error: "follow-up sources overlap the retained execution"}
		}
		sources = append(sources, source)
	}
	if len(sources) > 1024 {
		h.mu.Unlock()
		return WorkerReply{Error: "conversation source capacity exceeded"}
	}
	slices.Sort(sources)
	batch := *c
	batch.Sources = slices.Clone(c.Sources)
	e.Followups = append(e.Followups, WorkerConversationFollowup{Conversation: batch, Digest: digest, Unconfirmed: true})
	index := len(e.Followups) - 1
	if h.saveAdmissionLocked(e) != nil {
		h.failJournalLocked()
		h.mu.Unlock()
		return WorkerReply{Error: "persist worker follow-up failed"}
	}
	// A terminal racing Send must observe its proven reservation/restoration result.
	e.conversationSending = true
	h.mu.Unlock()
	receipt := h.info.ID + ":" + strconv.FormatUint(e.Sequence, 10) + ":followup:" + c.SourceMessageID
	var err error
	if h.conversation == nil {
		err = errors.New("worker conversation reservation unavailable")
	} else {
		err = h.conversation(h.supervisor.HarnessConfig().Cwd, request.Key, batch, receipt, nil)
	}
	h.mu.Lock()
	if h.failure != nil {
		h.mu.Unlock()
		return WorkerReply{Error: "worker follow-up journal unavailable"}
	}
	e.Followups[index].Reserved = err == nil
	if err == nil {
		next := previous
		next.SourceMessageID, next.Sources = c.SourceMessageID, sources
		e.Conversation = &next
	}
	if h.saveAdmissionLocked(e) != nil {
		h.failJournalLocked()
		h.mu.Unlock()
		return WorkerReply{Error: "persist worker follow-up reservation failed"}
	}
	h.mu.Unlock()
	if err == nil {
		// Only the acknowledgement is bounded; the active turn and queue retain
		// their original worker lifetime. Strict admission cannot start a new turn.
		ctx, cancel := context.WithTimeout(h.supervisor.ctx, 5*time.Second)
		_, _, err = h.supervisor.send(ctx, request.Key, request.Prompt, request.Message, func(event core.Event) { h.captureSource(e, event, batch.SourceMessageID) }, true)
		cancel()
		var rejected *SendNotAdmittedError
		if errors.As(err, &rejected) {
			if h.conversation(h.supervisor.HarnessConfig().Cwd, request.Key, batch, receipt, err) != nil {
				h.mu.Lock()
				h.failJournalLocked()
				h.mu.Unlock()
				return WorkerReply{Error: "restore worker follow-up admission failed"}
			}
			h.mu.Lock()
			e.Conversation = &previous
			e.Followups[index].Restored = true
			h.mu.Unlock()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	followup := &e.Followups[index]
	followup.Unconfirmed = err != nil && followup.Reserved && !followup.Restored
	if err != nil {
		if !followup.Reserved {
			followup.Error = "worker conversation source reservation refused"
		} else {
			followup.Error = err.Error()
		}
	}
	if followup.Unconfirmed {
		e.Error = "worker follow-up provider admission unconfirmed"
	}
	if h.saveAdmissionLocked(e) != nil {
		h.failJournalLocked()
		return WorkerReply{Error: "persist worker follow-up result failed"}
	}
	h.bytes -= e.pendingBytes
	pending := e.pendingEvents
	e.pendingEvents, e.pendingBytes, e.conversationSending = nil, 0, false
	for _, item := range pending {
		h.captureLocked(e, item.Event, item.RequestOnly, item.Source)
	}
	if followup.Unconfirmed && !e.Done {
		h.captureLocked(e, core.Event{Kind: core.EventStatus, Text: "Follow-up admission requires reconciliation", Execution: &core.ExecutionStatus{State: "degraded"}}, false, batch.SourceMessageID)
	}
	if h.failure != nil || h.saveAdmissionLocked(e) != nil {
		h.failJournalLocked()
		return WorkerReply{Error: "worker follow-up journal unavailable"}
	}
	return workerFollowupReply(e.WorkerExecution, *followup)
}

func workerFollowupReply(e WorkerExecution, followup WorkerConversationFollowup) WorkerReply {
	reply := WorkerReply{Executions: []WorkerExecution{e}}
	if followup.Unconfirmed && !e.Done {
		reply.Error = "worker follow-up admission unconfirmed; execution retained"
	} else if !followup.Reserved || followup.Restored {
		reply.Error = followup.Error
	}
	return reply
}
