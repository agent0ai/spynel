package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/updater"
)

const (
	WorkerProtocol      = 13
	workerDetachedGrace = 10 * time.Minute
	workerRequestBytes  = 256 * 1024
	workerEventBytes    = 32 * 1024 * 1024
	workerJournalBytes  = 128 * 1024 * 1024
	workerMaxEvents     = 65536
	workerMaxExecutions = 256
	workerPageBytes     = 2 * 1024 * 1024
	workerPageEvents    = 256
)

// WorkerBootstrap travels over a private inherited pipe, never arguments or
// environment. The worker owns the complete supervisor and all adapter state.
type WorkerBootstrap struct {
	Protocol   int
	Token      string
	InstanceID string
	TermToken  string
	Harness    HarnessConfig
}

type WorkerInfo struct {
	Protocol      int
	ID            string
	Socket        string
	PID           int
	EnvironmentID string
	WorkspaceID   string
	ConfigDigest  [32]byte
	Process       updater.ProcessRegistration
}

// Sequence is a strictly increasing admission number, not a provider turn.
// A repeated or retired sequence can never call Send again.
type WorkerRequest struct {
	WorkerID       string
	InstanceID     string
	TermToken      string
	Action         string
	Sequence       uint64
	Key            string
	Prompt         string
	Message        string
	After          uint64
	Expected       uint64
	Workflow       *WorkerWorkflow
	Agent          *WorkerAgent
	Conversation   *WorkerConversation
	Control        *WorkerControl
	Config         *HarnessConfig
	ExpectedConfig [32]byte
	Settings       *WorkerSettings
}

// WorkerSettings carries canonical YAML privately; it must never enter logs.
type WorkerSettings struct {
	Expected [32]byte
	YAML     []byte
}

// WorkerSettingsPlan separates valid saved intent from provider availability.
// Only the trusted canonical-settings validator supplies Unavailable.
type WorkerSettingsPlan struct {
	Commit, Restore func() error
	Unavailable     error
}

// WorkerSettingsAction validates canonical selection and prepares persistence.
// The callbacks run under the worker's configuration/dispatch fence, without RPCs.
type WorkerSettingsAction func(root string, current, next HarnessConfig, settings WorkerSettings) (WorkerSettingsPlan, error)

// WorkerConversationAction reserves sources when sendErr is nil, or restores
// the exact ordinary batch after SendNotAdmittedError. It runs inside the
// worker's existing exact-election fence, without another acquisition or RPC.
type WorkerConversationAction func(root, key string, conversation WorkerConversation, receipt string, sendErr error) error

type WorkerControl struct {
	ID                 string
	ContinuationPrompt string
}

// WorkerAgent retains an ordinary background turn, not a task transition or
// notification result. Its original deadline belongs to the worker lifetime.
type WorkerAgent struct {
	ID        string
	Kind      string
	File      string
	StartedAt time.Time
	Deadline  time.Time
}

// WorkerConversation retains private correlation independently of a controller.
// Sources identifies a request batch or an execution/event snapshot's accumulated sources.
type WorkerConversation struct {
	Channel, Conversation, SourceMessageID, ExecutionID string
	Sources                                             []string
	Recovery                                            bool
}

// WorkerWorkflow identifies the exact claimed work whose provider admission
// the worker reserves. The orchestrator owns validation and durable counters.
type WorkerWorkflow struct {
	LeaseID      string
	ClaimID      string
	OwnerID      string
	WorkID       string
	File         string
	Route        string
	Phase        string
	ClaimAttempt int
}

type WorkerReservation struct {
	FirstAssignedAt        time.Time
	ProviderIterations     int
	ImplementationAttempts int
}

// WorkerWorkflowAction runs inside the worker's exact-election fence.
type WorkerWorkflowAction func(root, key string, work WorkerWorkflow, action string) (WorkerReservation, error)

type WorkerEvent struct {
	Cursor            uint64
	Event             core.Event
	Conversation      *WorkerConversation
	RequestOnly       bool
	Streamed          string
	ConversationTurns int
}

type WorkerExecution struct {
	Sequence          uint64
	Key               string
	ThreadID          string
	Steered           bool
	Done              bool
	Error             string
	LastCursor        uint64
	CommittedCursor   uint64
	Workflow          *WorkerWorkflow
	Reservation       *WorkerReservation
	Agent             *WorkerAgent
	Cancelled         bool
	Conversation      *WorkerConversation
	SourcesReserved   bool
	Followups         []WorkerConversationFollowup
	ConversationTurns int
}

// Followups retain exact batch retry identities without storing prompt text.
type WorkerConversationFollowup struct {
	Conversation WorkerConversation
	Digest       [32]byte
	Error        string
	Reserved     bool
	Restored     bool
	Unconfirmed  bool
}

type WorkerSession struct {
	ThreadID  string
	Active    bool
	Admission string
	Retained  bool
}

type WorkerAvailability struct {
	Available bool
	Detail    string
}

type WorkerReply struct {
	Error        string
	NextSequence uint64
	Executions   []WorkerExecution
	Events       []WorkerEvent
	Interrupted  bool
	Session      *WorkerSession
	Control      *ControlResult
	ConfigDigest [32]byte
	Config       *HarnessConfig
	Models       []Model
	Availability *WorkerAvailability
}

type workerExecution struct {
	WorkerExecution
	file                *os.File
	offsets             []int64
	bytes               int64
	digest              [32]byte
	admissionBytes      int64
	deadlineInterrupted bool
	cancel              context.CancelFunc
	streamed            string
	conversationSending bool
	pendingEvents       []workerPendingEvent
	pendingBytes        int64
}

type workerPendingEvent struct {
	Event       core.Event
	RequestOnly bool
	Source      string
}

type workerHost struct {
	supervisor   *Supervisor
	election     *instance.Election
	info         WorkerInfo
	token        string
	directory    string
	recordPath   string
	stop         chan struct{}
	stopOnce     sync.Once
	operations   operationGate
	mu           sync.Mutex
	lastSeen     time.Time
	next         uint64
	executions   map[uint64]*workerExecution
	bytes        int64
	failure      error
	workflow     WorkerWorkflowAction
	settings     WorkerSettingsAction
	conversation WorkerConversationAction
}

// RunWorker is a private same-binary entry point. Losing its bootstrap pipe,
// HTTP clients or elected controller does not cancel its provider context.
// Explicit shutdown, signal, journal failure or bounded detachment still do.
func RunWorker(ctx context.Context, input io.Reader, ready io.Writer, registry *Registry, workflow WorkerWorkflowAction, settings WorkerSettingsAction, conversation WorkerConversationAction) (runErr error) {
	var bootstrap WorkerBootstrap
	data, err := io.ReadAll(io.LimitReader(input, workerRequestBytes+1))
	if err != nil || len(data) > workerRequestBytes {
		return errors.New("invalid worker bootstrap size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bootstrap); err != nil {
		return errors.New("invalid worker bootstrap")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid worker bootstrap")
	}
	if bootstrap.Protocol != WorkerProtocol || len(bootstrap.Token) < 32 || len(bootstrap.Token) > 128 {
		return errors.New("invalid worker protocol or credential")
	}
	root, err := filepath.EvalSymlinks(bootstrap.Harness.Cwd)
	if err != nil || !filepath.IsAbs(root) {
		return errors.New("worker requires an absolute existing workspace")
	}
	bootstrap.Harness.Cwd = root
	lockPath, err := ProcessOwnershipPath(root)
	if err != nil {
		return err
	}
	recordPath, err := workerRecordPath(lockPath)
	if err != nil {
		return err
	}
	native, err := updater.ProcessRecord(os.Getpid())
	if err != nil || !native.HarnessWorker {
		return errors.New("worker native registration unavailable")
	}
	if err := updater.VerifyProcess(native); err != nil {
		return errors.New("worker native identity unavailable")
	}
	// Unix socket names have a small platform limit; workspace/config paths do not.
	directory, err := os.MkdirTemp("", "spynel-worker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	election, err := instance.New(filepath.Join(root, ".spynel"))
	if err != nil {
		return err
	}
	id, err := election.NewToken()
	if err != nil {
		return err
	}
	info := WorkerInfo{
		Protocol: WorkerProtocol, ID: id, Socket: filepath.Join(directory, "control.sock"), PID: os.Getpid(),
		EnvironmentID: election.EnvironmentID(), WorkspaceID: filepath.Base(lockPath),
		ConfigDigest: ConfigDigest(bootstrap.Harness), Process: native,
	}
	listener, err := net.Listen("unix", info.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(info.Socket, 0600); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	supervisor := NewSupervisor(registry, bootstrap.Harness)
	supervisor.SetProcessStartGuard(lockPath, nil)
	var host *workerHost
	defer func() {
		cancel()
		runErr = errors.Join(runErr, supervisor.Close())
		if current, err := readWorkerRecord(recordPath); err == nil && current.Info.ID == info.ID {
			runErr = errors.Join(runErr, os.Remove(recordPath))
		}
		if host != nil {
			host.mu.Lock()
			defer host.mu.Unlock()
			for _, execution := range host.executions {
				_ = execution.file.Close()
			}
		}
	}()
	startup, startupCancel := context.WithTimeout(workerCtx, 30*time.Second)
	defer startupCancel()
	startupTick := time.NewTicker(100 * time.Millisecond)
	defer startupTick.Stop()
	for {
		busy := false
		ran, err := election.RunForOwner(bootstrap.InstanceID, bootstrap.TermToken, func() error {
			if err := startup.Err(); err != nil {
				return err
			}
			// Guardians may still be joining a confirmed-dead worker's provider.
			if err := supervisor.processOwner.acquire(); err != nil {
				if errors.Is(err, errProcessOwnershipBusy) {
					// A competing live worker owns discovery; do not wait to
					// replace it if that controller later shuts it down.
					record, readErr := readWorkerRecord(recordPath)
					if readErr == nil && errors.Is(updater.VerifyProcess(record.Info.Process), syscall.ESRCH) {
						busy = true
						return nil
					}
				}
				return err
			}
			// Availability reports startup errors; the control plane stays usable.
			_ = supervisor.Start(workerCtx)
			return workerCtx.Err()
		})
		if !ran || err != nil {
			return errors.New("worker startup ownership unavailable")
		}
		if !busy {
			break
		}
		// Never hold election ownership while waiting for provider cleanup.
		select {
		case <-startup.Done():
			return errors.New("worker startup ownership unavailable")
		case <-startupTick.C:
		}
	}
	host = &workerHost{
		supervisor: supervisor, election: election, info: info, token: bootstrap.Token, directory: directory, recordPath: recordPath,
		stop: make(chan struct{}), lastSeen: time.Now(), next: 1, executions: map[uint64]*workerExecution{},
		workflow: workflow, settings: settings, conversation: conversation,
	}
	if err := publishWorkerRecord(recordPath, workerRecord{Info: info, Token: bootstrap.Token}); err != nil {
		return err
	}
	server := &http.Server{
		Handler: http.HandlerFunc(host.serve), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 4096,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()
	if err := json.NewEncoder(ready).Encode(info); err != nil {
		return errors.New("worker readiness pipe unavailable")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-host.stop:
			host.mu.Lock()
			err := host.failure
			host.mu.Unlock()
			return err
		case err := <-served:
			return err
		case <-ticker.C:
			host.expireAgents()
			host.mu.Lock()
			expired := time.Since(host.lastSeen) >= workerDetachedGrace
			host.mu.Unlock()
			if expired {
				return errors.New("worker detached grace expired")
			}
		}
	}
}

func (h *workerHost) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/worker" {
		http.Error(w, "invalid worker request", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		http.Error(w, "worker authorization denied", http.StatusUnauthorized)
		return
	}
	var request WorkerRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, workerRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return
	}
	if request.WorkerID != h.info.ID {
		http.Error(w, "worker identity changed", http.StatusConflict)
		return
	}
	if err := h.operations.LockContext(r.Context()); err != nil {
		http.Error(w, "worker request expired before admission", http.StatusRequestTimeout)
		return
	}
	defer h.operations.Unlock()
	var reply WorkerReply
	ran, err := h.election.RunForOwner(request.InstanceID, request.TermToken, func() error {
		// Election acquisition may wait too; admitted work keeps its own lifetime.
		if err := r.Context().Err(); err != nil {
			return err
		}
		h.mu.Lock()
		if h.failure != nil {
			h.mu.Unlock()
			return errors.New("worker event journal unavailable")
		}
		h.lastSeen = time.Now()
		h.mu.Unlock()
		reply = h.apply(request)
		return nil
	})
	if err != nil {
		http.Error(w, "worker ownership or journal unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ran {
		http.Error(w, "worker controller ownership unavailable", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (h *workerHost) apply(request WorkerRequest) WorkerReply {
	switch request.Action {
	case "inspect":
		cfg := h.supervisor.HarnessConfig()
		available, detail := h.supervisor.Available()
		h.mu.Lock()
		defer h.mu.Unlock()
		reply := WorkerReply{NextSequence: h.next, ConfigDigest: h.info.ConfigDigest, Config: &cfg, Availability: &WorkerAvailability{Available: available, Detail: detail}}
		for _, execution := range h.executions {
			reply.Executions = append(reply.Executions, execution.WorkerExecution)
		}
		sort.Slice(reply.Executions, func(i, j int) bool { return reply.Executions[i].Sequence < reply.Executions[j].Sequence })
		return reply
	case "send":
		return h.send(request)
	case "followup":
		return h.followup(request)
	case "control":
		return h.control(request)
	case "configure":
		return h.configure(request)
	case "models":
		ctx, cancel := context.WithTimeout(h.supervisor.ctx, 10*time.Second)
		defer cancel()
		models, err := h.supervisor.Models(ctx)
		if err != nil {
			return WorkerReply{Error: err.Error()}
		}
		return WorkerReply{Models: models}
	case "events":
		return h.events(request)
	case "commit":
		return h.commit(request)
	case "ack":
		h.mu.Lock()
		defer h.mu.Unlock()
		execution := h.executions[request.Sequence]
		if execution == nil {
			if request.Sequence > 0 && request.Sequence < h.next {
				return WorkerReply{}
			}
			return WorkerReply{Error: "unknown worker execution"}
		}
		if !execution.Done || request.After != execution.LastCursor || execution.CommittedCursor != execution.LastCursor {
			return WorkerReply{Error: "worker execution is not fully committed"}
		}
		if err := os.Remove(execution.file.Name()); err != nil {
			h.failJournalLocked()
			return WorkerReply{Error: "retire worker journal failed"}
		}
		if err := os.Remove(execution.file.Name() + ".cursor"); err != nil && !os.IsNotExist(err) {
			h.failJournalLocked()
			return WorkerReply{Error: "retire worker cursor failed"}
		}
		if err := os.Remove(execution.file.Name() + ".admission"); err != nil && !os.IsNotExist(err) {
			h.failJournalLocked()
			return WorkerReply{Error: "retire worker admission failed"}
		}
		_ = execution.file.Close()
		h.bytes -= execution.bytes + execution.admissionBytes
		delete(h.executions, request.Sequence)
		return WorkerReply{}
	case "interrupt":
		return h.interrupt(request.Key)
	case "session", "reset":
		if request.Key == "" || len(request.Key) > 4096 {
			return WorkerReply{Error: "invalid worker session key"}
		}
		h.mu.Lock()
		retained := false
		for _, execution := range h.executions {
			retained = retained || execution.Key == request.Key
		}
		h.mu.Unlock()
		if request.Action == "reset" {
			if retained {
				return WorkerReply{Error: "consume and retire worker executions before resetting their session"}
			}
			if err := h.supervisor.ResetSession(request.Key); err != nil {
				return WorkerReply{Error: err.Error()}
			}
		}
		return WorkerReply{Session: &WorkerSession{
			ThreadID: h.supervisor.ThreadID(request.Key), Active: h.supervisor.IsActive(request.Key),
			Admission: h.supervisor.ConversationAdmission(request.Key), Retained: retained,
		}}
	case "shutdown":
		h.stopOnce.Do(func() { close(h.stop) })
		return WorkerReply{}
	default:
		return WorkerReply{Error: "unsupported worker action"}
	}
}

func (h *workerHost) send(request WorkerRequest) WorkerReply {
	data, _ := json.Marshal(struct {
		Key, Prompt, Message string
		Workflow             *WorkerWorkflow
		Agent                *WorkerAgent
		Conversation         *WorkerConversation
	}{request.Key, request.Prompt, request.Message, request.Workflow, request.Agent, request.Conversation})
	digest := sha256.Sum256(data)
	h.mu.Lock()
	if execution := h.executions[request.Sequence]; execution != nil {
		if execution.digest != digest {
			h.mu.Unlock()
			return WorkerReply{Error: "worker admission payload changed"}
		}
		reply := workerSendReply(execution.WorkerExecution)
		h.mu.Unlock()
		return reply
	}
	if request.Sequence != h.next || request.Key == "" || request.Prompt == "" || len(request.Key) > 4096 {
		h.mu.Unlock()
		return WorkerReply{Error: "invalid or retired worker admission"}
	}
	if err := h.validateConversationLocked(request); err != nil {
		h.mu.Unlock()
		return WorkerReply{Error: err.Error()}
	}
	if agent := request.Agent; agent != nil {
		if request.Workflow != nil || len(agent.ID) < 8 || len(agent.ID) > 128 ||
			(agent.Kind != "heartbeat" && agent.Kind != "notification") ||
			agent.StartedAt.IsZero() || !agent.Deadline.After(time.Now()) ||
			agent.StartedAt.After(time.Now().Add(time.Minute)) ||
			!agent.Deadline.After(agent.StartedAt) || agent.Deadline.Sub(agent.StartedAt) > 24*time.Hour ||
			(agent.Kind == "heartbeat" && request.Key != "orchestrator:semantic-heartbeat") ||
			(agent.Kind == "notification" && !strings.HasPrefix(request.Key, "orchestrator:notification:")) {
			h.mu.Unlock()
			return WorkerReply{Error: "invalid worker agent admission"}
		}
	}
	for _, retained := range h.executions {
		if (retained.Agent != nil || request.Agent != nil) && retained.Key == request.Key ||
			retained.Agent != nil && request.Agent != nil && retained.Agent.ID == request.Agent.ID {
			h.mu.Unlock()
			return WorkerReply{Error: "agent admission already retained; inspect and attach before any new dispatch"}
		}
		if retained.Workflow != nil && (retained.Key == request.Key || request.Workflow != nil &&
			retained.Workflow.LeaseID == request.Workflow.LeaseID && retained.Workflow.ClaimAttempt == request.Workflow.ClaimAttempt) {
			h.mu.Unlock()
			return WorkerReply{Error: "workflow admission already retained; inspect and attach before any new dispatch"}
		}
	}
	if len(h.executions) >= workerMaxExecutions {
		h.mu.Unlock()
		return WorkerReply{Error: "worker execution retention is full; consume completed executions"}
	}
	file, err := os.OpenFile(filepath.Join(h.directory, strconv.FormatUint(request.Sequence, 10)+".jsonl"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		h.mu.Unlock()
		return WorkerReply{Error: "create worker journal failed"}
	}
	execution := &workerExecution{WorkerExecution: WorkerExecution{Sequence: request.Sequence, Key: request.Key, Workflow: request.Workflow, Agent: request.Agent, Conversation: request.Conversation}, file: file, digest: digest}
	h.executions[request.Sequence] = execution
	h.next++
	if err := file.Sync(); err != nil || h.saveAdmissionLocked(execution) != nil {
		h.failJournalLocked()
		h.mu.Unlock()
		return WorkerReply{Error: "persist worker admission failed"}
	}
	h.mu.Unlock()
	if request.Conversation != nil {
		if h.conversation == nil {
			err = errors.New("worker conversation reservation unavailable")
		} else {
			receipt := h.info.ID + ":" + strconv.FormatUint(request.Sequence, 10) + ":admission"
			err = h.conversation(h.supervisor.HarnessConfig().Cwd, request.Key, *request.Conversation, receipt, nil)
		}
		h.mu.Lock()
		execution.SourcesReserved = err == nil
		if err != nil {
			execution.Error = err.Error()
			h.mu.Unlock()
			h.capture(execution, core.Event{Kind: core.EventStatus, Text: "Conversation source reservation refused; provider was not dispatched", Done: true})
			h.mu.Lock()
			if h.failure != nil {
				h.mu.Unlock()
				return WorkerReply{Error: "persist worker source refusal failed"}
			}
		}
		if saveErr := h.saveAdmissionLocked(execution); saveErr != nil {
			h.failJournalLocked()
			h.mu.Unlock()
			return WorkerReply{Error: "persist worker source reservation failed"}
		}
		if err != nil {
			reply := WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
			h.mu.Unlock()
			return reply
		}
		h.mu.Unlock()
	}
	if request.Workflow != nil {
		var reservation WorkerReservation
		if h.workflow == nil {
			err = errors.New("worker workflow reservation unavailable")
		} else {
			reservation, err = h.workflow(h.supervisor.HarnessConfig().Cwd, request.Key, *request.Workflow, "reserve")
		}
		h.mu.Lock()
		if err == nil {
			execution.Reservation = &reservation
		} else {
			execution.Error, execution.Done = err.Error(), true
		}
		if saveErr := h.saveAdmissionLocked(execution); saveErr != nil {
			h.failJournalLocked()
			h.mu.Unlock()
			return WorkerReply{Error: "persist worker reservation failed"}
		}
		if err != nil {
			reply := WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
			h.mu.Unlock()
			return reply
		}
		h.mu.Unlock()
	}
	// The worker lifetime, not a controller request, owns every admitted turn.
	turnCtx := h.supervisor.ctx
	if request.Agent != nil {
		turnCtx, execution.cancel = context.WithDeadline(turnCtx, request.Agent.Deadline)
	}
	var reserveTurn func() bool
	if request.Conversation != nil {
		reserveTurn = func() bool { return h.reserveConversationTurn(execution) }
	}
	threadID, steered, err := h.supervisor.sendReserved(turnCtx, request.Key, request.Prompt, request.Message, func(event core.Event) {
		if request.Conversation != nil {
			h.captureSource(execution, event, request.Conversation.SourceMessageID)
		} else {
			h.capture(execution, event)
		}
	}, false, reserveTurn)
	var rejected *SendNotAdmittedError
	if errors.As(err, &rejected) {
		if request.Conversation != nil && !request.Conversation.Recovery {
			receipt := h.info.ID + ":" + strconv.FormatUint(request.Sequence, 10) + ":admission"
			if restoreErr := h.conversation(h.supervisor.HarnessConfig().Cwd, request.Key, *request.Conversation, receipt, err); restoreErr != nil {
				h.mu.Lock()
				h.failJournalLocked()
				h.mu.Unlock()
				return WorkerReply{Error: "restore worker conversation admission failed"}
			}
		}
		h.capture(execution, core.Event{Kind: core.EventError, Text: err.Error(), ThreadID: threadID, Done: true})
	} else if err != nil {
		// A failed acknowledgement does not establish absence of provider effects.
		h.capture(execution, core.Event{Kind: core.EventStatus, Text: "Provider admission requires reconciliation", ThreadID: threadID,
			Execution: &core.ExecutionStatus{State: "degraded"}})
	}
	h.mu.Lock()
	if h.failure != nil {
		h.mu.Unlock()
		return WorkerReply{Error: "worker event journal unavailable"}
	}
	execution.ThreadID, execution.Steered = threadID, steered
	if err != nil && (!execution.Done || rejected != nil) {
		execution.Error = err.Error()
	}
	if err := h.saveAdmissionLocked(execution); err != nil {
		h.failJournalLocked()
		h.mu.Unlock()
		return WorkerReply{Error: "persist worker dispatch result failed"}
	}
	reply := workerSendReply(execution.WorkerExecution)
	h.mu.Unlock()
	return reply
}

// Exact Send retries preserve uncertainty without another provider admission.
func workerSendReply(execution WorkerExecution) WorkerReply {
	reply := WorkerReply{Executions: []WorkerExecution{execution}}
	if !execution.Done && execution.Error != "" {
		reply.Error = "worker provider admission unconfirmed; execution retained"
	}
	return reply
}

func (h *workerHost) expireAgents() {
	h.operations.Lock()
	defer h.operations.Unlock()
	h.mu.Lock()
	var expired []*workerExecution
	for _, execution := range h.executions {
		if execution.Agent != nil && !execution.Done && !execution.deadlineInterrupted && !time.Now().Before(execution.Agent.Deadline) {
			execution.deadlineInterrupted = true
			execution.Error = context.DeadlineExceeded.Error()
			if h.saveAdmissionLocked(execution) != nil {
				h.failJournalLocked()
				break
			}
			expired = append(expired, execution)
		}
	}
	h.mu.Unlock()
	for _, execution := range expired {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := h.supervisor.Interrupt(ctx, execution.Key)
		cancel()
		detail := "Background agent deadline exceeded; provider release pending"
		if err != nil {
			detail = "Background agent deadline exceeded; interruption failed"
		}
		event := core.Event{Kind: core.EventStatus, Text: detail, Execution: &core.ExecutionStatus{State: "degraded", Detail: detail}}
		if !h.supervisor.IsActive(execution.Key) {
			event.Kind, event.Done, event.Execution.State = core.EventError, true, "error"
		}
		h.capture(execution, event)
	}
}

func (h *workerHost) saveAdmissionLocked(execution *workerExecution) error {
	data, err := json.Marshal(struct {
		Execution WorkerExecution
		Digest    [32]byte
	}{execution.WorkerExecution, execution.digest})
	if err != nil {
		return err
	}
	size := int64(len(data))
	if size > workerRequestBytes || h.bytes-execution.admissionBytes+size > workerJournalBytes {
		return errors.New("worker admission capacity exceeded")
	}
	if err := fsx.AtomicWriteFile(execution.file.Name()+".admission", data, 0600); err != nil {
		return err
	}
	h.bytes += size - execution.admissionBytes
	execution.admissionBytes = size
	return nil
}

func (h *workerHost) capture(execution *workerExecution, event core.Event) {
	h.captureSource(execution, event, "")
}

func (h *workerHost) captureSource(execution *workerExecution, event core.Event, source string) {
	active := false
	if event.Done && !event.Continues && h.supervisor != nil {
		h.mu.Lock()
		conversation := execution.Conversation != nil
		h.mu.Unlock()
		active = conversation && h.supervisor.IsActive(execution.Key)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure != nil || execution.Done {
		return
	}
	requestOnly := execution.Conversation != nil && execution.SourcesReserved && active
	if execution.conversationSending {
		pending := workerPendingEvent{Event: event, RequestOnly: requestOnly, Source: source}
		data, err := json.Marshal(pending)
		if err != nil || len(data) > workerEventBytes || h.bytes+int64(len(data)) > workerJournalBytes || len(execution.offsets)+len(execution.pendingEvents) >= workerMaxEvents {
			h.failJournalLocked()
			return
		}
		execution.pendingEvents = append(execution.pendingEvents, pending)
		execution.pendingBytes += int64(len(data))
		h.bytes += int64(len(data))
		return
	}
	h.captureLocked(execution, event, requestOnly, source)
}

func (h *workerHost) captureLocked(execution *workerExecution, event core.Event, requestOnly bool, source string) {
	if h.failure != nil || execution.Done {
		return
	}
	conversation, streamed := h.captureConversationLocked(execution, event, requestOnly)
	if conversation != nil && source != "" {
		conversation.SourceMessageID = source
	}
	data, err := json.Marshal(WorkerEvent{Cursor: uint64(len(execution.offsets)) + 1, Event: event, Conversation: conversation, RequestOnly: requestOnly, Streamed: streamed, ConversationTurns: execution.ConversationTurns})
	if err == nil && (len(data) > workerEventBytes || h.bytes+int64(len(data)+1) > workerJournalBytes || len(execution.offsets) >= workerMaxEvents) {
		err = errors.New("worker event journal capacity exceeded")
	}
	if err == nil {
		data = append(data, '\n')
		var written int
		written, err = execution.file.Write(data)
		if err == nil && written != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = execution.file.Sync()
		}
	}
	if err != nil {
		// Unrecorded events cannot become successful delivery or a new turn.
		h.failJournalLocked()
		return
	}
	execution.offsets = append(execution.offsets, execution.bytes)
	execution.LastCursor = uint64(len(execution.offsets))
	execution.bytes += int64(len(data))
	h.bytes += int64(len(data))
	if event.Done && !event.Continues && !requestOnly {
		execution.Done = true
		// The provider's actual terminal resolves a previously uncertain Send.
		if !execution.deadlineInterrupted && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			execution.Error = ""
		}
		if execution.Agent != nil && !time.Now().Before(execution.Agent.Deadline) {
			execution.Error = context.DeadlineExceeded.Error()
		}
		if execution.cancel != nil {
			execution.cancel()
		}
	}
}

// Commit only after downstream effects are durable and deduplicated by worker
// ID, sequence and cursor. Reading a page alone never settles those effects.
func (h *workerHost) commit(request WorkerRequest) WorkerReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	execution := h.executions[request.Sequence]
	if execution == nil || request.Expected > request.After || request.After > execution.LastCursor {
		return WorkerReply{Error: "unknown worker execution or commit cursor"}
	}
	if request.After == execution.CommittedCursor {
		return WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
	}
	if request.Expected != execution.CommittedCursor || request.After < execution.CommittedCursor {
		return WorkerReply{Error: "worker committed cursor changed"}
	}
	if err := fsx.AtomicWriteFile(execution.file.Name()+".cursor", []byte(strconv.FormatUint(request.After, 10)+"\n"), 0600); err != nil {
		h.failJournalLocked()
		return WorkerReply{Error: "commit worker cursor failed"}
	}
	execution.CommittedCursor = request.After
	return WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
}

func (h *workerHost) events(request WorkerRequest) WorkerReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	execution := h.executions[request.Sequence]
	if execution == nil || request.After > uint64(len(execution.offsets)) {
		return WorkerReply{Error: "unknown worker execution or cursor"}
	}
	reply := WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
	var bytes int64
	for index := int(request.After); index < len(execution.offsets) && len(reply.Events) < workerPageEvents; index++ {
		start, end := execution.offsets[index], execution.bytes
		if index+1 < len(execution.offsets) {
			end = execution.offsets[index+1]
		}
		if len(reply.Events) > 0 && bytes+end-start > workerPageBytes {
			break
		}
		data := make([]byte, end-start)
		if _, err := execution.file.ReadAt(data, start); err != nil {
			h.failJournalLocked()
			return WorkerReply{Error: "read worker journal failed"}
		}
		var event WorkerEvent
		if err := json.Unmarshal(data, &event); err != nil || event.Cursor != uint64(index+1) {
			h.failJournalLocked()
			return WorkerReply{Error: "invalid worker journal record"}
		}
		reply.Events = append(reply.Events, event)
		bytes += end - start
	}
	return reply
}

func (h *workerHost) failJournalLocked() {
	h.failure = errors.New("worker event journal unavailable")
	h.stopOnce.Do(func() { close(h.stop) })
}
