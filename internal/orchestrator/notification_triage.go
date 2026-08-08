package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/agentdocs"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	notificationTriageSchema = "spynel.notification-triage/v1"
	maxTriageAttempts        = 3
	maxTriageInputBytes      = 48 << 10
	maxTriageOutputBytes     = 16 << 10
)

type TriageResult struct {
	Schema           string         `json:"schema"`
	Decision         string         `json:"decision"`
	Message          string         `json:"message"`
	Question         string         `json:"question,omitempty"`
	Choices          []string       `json:"choices,omitempty"`
	NextAction       string         `json:"next_action,omitempty"`
	Urgency          string         `json:"urgency"`
	ResponseRequired bool           `json:"response_required"`
	FollowUp         FollowUpPolicy `json:"follow_up"`
}

type FollowUpPolicy struct {
	Enabled      bool `json:"enabled"`
	AfterMinutes int  `json:"after_minutes,omitempty"`
	MaxReminders int  `json:"max_reminders,omitempty"`
}

type TriageEvent struct {
	ID            string        `json:"id"`
	TaskID        string        `json:"task_id"`
	TransitionID  string        `json:"transition_id"`
	Outcome       string        `json:"outcome"`
	Origin        string        `json:"origin"`
	PolicyHash    string        `json:"policy_hash"`
	TaskFile      string        `json:"task_file"`
	State         string        `json:"state"`
	Attempts      int           `json:"attempts"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	NextAttemptAt time.Time     `json:"next_attempt_at"`
	LastError     string        `json:"last_error,omitempty"`
	Result        *TriageResult `json:"result,omitempty"`
}

type ActionRequest struct {
	ID                    string           `json:"id"`
	TriageEventID         string           `json:"triage_event_id"`
	TaskID                string           `json:"task_id"`
	TransitionID          string           `json:"transition_id"`
	Outcome               string           `json:"outcome"`
	Origin                string           `json:"origin"`
	UserIdentity          string           `json:"user_identity"`
	SentChannel           string           `json:"sent_channel,omitempty"`
	DeliveryEventID       string           `json:"delivery_event_id,omitempty"`
	Question              string           `json:"question"`
	Choices               []string         `json:"choices,omitempty"`
	NextAction            string           `json:"next_action,omitempty"`
	Urgency               string           `json:"urgency"`
	State                 string           `json:"state"`
	CreatedAt             time.Time        `json:"created_at"`
	SentAt                time.Time        `json:"sent_at,omitempty"`
	ReminderDueAt         time.Time        `json:"reminder_due_at,omitempty"`
	ReminderCount         int              `json:"reminder_count"`
	MaxReminders          int              `json:"max_reminders"`
	AcknowledgedAt        time.Time        `json:"acknowledged_at,omitempty"`
	Resolution            string           `json:"resolution,omitempty"`
	EscalationUnavailable string           `json:"escalation_unavailable,omitempty"`
	Deliveries            []ActionDelivery `json:"deliveries,omitempty"`
}

type ActionDelivery struct {
	EventID          string    `json:"event_id"`
	Origin           string    `json:"origin"`
	NativeMessageIDs []string  `json:"native_message_ids,omitempty"`
	SentAt           time.Time `json:"sent_at"`
	Kind             string    `json:"kind"`
}

// ActionRequestStatus is a bounded, body-free inspection view. It deliberately
// excludes the question, choices, recipient, and transport message IDs.
type ActionRequestStatus struct {
	State         string
	SentChannel   string
	ReminderDueAt time.Time
	ReminderCount int
	MaxReminders  int
	Acknowledged  bool
}

func stableTriageID(taskID, transitionID, outcome, origin string, outcomes map[string]bool) (string, string) {
	keys := make([]string, 0, len(outcomes))
	for key, enabled := range outcomes {
		if enabled {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	policy := origin + "\x00" + strings.Join(keys, ",")
	policySum := sha256.Sum256([]byte(policy))
	policyHash := hex.EncodeToString(policySum[:8])
	sum := sha256.Sum256([]byte(taskID + "\x00" + transitionID + "\x00" + outcome + "\x00" + policyHash))
	return hex.EncodeToString(sum[:16]), policyHash
}

func (m *Manager) enqueueTriage(document Document, lease Lease, status, path string, policy NotificationPolicy) error {
	taskID := stringField(document, "id")
	if taskID == "" {
		taskID = lease.ID
	}
	transitionID := lease.ID + ":" + status
	origin := policy.Origin.Channel + "/" + policy.Origin.Conversation
	id, policyHash := stableTriageID(taskID, transitionID, status, origin, policy.Outcomes)
	directory := m.Config.StatePath("runtime", "notification-triage")
	target := filepath.Join(directory, id+".json")
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	now := m.notificationNow()
	event := TriageEvent{ID: id, TaskID: taskID, TransitionID: transitionID, Outcome: status, Origin: origin, PolicyHash: policyHash, TaskFile: path, State: "pending", CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
	return writePrivateJSON(target, event)
}

func (m *Manager) notificationNow() time.Time {
	if m.Outbox != nil && m.Outbox.Now != nil {
		return m.Outbox.Now().UTC()
	}
	return time.Now().UTC()
}

func writePrivateJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(path, append(data, '\n'), 0o600)
}

func readPrivateJSON(path string, value any, limit int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > limit {
		return errors.New("durable notification record exceeds size limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (m *Manager) processNotificationTriage(ctx context.Context) error {
	directory := m.Config.StatePath("runtime", "notification-triage")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, item.Name())
		var event TriageEvent
		if err := readPrivateJSON(path, &event, 128<<10); err != nil {
			m.log("notification triage record rejected: " + err.Error())
			continue
		}
		if event.State == "completed" || event.NextAttemptAt.After(m.notificationNow()) {
			continue
		}
		if err := m.processOneTriage(ctx, path, &event); err != nil {
			m.log("notification triage deferred: " + err.Error())
		}
		break // bounded concurrency: one notification-agent session per scan
	}
	return nil
}

// startNotificationTriage keeps the provider-backed notification session out
// of the deterministic scan critical section. A slow or unavailable provider
// must not delay task/review dispatch or transition reconciliation. The CAS
// also bounds triage to one session per manager while later scans continue.
func (m *Manager) startNotificationTriage(ctx context.Context) {
	if !m.notificationTriageRunning.CompareAndSwap(false, true) {
		return
	}
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		defer m.notificationTriageRunning.Store(false)
		if err := m.processNotificationTriage(ctx); err != nil {
			m.log("notification triage scan deferred: " + err.Error())
		}
	}()
}

func (m *Manager) processOneTriage(parent context.Context, eventPath string, event *TriageEvent) error {
	event.State = "running"
	event.UpdatedAt = m.notificationNow()
	if err := writePrivateJSON(eventPath, event); err != nil {
		return err
	}
	document, err := ReadDocument(event.TaskFile)
	if err != nil {
		return m.deferTriage(eventPath, event, err)
	}
	prompt, err := m.notificationTriagePrompt(*event, document)
	if err != nil {
		return m.deferTriage(eventPath, event, err)
	}
	timeout := 2 * time.Minute
	if m.notificationTriageTimeout > 0 {
		timeout = m.notificationTriageTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var output strings.Builder
	emit := func(item core.Event) {
		text := item.Text
		if item.FinalText != nil {
			text = *item.FinalText
		}
		if item.Kind == core.EventFinal {
			output.Reset()
		}
		if remain := maxTriageOutputBytes - output.Len(); remain > 0 {
			if len(text) > remain {
				text = text[:remain]
			}
			output.WriteString(text)
		}
	}
	session := "orchestrator:notification:" + event.ID
	_, _, sendErr := m.Harness.Send(ctx, session, prompt, emit)
	if sendErr != nil || ctx.Err() != nil {
		if sendErr == nil {
			sendErr = ctx.Err()
		}
		return m.deferTriage(eventPath, event, sendErr)
	}
	result, err := parseTriageResult(output.String(), event.Outcome)
	if err != nil {
		return m.deferTriage(eventPath, event, err)
	}
	if result.Decision == "notify" {
		if result.ResponseRequired {
			request, err := m.createActionRequest(*event, result)
			if err != nil {
				return m.deferTriage(eventPath, event, err)
			}
			if _, err := m.Outbox.EnqueueLinked(event.ID, event.Outcome, event.Origin, actionableNotificationMessage(result), request.ID, "action_request"); err != nil {
				return m.deferTriage(eventPath, event, err)
			}
		} else if _, err := m.Outbox.Enqueue(event.ID, event.Outcome, event.Origin, result.Message); err != nil {
			return m.deferTriage(eventPath, event, err)
		}
	}
	event.State = "completed"
	event.Result = &result
	event.UpdatedAt = m.notificationNow()
	event.LastError = ""
	return writePrivateJSON(eventPath, event)
}

func (m *Manager) deferTriage(path string, event *TriageEvent, cause error) error {
	event.Attempts++
	event.UpdatedAt = m.notificationNow()
	event.LastError = truncateLine(cause.Error(), 240)
	if event.Attempts >= maxTriageAttempts {
		doc, readErr := ReadDocument(event.TaskFile)
		if readErr != nil {
			return errors.Join(cause, readErr)
		}
		message := FormatTaskNotification(doc, event.Outcome, event.TaskID)
		if event.Outcome == "waiting" || event.Outcome == "failed" {
			result := TriageResult{Decision: "notify", Message: message, Question: fallbackQuestion(event.Outcome), Urgency: "normal", ResponseRequired: true, FollowUp: FollowUpPolicy{Enabled: true, AfterMinutes: 60, MaxReminders: 2}}
			request, requestErr := m.createActionRequest(*event, result)
			if requestErr != nil {
				return errors.Join(cause, requestErr)
			}
			if _, writeErr := m.Outbox.EnqueueLinked(event.ID, event.Outcome, event.Origin, actionableNotificationMessage(result), request.ID, "action_request"); writeErr != nil {
				return errors.Join(cause, writeErr)
			}
			event.Result = &result
		} else if _, enqueueErr := m.Outbox.Enqueue(event.ID, event.Outcome, event.Origin, message); enqueueErr != nil {
			return errors.Join(cause, enqueueErr)
		}
		event.State = "completed"
	} else {
		delay := time.Second * time.Duration(1<<min(event.Attempts-1, 8))
		event.State = "pending"
		event.NextAttemptAt = event.UpdatedAt.Add(delay)
	}
	if err := writePrivateJSON(path, event); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func fallbackQuestion(outcome string) string {
	if outcome == "waiting" {
		return "What should I use to unblock this work?"
	}
	return "Would you like me to retry, change approach, or stop here?"
}

func actionableNotificationMessage(result TriageResult) string {
	parts := []string{result.Message, result.Question}
	if len(result.Choices) > 0 {
		choices := make([]string, 0, len(result.Choices))
		for _, choice := range result.Choices {
			choices = append(choices, "- "+choice)
		}
		parts = append(parts, strings.Join(choices, "\n"))
	}
	if result.NextAction != "" {
		parts = append(parts, result.NextAction)
	}
	return strings.Join(parts, "\n\n")
}

func parseTriageResult(raw, outcome string) (TriageResult, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return TriageResult{}, errors.New("notification triage returned no JSON object")
	}
	var result TriageResult
	decoder := json.NewDecoder(strings.NewReader(raw[start : end+1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode notification triage: %w", err)
	}
	if result.Schema != notificationTriageSchema {
		return result, errors.New("unsupported notification triage schema")
	}
	if result.Decision != "notify" && result.Decision != "skip" {
		return result, errors.New("invalid notification triage decision")
	}
	if result.Urgency != "low" && result.Urgency != "normal" && result.Urgency != "urgent" {
		return result, errors.New("invalid notification urgency")
	}
	if result.Decision == "skip" {
		if result.Message != "" || result.Question != "" || result.ResponseRequired || result.FollowUp.Enabled {
			return result, errors.New("skip result contains delivery fields")
		}
		return result, nil
	}
	result.Message = cleanNotificationLine(result.Message)
	result.Question = cleanNotificationLine(result.Question)
	result.NextAction = cleanNotificationLine(result.NextAction)
	if result.Message == "" || utf8.RuneCountInString(result.Message) > 700 || containsAbsolutePath(result.Message) {
		return result, errors.New("unsafe notification message")
	}
	if result.ResponseRequired && (result.Question == "" || (outcome != "waiting" && outcome != "failed")) {
		return result, errors.New("response requirement is inconsistent with outcome")
	}
	if !result.ResponseRequired && (result.Question != "" || len(result.Choices) > 0) {
		return result, errors.New("non-actionable result contains response fields")
	}
	if utf8.RuneCountInString(result.Question) > 280 || utf8.RuneCountInString(result.NextAction) > 280 || len(result.Choices) > 3 {
		return result, errors.New("notification action fields exceed limits")
	}
	for i := range result.Choices {
		result.Choices[i] = cleanNotificationLine(result.Choices[i])
		if result.Choices[i] == "" || utf8.RuneCountInString(result.Choices[i]) > 100 {
			return result, errors.New("invalid notification choice")
		}
	}
	if result.ResponseRequired != result.FollowUp.Enabled {
		return result, errors.New("follow-up policy does not match response requirement")
	}
	if result.FollowUp.Enabled && (result.FollowUp.AfterMinutes < 5 || result.FollowUp.AfterMinutes > 10080 || result.FollowUp.MaxReminders < 1 || result.FollowUp.MaxReminders > 5) {
		return result, errors.New("invalid follow-up policy")
	}
	return result, nil
}

func (m *Manager) notificationTriagePrompt(event TriageEvent, document Document) (string, error) {
	data, err := os.ReadFile(m.Config.StatePath("prompts", "notification.md"))
	if err != nil {
		return "", err
	}
	title := truncateLine(stringField(document, "title"), notificationTitleRunes)
	summary := "No valid bounded summary was recorded."
	if value, ok := parseNotificationSummary(document); ok && summaryMatchesStatus(value, event.Outcome) {
		summary = fmt.Sprintf("Outcome: %s\nEvidence: %s\nUncertainty: %s", value.Outcome, value.Evidence, value.Uncertainty)
	}
	progress := boundedProgress(document.Body, 6, 6000)
	prompt := string(data)
	for key, value := range map[string]string{"{{EVENT_ID}}": event.ID, "{{TASK_ID}}": event.TaskID, "{{OUTCOME}}": event.Outcome, "{{TITLE}}": title, "{{SUMMARY}}": summary, "{{PROGRESS}}": progress} {
		prompt = strings.ReplaceAll(prompt, key, value)
	}
	prompt = agentdocs.InjectPromptGuidance(prompt)
	if len(prompt) > maxTriageInputBytes {
		return "", errors.New("notification triage prompt exceeds bounded input limit")
	}
	return prompt, nil
}

func boundedProgress(body string, maxEntries, maxBytes int) string {
	lines := strings.Split(body, "\n")
	selected := make([]string, 0, maxEntries)
	for i := len(lines) - 1; i >= 0 && len(selected) < maxEntries; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "-") {
			line = truncateLine(line, 600)
			if line != "" {
				selected = append(selected, line)
			}
		}
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	result := strings.Join(selected, "\n")
	if len(result) > maxBytes {
		result = result[len(result)-maxBytes:]
	}
	return result
}

func (m *Manager) createActionRequest(event TriageEvent, result TriageResult) (ActionRequest, error) {
	m.notificationRequestMu.Lock()
	defer m.notificationRequestMu.Unlock()
	id := event.ID
	identity := event.Origin
	if principal, ok := m.boundPrincipal(event.Origin); ok {
		identity = principal
	}
	request := ActionRequest{ID: id, TriageEventID: event.ID, TaskID: event.TaskID, TransitionID: event.TransitionID, Outcome: event.Outcome, Origin: event.Origin, UserIdentity: identity, Question: result.Question, Choices: result.Choices, NextAction: result.NextAction, Urgency: result.Urgency, State: "pending_delivery", CreatedAt: m.notificationNow(), MaxReminders: result.FollowUp.MaxReminders}
	path := m.Config.StatePath("runtime", "action-requests", id+".json")
	if err := readPrivateJSON(path, &request, 64<<10); err == nil {
		return request, nil
	}
	return request, writePrivateJSON(path, request)
}

func (m *Manager) markActionDelivered(entry OutboxEntry) error {
	if entry.ActionRequestID == "" {
		return nil
	}
	m.notificationRequestMu.Lock()
	defer m.notificationRequestMu.Unlock()
	path := m.Config.StatePath("runtime", "action-requests", entry.ActionRequestID+".json")
	var request ActionRequest
	if err := readPrivateJSON(path, &request, 64<<10); err != nil {
		return err
	}
	request.Deliveries = upsertActionDelivery(request.Deliveries, ActionDelivery{
		EventID: entry.ID, Origin: entry.Origin, NativeMessageIDs: boundedNativeMessageIDs(entry.NativeMessageIDs),
		SentAt: m.notificationNow(), Kind: entry.Kind,
	})
	if entry.Kind == "reminder" {
		if request.State != "awaiting_response" {
			return nil
		}
		request.ReminderCount++
		request.ReminderDueAt = m.notificationNow().Add(reminderDelay(request.ReminderCount))
		return writePrivateJSON(path, request)
	}
	if request.State != "pending_delivery" {
		return nil
	}
	request.State = "awaiting_response"
	request.SentAt = m.notificationNow()
	request.DeliveryEventID = entry.ID
	if origin, err := ParseOrigin(entry.Origin); err == nil {
		request.SentChannel = origin.Channel
	}
	after := 60
	if event := m.loadTriageResult(request.TriageEventID); event != nil && event.Result != nil && event.Result.FollowUp.AfterMinutes > 0 {
		after = event.Result.FollowUp.AfterMinutes
	}
	request.ReminderDueAt = request.SentAt.Add(time.Duration(after) * time.Minute)
	return writePrivateJSON(path, request)
}

func upsertActionDelivery(existing []ActionDelivery, delivery ActionDelivery) []ActionDelivery {
	for index := range existing {
		if existing[index].EventID == delivery.EventID {
			existing[index] = delivery
			return existing
		}
	}
	if len(existing) >= 8 {
		existing = append([]ActionDelivery(nil), existing[len(existing)-7:]...)
	}
	return append(existing, delivery)
}

func boundedNativeMessageIDs(values []string) []string {
	if len(values) > 8 {
		values = values[len(values)-8:]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || utf8.RuneCountInString(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
			continue
		}
		result = append(result, value)
	}
	return result
}

func reminderDelay(count int) time.Duration {
	if count < 0 {
		count = 0
	}
	if count > 5 {
		count = 5
	}
	return time.Hour * time.Duration(1<<count)
}

// ActionRequestStatusForTask returns the newest request for one durable task.
// Corrupt and oversized records are ignored rather than exposed through a
// diagnostic surface.
func (m *Manager) ActionRequestStatusForTask(taskID string) (ActionRequestStatus, bool) {
	entries, err := os.ReadDir(m.Config.StatePath("runtime", "action-requests"))
	if err != nil {
		return ActionRequestStatus{}, false
	}
	var selected ActionRequest
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var request ActionRequest
		if readPrivateJSON(m.Config.StatePath("runtime", "action-requests", entry.Name()), &request, 64<<10) != nil || request.TaskID != taskID {
			continue
		}
		if !found || request.CreatedAt.After(selected.CreatedAt) || (request.CreatedAt.Equal(selected.CreatedAt) && request.ID < selected.ID) {
			selected, found = request, true
		}
	}
	if !found {
		return ActionRequestStatus{}, false
	}
	return ActionRequestStatus{
		State: selected.State, SentChannel: selected.SentChannel,
		ReminderDueAt: selected.ReminderDueAt, ReminderCount: selected.ReminderCount,
		MaxReminders: selected.MaxReminders, Acknowledged: !selected.AcknowledgedAt.IsZero(),
	}, true
}

// PendingActionSummary returns only requests for the exact authorized
// conversation. It is prompt context, not an acknowledgement signal.
func (m *Manager) PendingActionSummary(origin string, nativeReplyID ...string) string {
	directory := m.Config.StatePath("runtime", "action-requests")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return ""
	}
	lines := make([]string, 0, 8)
	replyID := ""
	if len(nativeReplyID) > 0 {
		replyID = strings.TrimSpace(nativeReplyID[0])
	}
	correlated := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || len(lines) >= 8 {
			continue
		}
		var request ActionRequest
		if readPrivateJSON(filepath.Join(directory, entry.Name()), &request, 64<<10) != nil || request.State != "awaiting_response" {
			continue
		}
		matchedReply := replyID != "" && actionRequestMatchesReply(request, origin, replyID)
		if replyID != "" && !matchedReply {
			continue
		}
		if replyID == "" && !actionRequestMatchesOrigin(request, origin) {
			continue
		}
		correlated = correlated || matchedReply
		line := fmt.Sprintf("- task %s asks: %s", truncateLine(request.TaskID, 120), truncateLine(request.Question, 280))
		if len(request.Choices) > 0 {
			line += " Choices: " + strings.Join(request.Choices, " | ")
		}
		lines = append(lines, truncateLine(line, 700))
	}
	if len(lines) == 0 {
		return ""
	}
	header := "Pending action requests for this exact conversation:\n"
	if correlated {
		header = "This inbound message is an explicit native reply to this pending action request:\n"
	}
	return header + strings.Join(lines, "\n") + "\nTreat a message as an answer only when it explicitly addresses a question above. Clarify ambiguity; do not acknowledge unrelated messages. Record a validated answer in the task and resume it through the normal durable lifecycle."
}

func actionRequestMatchesOrigin(request ActionRequest, origin string) bool {
	if request.Origin == origin {
		return true
	}
	for _, delivery := range request.Deliveries {
		if delivery.Origin == origin {
			return true
		}
	}
	return false
}

func actionRequestMatchesReply(request ActionRequest, origin, replyID string) bool {
	for _, delivery := range request.Deliveries {
		if delivery.Origin != origin {
			continue
		}
		for _, messageID := range delivery.NativeMessageIDs {
			if messageID == replyID {
				return true
			}
		}
	}
	return false
}

// ReconcileActionRequests observes a communication-agent resolution only
// after the durable task has left the request's originating outcome. Merely
// receiving a later message never resolves a request.
func (m *Manager) reconcileActionRequests() error {
	directory := m.Config.StatePath("runtime", "action-requests")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pending := map[string]ActionRequest{}
	paths := map[string]string{}
	taskIDs := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || len(pending) >= 256 {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		var request ActionRequest
		if readPrivateJSON(path, &request, 64<<10) == nil && request.State == "awaiting_response" {
			pending[request.ID] = request
			paths[request.ID] = path
			taskIDs[request.TaskID] = true
		}
	}
	if len(pending) == 0 {
		return nil
	}
	documents, err := m.semanticDocuments(taskIDs)
	if err != nil {
		return err
	}
	for requestID, request := range pending {
		document, exists := documents[request.TaskID]
		outcome := actionRequestOutcome(request)
		if !exists || outcome == "" || stringField(document, "status") == outcome {
			continue
		}
		m.notificationRequestMu.Lock()
		if err := readPrivateJSON(paths[requestID], &request, 64<<10); err != nil || request.State != "awaiting_response" {
			m.notificationRequestMu.Unlock()
			continue
		}
		request.State = "answered"
		request.AcknowledgedAt = m.notificationNow()
		request.ReminderDueAt = time.Time{}
		request.Resolution = "Validated durable task transition to " + stringField(document, "status") + "."
		if err := writePrivateJSON(paths[requestID], request); err != nil {
			m.notificationRequestMu.Unlock()
			return err
		}
		m.notificationRequestMu.Unlock()
		if err := m.cancelActionReminders(request.ID); err != nil {
			return err
		}
	}
	return nil
}

func actionRequestOutcome(request ActionRequest) string {
	switch request.Outcome {
	case "waiting", "failed", "cancelled", "done":
		return request.Outcome
	case "":
	default:
		return ""
	}
	// Records created before the outcome field was added still carry the
	// terminal status as the final transition-ID component. If it cannot be
	// recovered, fail closed without acknowledging the request.
	if index := strings.LastIndex(request.TransitionID, ":"); index >= 0 {
		outcome := request.TransitionID[index+1:]
		switch outcome {
		case "waiting", "failed", "cancelled", "done":
			return outcome
		}
	}
	return ""
}

func (m *Manager) cancelActionReminders(requestID string) error {
	return m.Outbox.cancelPendingActionReminders(requestID)
}

// processActionReminders is called by the elected primary's semantic
// heartbeat. With no explicit trusted cross-channel binding it deliberately
// remains on the authorized origin and records that fallback is unavailable.
func (m *Manager) processActionReminders(ctx context.Context) error {
	if !m.primaryOwned.Load() || ctx.Err() != nil {
		return ctx.Err()
	}
	directory := m.Config.StatePath("runtime", "action-requests")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := m.notificationNow()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		var request ActionRequest
		m.notificationRequestMu.Lock()
		readErr := readPrivateJSON(path, &request, 64<<10)
		if readErr != nil || request.State != "awaiting_response" || request.ReminderDueAt.After(now) || request.ReminderCount >= request.MaxReminders {
			m.notificationRequestMu.Unlock()
			continue
		}
		if request.Urgency != "urgent" {
			if quietEnd, quiet := notificationQuietEnd(now, m.Config.Notifications.QuietHours); quiet {
				request.ReminderDueAt = quietEnd
				if err := writePrivateJSON(path, request); err != nil {
					m.notificationRequestMu.Unlock()
					return err
				}
				m.notificationRequestMu.Unlock()
				continue
			}
		}
		m.notificationRequestMu.Unlock()
		origin, selectionReason, parseErr := m.reminderOrigin(request.Origin)
		if parseErr != nil {
			m.notificationRequestMu.Lock()
			if readPrivateJSON(path, &request, 64<<10) != nil || request.State != "awaiting_response" {
				m.notificationRequestMu.Unlock()
				continue
			}
			request.EscalationUnavailable = "origin is no longer authorized"
			_ = writePrivateJSON(path, request)
			m.notificationRequestMu.Unlock()
			continue
		}
		message := "I’m still waiting for your answer: " + request.Question
		if len(request.Choices) > 0 {
			message += " Options: " + strings.Join(request.Choices, "; ") + "."
		}
		key := fmt.Sprintf("action-reminder:%s:%d", request.ID, request.ReminderCount+1)
		selectedOrigin := origin.Channel + "/" + origin.Conversation
		_, enqueueErr := m.Outbox.EnqueueLinked(key, "reminder", selectedOrigin, message, request.ID, "reminder")
		if enqueueErr != nil {
			return enqueueErr
		}
		m.notificationRequestMu.Lock()
		if err := readPrivateJSON(path, &request, 64<<10); err != nil {
			m.notificationRequestMu.Unlock()
			return err
		}
		if request.State != "awaiting_response" {
			m.notificationRequestMu.Unlock()
			if err := m.cancelActionReminders(request.ID); err != nil {
				return err
			}
			continue
		}
		request.EscalationUnavailable = selectionReason
		if err := writePrivateJSON(path, request); err != nil {
			m.notificationRequestMu.Unlock()
			return err
		}
		m.notificationRequestMu.Unlock()
	}
	return nil
}

func notificationQuietEnd(now time.Time, policy config.QuietHours) (time.Time, bool) {
	if !policy.Enabled {
		return time.Time{}, false
	}
	start, startErr := time.Parse("15:04", strings.TrimSpace(policy.Start))
	end, endErr := time.Parse("15:04", strings.TrimSpace(policy.End))
	if startErr != nil || endErr != nil {
		return time.Time{}, false
	}
	now = now.UTC()
	startMinute := start.Hour()*60 + start.Minute()
	endMinute := end.Hour()*60 + end.Minute()
	nowMinute := now.Hour()*60 + now.Minute()
	quiet := false
	endDay := now
	if startMinute < endMinute {
		quiet = nowMinute >= startMinute && nowMinute < endMinute
	} else {
		quiet = nowMinute >= startMinute || nowMinute < endMinute
		if nowMinute >= startMinute {
			endDay = now.AddDate(0, 0, 1)
		}
	}
	if !quiet {
		return time.Time{}, false
	}
	return time.Date(endDay.Year(), endDay.Month(), endDay.Day(), end.Hour(), end.Minute(), 0, 0, time.UTC), true
}

func (m *Manager) loadTriageResult(id string) *TriageEvent {
	var event TriageEvent
	if readPrivateJSON(m.Config.StatePath("runtime", "notification-triage", id+".json"), &event, 128<<10) != nil {
		return nil
	}
	return &event
}
